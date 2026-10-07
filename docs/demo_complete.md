# Complete In-Cluster Operator Demo

This is the end-to-end Helm-based demo driven by
[`scripts/3-demo-complete.sh`](../scripts/3-demo-complete.sh). It installs the
published operator image from GHCR, configures the chart with the controller
flags, creates three test namespaces, and runs workloads against the Liqo
virtual node:

- `demo-allowed`, which is eligible for auto-healing;
- `demo-system`, excluded by the `*-system` namespace pattern;
- `demo-labeled`, excluded by the `ignore-trap=true` label.

The script then reports which namespaces received a `NamespaceOffloading`
policy and streams the Helm-managed operator logs until you press `Ctrl+C`.

## Prerequisites

- Helm 3
- [kind](https://kind.sigs.k8s.io/)
- `kubectl`
- `liqoctl` 1.0 or later
- Bash, Git Bash, or WSL

First prepare the Liqo clusters:

```bash
chmod +x scripts/0-reset.sh scripts/1-setup.sh scripts/3-demo-complete.sh
./scripts/1-setup.sh
```

## Run the complete demo

```bash
./scripts/3-demo-complete.sh
```

The script performs these stages:

1. Installs or upgrades the `liqo-dynamic-offloader` Helm release in `default`.
2. Pulls `ghcr.io/samvia/liqo-dynamic-offloader:latest` with
   `imagePullPolicy: Always`.
3. Waits for the operator Deployment to become available.
4. Creates the three test namespaces and their trap workloads.
5. Verifies the policy result for each filtering scenario.
6. Streams logs from the Helm-managed operator Deployment.

The default target virtual node is `cluster-remote`. The script also accepts
these environment variables:

| Variable | Purpose | Default |
| --- | --- | --- |
| `TARGET_CLUSTER_ID` | Comma-separated Liqo target cluster IDs | all available remote clusters |
| `DEMO_REMOTE_CLUSTER` | Node name used by the generated trap workloads | `cluster-remote` |
| `IMAGE_REPOSITORY` | Container image repository | `ghcr.io/samvia/liqo-dynamic-offloader` |
| `IMAGE_TAG` | Container image tag | `latest` |
| `IMAGE_PULL_POLICY` | Kubernetes image pull policy | `Always` |
| `HELM_RELEASE` | Helm release name | `liqo-dynamic-offloader` |
| `OPERATOR_NAMESPACE` | Namespace for the operator release | `default` |

For example:

```bash
TARGET_CLUSTER_ID=cluster-remote \
DEMO_REMOTE_CLUSTER=cluster-remote \
IMAGE_TAG=0.1.0 \
IMAGE_PULL_POLICY=IfNotPresent \
./scripts/3-demo-complete.sh
```

While the logs stream, useful inspection commands in another terminal are:

```bash
kubectl get pods -A
kubectl get namespaceoffloading -A
kubectl get events -A --sort-by=.lastTimestamp
```

Press `Ctrl+C` to stop the script. Its signal handler removes the demo
namespaces and uninstalls the Helm release. The image itself is not deleted
from the node cache.

To clean up explicitly after an interrupted run:

```bash
./scripts/0-reset.sh
```

See [`demo_base.md`](demo_base.md) for the isolation proof without the
operator and [`demo_advanced.md`](demo_advanced.md) for the legacy
step-by-step local development version.

## Local validation alternative: `4-locale.sh`

When an in-cluster image deployment is not required, use
[`scripts/4-locale.sh`](../scripts/4-locale.sh). This alternative starts the
operator with `go run ./main.go` on the host and stores the output in
`/tmp/liqo-operator-local.log`.

```bash
./scripts/4-locale.sh
```

It uses the same three namespace scenarios as the Helm demo, waits for policy
creation with dynamic polling, then removes the allowed workload and verifies
that the Cleanup Controller deletes the policy after the configured
15-second delay. Pass `--dry-run` to observe filtering without mutating
`NamespaceOffloading` resources:

```bash
./scripts/4-locale.sh --dry-run
```

Use `4-locale.sh` for fast source-level feedback. Use
`3-demo-complete.sh` when the objective is to validate the Helm chart, RBAC,
container image pull, and in-cluster leader-election behavior.
