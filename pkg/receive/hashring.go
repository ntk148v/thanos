// Copyright (c) The Thanos Authors.
// Licensed under the Apache License 2.0.

package receive

import (
	"fmt"
	"math"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/cespare/xxhash/v2"
	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/pkg/errors"

	"github.com/thanos-io/thanos/pkg/store/labelpb"
	"github.com/thanos-io/thanos/pkg/store/storepb/prompb"
)

// HashringAlgorithm is the algorithm used to distribute series in the ring.
type HashringAlgorithm string

const (
	AlgorithmHashmod HashringAlgorithm = "hashmod"
	AlgorithmKetama  HashringAlgorithm = "ketama"

	// SectionsPerNode is the number of sections in the ring assigned to each node
	// in the ketama hashring. A higher number yields a better series distribution,
	// but also comes with a higher memory cost.
	SectionsPerNode = 1000
)

// insufficientNodesError is returned when a hashring does not
// have enough nodes to satisfy a request for a node.
type insufficientNodesError struct {
	have uint64
	want uint64
}

// Error implements the error interface.
func (i *insufficientNodesError) Error() string {
	return fmt.Sprintf("insufficient nodes; have %d, want %d", i.have, i.want)
}

// Hashring finds the correct node to handle a given time series
// for a specified tenant.
// It returns the node and any error encountered.
type Hashring interface {
	// GetN returns the nth node that should handle the given tenant and time series.
	GetN(tenant string, timeSeries *prompb.TimeSeries, n uint64) (Endpoint, error)
	// Nodes returns a sorted slice of nodes that are in this hashring. Addresses could be duplicated
	// if, for example, the same address is used for multiple tenants in the multi-hashring.
	Nodes() []Endpoint
}

// SingleNodeHashring always returns the same node.
type SingleNodeHashring string

func (s SingleNodeHashring) Nodes() []Endpoint {
	return []Endpoint{{Address: string(s), CapNProtoAddress: string(s)}}
}

// GetN implements the Hashring interface.
func (s SingleNodeHashring) GetN(_ string, _ *prompb.TimeSeries, n uint64) (Endpoint, error) {
	if n > 0 {
		return Endpoint{}, &insufficientNodesError{have: 1, want: n + 1}
	}
	return Endpoint{
		Address:          string(s),
		CapNProtoAddress: string(s),
	}, nil
}

// simpleHashring represents a group of nodes handling write requests by hashmoding individual series.
type simpleHashring []Endpoint

func newSimpleHashring(endpoints []Endpoint) (Hashring, error) {
	for i := range endpoints {
		if endpoints[i].AZ != "" {
			return nil, errors.New("Hashmod algorithm does not support AZ aware hashring configuration. Either use Ketama or remove AZ configuration.")
		}
	}
	slices.SortFunc(endpoints, func(a, b Endpoint) int {
		return strings.Compare(a.Address, b.Address)
	})

	return simpleHashring(endpoints), nil
}

func (s simpleHashring) Nodes() []Endpoint {
	return s
}

// Get returns a target to handle the given tenant and time series.
func (s simpleHashring) Get(tenant string, ts *prompb.TimeSeries) (Endpoint, error) {
	return s.GetN(tenant, ts, 0)
}

// GetN returns the nth target to handle the given tenant and time series.
func (s simpleHashring) GetN(tenant string, ts *prompb.TimeSeries, n uint64) (Endpoint, error) {
	if n >= uint64(len(s)) {
		return Endpoint{}, &insufficientNodesError{have: uint64(len(s)), want: n + 1}
	}

	return s[(labelpb.HashWithPrefix(tenant, ts.Labels)+n)%uint64(len(s))], nil
}

type section struct {
	az            string
	endpointIndex uint64
	hash          uint64
	replicas      []uint64
}

type sections []*section

func (p sections) Len() int           { return len(p) }
func (p sections) Less(i, j int) bool { return p[i].hash < p[j].hash }
func (p sections) Swap(i, j int)      { p[i], p[j] = p[j], p[i] }
func (p sections) Sort()              { sort.Sort(p) }

// ketamaHashring represents a group of nodes handling write requests with consistent hashing.
type ketamaHashring struct {
	endpoints    []Endpoint
	sections     sections
	numEndpoints uint64
}

func newKetamaHashring(endpoints []Endpoint, sectionsPerNode int, replicationFactor uint64) (*ketamaHashring, error) {
	numSections := len(endpoints) * sectionsPerNode

	if len(endpoints) < int(replicationFactor) {
		return nil, errors.New("ketama: amount of endpoints needs to be larger than replication factor")

	}
	hash := xxhash.New()
	availabilityZones := make(map[string]struct{})
	ringSections := make(sections, 0, numSections)

	for endpointIndex, endpoint := range endpoints {
		availabilityZones[endpoint.AZ] = struct{}{}
		for i := 1; i <= sectionsPerNode; i++ {
			_, _ = hash.Write([]byte(endpoint.Address + ":" + strconv.Itoa(i)))
			n := &section{
				az:            endpoint.AZ,
				endpointIndex: uint64(endpointIndex),
				hash:          hash.Sum64(),
				replicas:      make([]uint64, 0, replicationFactor),
			}

			ringSections = append(ringSections, n)
			hash.Reset()
		}
	}
	sort.Sort(ringSections)
	calculateSectionReplicas(ringSections, replicationFactor, availabilityZones)

	return &ketamaHashring{
		endpoints:    endpoints,
		sections:     ringSections,
		numEndpoints: uint64(len(endpoints)),
	}, nil
}

func (k *ketamaHashring) Nodes() []Endpoint {
	return k.endpoints
}

func sizeOfLeastOccupiedAZ(azSpread map[string]int64) int64 {
	minValue := int64(math.MaxInt64)
	for _, value := range azSpread {
		if value < minValue {
			minValue = value
		}
	}
	return minValue
}

// calculateSectionReplicas pre-calculates replicas for each section,
// ensuring that replicas for each ring section are owned by different endpoints.
func calculateSectionReplicas(ringSections sections, replicationFactor uint64, availabilityZones map[string]struct{}) {
	for i, s := range ringSections {
		replicas := make(map[uint64]struct{})
		azSpread := make(map[string]int64)
		for az := range availabilityZones {
			// This is to make sure each az is initially represented
			azSpread[az] = 0
		}
		j := i - 1
		for uint64(len(replicas)) < replicationFactor {
			j = (j + 1) % len(ringSections)
			rep := ringSections[j]
			if _, ok := replicas[rep.endpointIndex]; ok {
				continue
			}
			if len(azSpread) > 1 && azSpread[rep.az] > 0 && azSpread[rep.az] > sizeOfLeastOccupiedAZ(azSpread) {
				// We want to ensure even AZ spread before we add more replicas within the same AZ
				continue
			}
			replicas[rep.endpointIndex] = struct{}{}
			azSpread[rep.az]++
			s.replicas = append(s.replicas, rep.endpointIndex)
		}
	}
}

func (c ketamaHashring) Get(tenant string, ts *prompb.TimeSeries) (Endpoint, error) {
	return c.GetN(tenant, ts, 0)
}

func (c ketamaHashring) GetN(tenant string, ts *prompb.TimeSeries, n uint64) (Endpoint, error) {
	if n >= c.numEndpoints {
		return Endpoint{}, &insufficientNodesError{have: c.numEndpoints, want: n + 1}
	}

	v := labelpb.HashWithPrefix(tenant, ts.Labels)

	var i uint64
	i = uint64(sort.Search(len(c.sections), func(i int) bool {
		return c.sections[i].hash >= v
	}))

	numSections := uint64(len(c.sections))
	if i == numSections {
		i = 0
	}

	endpointIndex := c.sections[i].replicas[n]
	return c.endpoints[endpointIndex], nil
}

type tenantSet map[string]tenantMatcher

func (t tenantSet) match(tenant string) (bool, error) {
	// Fast path for the common case of direct match.
	if mt, ok := t[tenant]; ok && isExactMatcher(mt) {
		return true, nil
	} else {
		for tenantPattern, matcherType := range t {
			switch matcherType {
			case TenantMatcherGlob:
				matches, err := filepath.Match(tenantPattern, tenant)
				if err != nil {
					return false, fmt.Errorf("error matching tenant pattern %s (tenant %s): %w", tenantPattern, tenant, err)
				}
				if matches {
					return true, nil
				}
			case TenantMatcherTypeExact:
				// Already checked above, skipping.
				fallthrough
			default:
				continue
			}

		}
	}
	return false, nil
}

// multiHashring represents a set of hashrings.
// Which hashring to use for a tenant is determined
// by the tenants field of the hashring configuration.
type multiHashring struct {
	cache      map[string]Hashring
	hashrings  []Hashring
	tenantSets []tenantSet

	// We need a mutex to guard concurrent access
	// to the cache map, as this is both written to
	// and read from.
	mu sync.RWMutex

	nodes []Endpoint
}

// Get returns a target to handle the given tenant and time series.
func (m *multiHashring) Get(tenant string, ts *prompb.TimeSeries) (Endpoint, error) {
	return m.GetN(tenant, ts, 0)
}

// GetN returns the nth target to handle the given tenant and time series.
func (m *multiHashring) GetN(tenant string, ts *prompb.TimeSeries, n uint64) (Endpoint, error) {
	m.mu.RLock()
	h, ok := m.cache[tenant]
	m.mu.RUnlock()
	if ok {
		return h.GetN(tenant, ts, n)
	}
	var found bool

	// If the tenant is not in the cache, then we need to check
	// every tenant in the configuration.
	for i, t := range m.tenantSets {
		// If the hashring has no tenants, then it is
		// considered a default hashring and matches everything.
		if t == nil {
			found = true
		} else {
			// Fast path for the common case of direct match.
			if mt, ok := t[tenant]; ok && isExactMatcher(mt) {
				found = true
			} else {
				var err error
				if found, err = t.match(tenant); err != nil {
					return Endpoint{}, err
				}
			}

		}
		if found {
			m.mu.Lock()
			m.cache[tenant] = m.hashrings[i]
			m.mu.Unlock()

			return m.hashrings[i].GetN(tenant, ts, n)
		}
	}
	return Endpoint{}, errors.New("no matching hashring to handle tenant")
}

func (m *multiHashring) Nodes() []Endpoint {
	return m.nodes
}

// newMultiHashring creates a multi-tenant hashring for a given slice of
// groups.
// Which hashring to use for a tenant is determined
// by the tenants field of the hashring configuration.
func NewMultiHashring(algorithm HashringAlgorithm, replicationFactor uint64, cfg []HashringConfig) (Hashring, error) {
	m := &multiHashring{
		cache: make(map[string]Hashring),
	}

	for _, h := range cfg {
		var hashring Hashring
		var err error
		activeAlgorithm := algorithm
		if h.Algorithm != "" {
			activeAlgorithm = h.Algorithm
		}
		hashring, err = newHashring(activeAlgorithm, h.Endpoints, replicationFactor, h.Hashring, h.Tenants)
		if err != nil {
			return nil, err
		}
		m.nodes = append(m.nodes, hashring.Nodes()...)
		m.hashrings = append(m.hashrings, hashring)
		var t map[string]tenantMatcher
		if len(h.Tenants) != 0 {
			t = make(map[string]tenantMatcher)
		}
		for _, tenant := range h.Tenants {
			t[tenant] = h.TenantMatcherType
		}
		m.tenantSets = append(m.tenantSets, t)
	}
	slices.SortFunc(m.nodes, func(a, b Endpoint) int {
		return strings.Compare(a.Address, b.Address)
	})
	return m, nil
}

func newHashring(algorithm HashringAlgorithm, endpoints []Endpoint, replicationFactor uint64, hashring string, tenants []string) (Hashring, error) {
	switch algorithm {
	case AlgorithmHashmod:
		return newSimpleHashring(endpoints)
	case AlgorithmKetama:
		return newKetamaHashring(endpoints, SectionsPerNode, replicationFactor)
	default:
		l := log.NewNopLogger()
		level.Warn(l).Log("msg", "Unrecognizable hashring algorithm. Fall back to hashmod algorithm.",
			"hashring", hashring,
			"tenants", tenants)
		return newSimpleHashring(endpoints)
	}
}
