# Complete In-Cluster Operator Demo

This is the end-to-end version driven by [`scripts/3-demo-complete.sh`](../scripts/3-demo-complete.sh). It builds the Go operator into a container image, loads the image into the local Kind cluster, deploys the operator with its RBAC configuration, and runs two trap workloads:

- `default`, which is eligible for auto-healing;
- `demo-not-selected`, which is explicitly excluded and must not receive a `NamespaceOffloading` policy.

The script then streams the in-cluster operator logs until you press `Ctrl+C`.

## Prerequisites

- Docker
- [kind](https://kind.sigs.k8s.io/)
- `kubectl`
- `liqoctl` 1.0 or later
- GNU Make
- Bash, Git Bash, or WSL

First prepare the Liqo clusters:

```bash
chmod +x scripts/0-reset.sh scripts/1-setup.sh scripts/3-demo-complete.sh
./scripts/1-setup.sh
```

## Build the operator image

From the repository root, build the image expected by the complete demo:

```bash
make docker-build
```

This creates `liqo-dynamic-offloader:latest` using the repository's
`Dockerfile`. The complete demo loads that image into the `cluster-local` Kind
cluster, so a registry is not required.

## Run the complete demo

```bash
./scripts/3-demo-complete.sh
```

The script performs these stages:

1. Loads `liqo-dynamic-offloader:latest` into `cluster-local`.
2. Applies `config/rbac/role.yaml` and deploys the operator in `default`.
3. Waits for the operator Deployment to become available.
4. Creates the normal trap in `default` and a second trap in
   `demo-not-selected`.
5. Verifies that `demo-not-selected` does not receive an offloading policy.
6. Streams logs from `liqo-auto-healing-operator`.

The default target virtual node is `cluster-remote`. The script also accepts
these environment variables:

| Variable | Purpose | Default |
| --- | --- | --- |
| `TARGET_CLUSTER_ID` | Comma-separated Liqo target cluster IDs | all available remote clusters |
| `DEMO_REMOTE_CLUSTER` | Node name used by the generated trap workloads | `cluster-remote` |
| `DEMO_EXCLUDED_NAMESPACE` | Namespace used to verify exclusion | `demo-not-selected` |
| `EXCLUDED_NAMESPACES` | Comma-separated namespaces ignored by the operator | system namespaces plus the demo exclusion |

For example:

```bash
TARGET_CLUSTER_ID=cluster-remote \
DEMO_REMOTE_CLUSTER=cluster-remote \
./scripts/3-demo-complete.sh
```

While the logs stream, useful inspection commands in another terminal are:

```bash
kubectl get pods -A
kubectl get namespaceoffloading -A
kubectl get events -A --sort-by=.lastTimestamp
```

Press `Ctrl+C` to stop the script. Its signal handler removes the demo
Deployments, the temporary namespace, the operator Deployment, and the
`NamespaceOffloading` policy.

To clean up explicitly after an interrupted run:

```bash
./scripts/0-reset.sh
```

See [`demo_base.md`](demo_base.md) for the isolation proof without the
operator and [`demo_advanced.md`](demo_advanced.md) for the local Go process
version.
