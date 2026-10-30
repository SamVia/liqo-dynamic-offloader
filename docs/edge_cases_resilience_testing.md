# Edge Cases & Resilience Testing

This document outlines the testing strategies used to validate the robustness, concurrency handling, and state-reconciliation capabilities of the **Liqo Dynamic Offloader** operator.

We have provided four automated Bash scripts that demonstrate the operator's behavior under stressful or unexpected cluster conditions. The scripts run the operator locally against Kind clusters and automatically invoke the setup script to prepare the required clusters if they do not exist. Scripts 5, 7, and 8 compile the operator with `go build`; script 6 runs it with `go run`. All scripts include automated cleanup for local processes and test resources.

### Prerequisites

The scripts require Docker, Kind, `kubectl`, `liqoctl` (v1.2.0 or newer), Go (1.22+), and Bash, Git Bash, or WSL. Run them from the repository root:

```bash
chmod +x scripts/*.sh
./scripts/5-test-resilience.sh
./scripts/6-test-edge-case.sh
./scripts/7-test-concurrency.sh
./scripts/8-test-multi-cluster-spread.sh
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

---

## 4. Multi-Cluster Pod Distribution (Topology Spread)
**Script:** `8-test-multi-cluster-spread.sh`

### The Scenario
When peering with multiple remote clusters (`cluster-remote` and `cluster-alpha`), an operator configured with multiple target clusters (`--target-cluster-ids=cluster-remote,cluster-alpha`) generates a `NamespaceOffloading` policy with a `clusterSelector` admitting both clusters. 

This test proves that a single policy genuinely enables multi-cluster offloading for all allowed targets and allows multi-replica workloads to spread dynamically across all peered virtual nodes without hardcoding hostnames on individual pods.

### What the Script Does
1. Launches the local operator configured with `--target-cluster-ids="cluster-remote,cluster-alpha"`.
2. Creates a single namespace (`test-cluster-spread`).
3. Deploys a workload with 4 replicas configured with:
   * Generic virtual node selector: `nodeSelector: {"liqo.io/type": "virtual-node"}`.
   * Liqo taint toleration: `tolerations: [{key: "virtual-node.liqo.io/not-allowed", operator: "Exists"}]`.
   * Topology spread constraint: `topologySpreadConstraints` on `kubernetes.io/hostname` to distribute replicas evenly.
4. Verifies that the Trap controller creates `NamespaceOffloading` covering both clusters, recovers the pods from `OffloadingBackOff`, and achieves `Running` status on both `cluster-remote` and `cluster-alpha`.
5. Deletes the workload and verifies that the Cleanup controller revokes the offloading policy when the namespace becomes idle.

### How to Run
```bash
./scripts/8-test-multi-cluster-spread.sh
```

### Verified Execution Output
```text
=== Multi-Cluster Distribution Analysis ===
NAME                                READY   STATUS    RESTARTS   AGE   IP           NODE             NOMINATED NODE   READINESS GATES
spread-deployment-599887985-hbqvv   1/1     Running   0          15s   10.71.0.13   cluster-remote   <none>           <none>
spread-deployment-599887985-hv8cs   1/1     Running   0          15s   10.71.0.12   cluster-remote   <none>           <none>
spread-deployment-599887985-qm7j9   1/1     Running   0          15s   10.69.0.12   cluster-alpha    <none>           <none>
spread-deployment-599887985-rwnqp   1/1     Running   0          15s   10.69.0.13   cluster-alpha    <none>           <none>

------------------------------------------------------------
Pods on cluster-remote: 2
Pods on cluster-alpha:  2
------------------------------------------------------------
[SUCCESS] A single NamespaceOffloading policy successfully distributed the workload across BOTH peered clusters!

=== Testing Phase 2: Cleanup Controller Verification ===
Deleting deployment to test automated policy revocation when namespace is idle...
Waiting for cleanup countdown and policy deletion (up to 35s)...
[SUCCESS] NamespaceOffloading policy was successfully cleaned up after workload ended!

=== Test 8 Completed Successfully ===
```

### What it Proves & Difference with Test 7
* **Test 7 vs. Test 8 Distinction:**
  * In **Test 7**, two namespaces each contain a pod explicitly pinned via `nodeSelector: {"kubernetes.io/hostname": "<node>"}`. It demonstrates multi-namespace concurrency and isolation without cross-contamination.
  * In **Test 8**, a single namespace runs a multi-replica deployment without hardcoded node names (`nodeSelector: {"liqo.io/type": "virtual-node"}`). It confirms that a single `NamespaceOffloading` policy created with `--target-cluster-ids=cluster-remote,cluster-alpha` genuinely allows the native Kubernetes scheduler (`topologySpreadConstraints`) to distribute workloads across all peered Liqo remote clusters.
* **Automated Cleanup:** It additionally confirms that once all distributed pods are deleted, the Cleanup controller detects zero active remote workloads across all peered clusters and safely tears down the policy.

