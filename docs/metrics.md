# Metrics & Observability

This guide outlines the key Prometheus metrics available for monitoring, debugging, and demonstrating the **`liqo-dynamic-offloader`** operator.

> **Note on Tooling:** The metrics and PromQL queries in this document were analyzed and verified using **[k8s-metrics-explorer](https://github.com/SamVia/k8s-metrics-explorer)**, a standalone utility hosted on GitHub for scraping and exploring Kubernetes operator metrics.

## 1. Core Operator Metrics (The Showcase)

These custom metrics are emitted directly by the operator and are perfect for demonstrating the dynamic offloading lifecycle in real-time.

| Metric | Type | What it tracks & Why it matters |
| --- | --- | --- |
| `liqo_cleanup_namespaces_total` | Counter | Total offloading policies successfully revoked. A rising count during a demo visually proves that isolated namespaces are being automatically restored. |
| `liqo_cleanup_active_remote_pods` | Gauge | Current remote (or gracefully terminating) pods per namespace. Drops to `0` exactly when the cleanup controller tears down the policy. |

## 2. Controller & Event Queue Health

Standard `controller-runtime` metrics that validate the operator's speed, reliability, and ability to handle high pod churn.

| Metric | Type | What it tracks & Why it matters |
| --- | --- | --- |
| `controller_runtime_reconcile_errors_total` | Counter | Failed reconciliation attempts. A flat line at zero validates operator stability and correct RBAC permissions. |
| `controller_runtime_reconcile_time_seconds` | Histogram | Reconciliation latency. Proves the operator can detect a trapped pod and inject the offloading policy in milliseconds. |
| `workqueue_depth` | Gauge | Backlog of pod events. A baseline near zero proves the Trap Controller is instantly consuming pods entering `OffloadingBackOff`. |
| `workqueue_unfinished_work_seconds` | Gauge | Time spent processing active items. Crucial for verifying that the configured `TRAP_BACKOFF` intervals are not stalling the queue. |

## 3. Cross-Cluster Infrastructure (Liqo)

Because the operator relies on Liqo's underlying fabric, exposing these infrastructure metrics proves the tunnel is healthy and workloads are successfully offloaded.

| Metric | Type | What it tracks & Why it matters |
| --- | --- | --- |
| `liqo_peer_is_connected` | Gauge | Binary indicator (`1`=Up) of the WireGuard tunnel between local and remote clusters. |
| `liqo_peer_latency_us` | Gauge | Inter-cluster microsecond latency. Helps correlate any pod scheduling delays with network latency. |
| `liqo_peer_transmit_bytes_total` | Counter | Gateway throughput. Visually demonstrates active network traffic once the operator unsticks the pods and they boot remotely. |
| `controller_runtime_webhook_latency_seconds` | Histogram | Liqo webhook latency. Tracks the delay introduced when the operator dynamically creates the `NamespaceOffloading` resource. |

## 4. Operator Resource Efficiency

Baseline metrics to verify the operator remains lightweight in production environments.

| Metric | Type | What it tracks & Why it matters |
| --- | --- | --- |
| `process_resident_memory_bytes` | Gauge | Physical memory footprint (RSS). Confirms there are no memory leaks over extended periods of operation. |
| `process_cpu_seconds_total` | Counter | Cumulative CPU time. Demonstrates the operator's minimal overhead while continuously watching cluster-wide pod events. |
| `go_goroutines` | Gauge | Active Go routines. Ensures the independent Trap and Cleanup reconcilers terminate cleanly without leaking concurrency. |

---

## Grafana Dashboard Snippets

Use these PromQL queries to quickly build a showcase dashboard for the operator:

**Total Namespaces Successfully Remediated (Rate):**

```promql
sum(rate(liqo_cleanup_namespaces_total[1m]))

```

**Active Remote Pods Awaiting Cleanup:**

```promql
liqo_cleanup_active_remote_pods > 0

```

**Operator Error Rate (by Controller):**

```promql
sum by (controller) (rate(controller_runtime_reconcile_errors_total[1m]))

```

**Cross-Cluster Network Throughput (Bytes/sec):**

```promql
sum(rate(liqo_peer_transmit_bytes_total[1m]) + rate(liqo_peer_receive_bytes_total[1m]))

```