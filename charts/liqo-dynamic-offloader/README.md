# Liqo Dynamic Offloader Helm chart

This chart deploys the Liqo Dynamic Offloader controller manager with
cluster-scoped RBAC and configurable controller flags.

## Metrics

Metrics are disabled by default. Set `metrics.enabled=true` to deploy the
`kube-rbac-proxy` sidecar, its HTTPS Service, the delegated authorization RBAC
binding, and the Prometheus Operator `ServiceMonitor`. The Prometheus
Operator CRDs must already be installed in the cluster.

## Webhook support

Webhook support is reserved for a future release and is disabled by default.
The current controller does not expose a webhook endpoint, Service, or
certificate integration. Keep `webhook.enabled` set to `false` until those
components are implemented and tested together.
