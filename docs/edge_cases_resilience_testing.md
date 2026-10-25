# Edge Cases & Resilience Testing

This document outlines the testing strategies used to validate the robustness, concurrency handling, and state-reconciliation capabilities of the **Liqo Dynamic Offloader** operator.

We have provided three automated Bash scripts that demonstrate the operator's behavior under stressful or unexpected cluster conditions. The scripts run the operator locally against Kind clusters and automatically invoke the setup script to prepare the required clusters if they do not exist. Scripts 5 and 7 compile the operator with `go build`; script 6 runs it with `go run`. All three scripts include cleanup for local processes and test resources.

### Prerequisites

The scripts require Docker, Kind, `kubectl`, `liqoctl`, Go, and Bash, Git Bash, or WSL. Run them from the repository root. The setup script installs Liqo and peers the local cluster with the configured remote cluster(s):

```bash
./scripts/5-test-resilience.sh
./scripts/6-test-edge-case.sh
./scripts/7-test-concurrency.sh
```

---

## 1. Controller Resilience & State Reconciliation
**Script:** `5-test-resilience.sh`

### The Scenario
In a production environment, an operator might crash, be evicted, or undergo a rolling update. During its downtime, cluster state continues to evolve (e.g., new pods are scheduled and become trapped).

### What the Script Does
1. Stops matching local operator processes and scales matching in-cluster operator deployments to `0` replicas (simulating downtime).
2. Deploys a pod that requires offloading. The pod becomes stuck in `OffloadingBackOff`.
3. Asserts that no `NamespaceOffloading` policy is created during the downtime.
4. Starts the locally compiled operator binary with the configured target cluster.
5. Waits for the operator to acquire the leader-election lease and reconcile the existing pod.

### What it Proves
This test demonstrates that the operator can recover from downtime using Kubernetes' declarative state rather than relying only on events received while it was running. Upon startup, the controller discovers the trapped pod and generates the required offloading policy.

---

## 2. Ghost Pods (Early Deletion)
**Script:** `6-test-edge-case.sh`

### The Scenario
A pod is scheduled and enters the `OffloadingBackOff` state, triggering a reconciliation event. However, before the operator's requeue timer expires (or before the worker thread picks up the event), the user or a CI/CD pipeline deletes the pod.

### What the Script Does
1. Creates a trapped pod to trigger the controller.
2. Immediately deletes the deployment/pod before the controller can finish its backoff cycle or act on it.
3. Monitors the controller's logs for panics or crash loops.

### What it Proves
This test checks that the operator remains stable when a resource disappears during processing. The script specifically checks the local operator log for panics; this supports, but does not by itself prove, the expected `NotFound` handling in the reconciliation path.

---

## 3. Multi-Cluster Concurrency
**Script:** `7-test-concurrency.sh`

### The Scenario
In a large multi-tenant cluster, multiple namespaces might deploy workloads targeting different remote clusters simultaneously. The operator must process these events concurrently without race conditions, blocking threads, or cross-contaminating namespace policies.

### What the Script Does
1. Creates two isolated namespaces (`test-cluster-remote` and `test-cluster-alpha`).
2. Applies two trapped deployments in sequence. Each deployment explicitly targets a different remote cluster.
3. Waits for the operator to process the events.
4. Verifies that a valid `NamespaceOffloading` policy is generated in *both* namespaces and that both workloads reach the expected state.

### What it Proves
This demonstrates that the controller can process offloading requests for multiple namespaces and target clusters without cross-contaminating their policies. Because the two deployments are applied sequentially, the script does not independently measure simultaneous request submission or prove a particular reconciliation scheduling order. The observed behavior is consistent with requeue-based reconciliation, which avoids blocking a worker with a synchronous `time.Sleep()`.