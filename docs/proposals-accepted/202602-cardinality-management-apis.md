---
type: proposal
title: Global Cardinality Exploration API
status: accepted
owner: ntk148v
menu: proposals-accepted
---

## 1 Related links/tickets

- <https://github.com/thanos-io/thanos/issues/6007>
- Similar feature in VictoriaMetrics: [Cardinality Explorer](https://docs.victoriametrics.com/victoriametrics/#cardinality-explorer)

## 2 Why

High cardinality is a frequent source of instability and performance degradation in Prometheus and Thanos ecosystems. Operators often need to identify which metrics or label names are consuming the most resources (series count, label value count, etc.) to optimize ingestion and storage.

Currently, Thanos lacks a unified way to inspect cardinality across the entire cluster.

- **Thanos Receiver** exposes a local `/api/v1/status/tsdb` endpoint, but this is specific to the Receiver component.
- **Thanos Sidecar** and **Store Gateway** do not standardize this information.
- **Thanos Query** cannot currently aggregate this information, forcing operators to inspect individual components manually, which is impractical in large-scale deployments.

By implementing a standardized Cardinality API, we can provide a "single pane of glass" in Thanos Query to explore cardinality across all connected stores (Sidecars, Receivers, Rulers, Store Gateways).

## 3 Pitfalls of current solutions

- **Fragmented Visibility**: Operators must check individual Prometheus/Sidecar or Receiver instances to find high-cardinality sources. In a cluster with hundreds of sidecars, this is impossible.
- **Inconsistent API**: The `TSDBStats` interface currently exists only in the `receive` package and is not part of the standard `StoreAPI`, preventing other components from exposing it in a standard way.
- **No Global Aggregation**: There is no mechanism to merge "Top 10" lists from multiple sources to see the "Global Top 10" high-cardinality series.

## 4 Audience

- **Platform Engineers**: To debug high memory usage or ingestion delays caused by cardinality explosions.
- **Thanos Users**: To understand their metric usage and optimize their instrumentation.

## 5 Goals

- Standardize `TSDBStats` as a first-class citizen in the Thanos `StoreAPI` (gRPC).
- Implement `TSDBStats` support in **Thanos Query** to fan-out requests and aggregate results.
- Expose a unified HTTP API on Thanos Query compatible with Prometheus's `/api/v1/status/tsdb` but with distributed capabilities.

## 6 Non-Goals

- Implementing the UI for Cardinality Explorer (this will be a follow-up task once the API is in place).
- Implementing deep/expensive cardinality analysis for Object Storage (Store Gateway) in the first iteration (basic stats or stubs are acceptable initially).

## 7 Proposal

We propose extending the `Store` gRPC service to include a `TSDBStats` method. Thanos Query will leverage its existing `ProxyStore` architecture to broadcast this request to all healthy stores, aggregate the partial results (merging counters and top-N lists), and return a global view.

### 7.1 Proto Definitions (`pkg/store/storepb/rpc.proto`)

We will add a new RPC method `TSDBStats` to the `Store` service.

```protobuf
service Store {
  // ... existing RPCs ...

  // TSDBStats returns statistics about the underlying TSDB, including cardinality information.
  rpc TSDBStats(TSDBStatsRequest) returns (TSDBStatsResponse);
}

message TSDBStatsRequest {
    // limit is the number of top items to return for cardinality lists (e.g., top 10).
    int64 limit = 1;

    // stats_by_label_name requests stats for a specific label name.
    // If empty, global stats are returned.
    string stats_by_label_name = 2;

    // matchers restricts the stats to a subset of series/tenants (optional).
    // This is primarily for multi-tenant components (Receiver) or filtering by external labels in Proxy.
    repeated LabelMatcher matchers = 3;
}

message TSDBStatsResponse {
    // Global stats
    int64 num_series = 1;
    int64 num_label_pairs = 2;
    int64 num_chunks = 3;

    // Cardinality stats (Top N)
    repeated LabelCardinalityInfo label_names_with_highest_cardinality = 10;
    repeated LabelCardinalityInfo label_values_with_highest_cardinality = 11;
    // For specific label value stats (when stats_by_label_name is set)
    repeated LabelCardinalityInfo label_value_pairs_with_highest_cardinality = 12;
}

message LabelCardinalityInfo {
    string name = 1;
    uint64 count = 2;
    string value = 3; // Populated for value-specific stats
}
```

### 7.2 Query Component Implementation

The **Thanos Query** component (specifically `ProxyStore` in `pkg/store/proxy.go`) will implement the aggregation logic:

1. **Fan-out**: When `TSDBStats` is called on the Proxy, it forwards the request to all active Store Clients.
    - It filters stores based on `external_labels` matches (if `matchers` are provided in the request).
2. **Aggregation**:
    - **Sum** scalar values: `num_series`, `num_chunks`, `num_label_pairs` are summed across all responses.
    - **Merge** Top-N lists: For `label_names_with_highest_cardinality`, the proxy collects all lists, sorts them by count (descending), and keeps the top `N` items.
    - _Note_: This provides an approximate "Global Top N" because a label might be #11 in one store (and thus not returned) but #1 in another. However, for identifying cardinality explosions, this approximation is usually sufficient and standard in distributed systems.

### 7.3 HTTP API

Thanos Query will expose:
`GET /api/v1/status/tsdb`

Parameters:

- `limit`: Integer (default 10).
- `match[]`: Repeated label matchers to select specific stores (e.g., `match[]=cluster="eu-west-1"`).

This API will be compatible with the Prometheus TSDB Stats API structure, allowing existing tooling to potentially work with it, while adding the power of distributed querying.

### 7.4 Store Implementations

- **Sidecar**: Proxies the request to the underlying Prometheus `/api/v1/status/tsdb`.
- **Receiver**: Uses its existing internal `TSDBStats` logic but exposes it via the new gRPC method.
- **Ruler**: Uses the local TSDB reference to calculate stats.

## 8 Alternatives

### 8.1 Scrape Metrics

We could rely on `prometheus_tsdb_...` metrics.

- _Rejection_: Metrics only provide counters (e.g., total series). They cannot provide "Top 10 Label Names" or "Top 10 Values for Label X" which are dynamic and high-cardinality themselves.

### 8.2 Direct Querying

Operators query each Sidecar/Receiver individually.

- _Rejection_: Does not scale. The goal of Thanos is to provide a unified view.

## 9 Action Plan

1. **Define API**: Update `pkg/store/storepb/rpc.proto` and regenerate code.
2. **Implement Sidecar**: Add `TSDBStats` support to Sidecar (proxy to Prometheus).
3. **Implement Receiver**: Adapt Receiver to implement the new gRPC method using its existing logic.
4. **Implement Query**: Add `TSDBStats` to `ProxyStore` with aggregation logic.
5. **Expose HTTP**: Add the endpoint to Thanos Query's HTTP API.
