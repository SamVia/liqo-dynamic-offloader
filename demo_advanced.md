# Liqo Strict Isolation & Advanced Auto-Healing Demo

This advanced demo extends the base security proof with a full auto-healing controller pair from the `test` folder.

It shows that Liqo still enforces strict namespace isolation, then builds a controller that:

- detects rejected remote pod attempts via `ReflectionDisabled` events,
- auto-applies `NamespaceOffloading` for the trapped namespace,
- deletes the stuck pod so the workload can retry,
- and finally tears down the offloading policy when the namespace becomes empty.

## Prerequisites

Ensure you have the following installed on your machine:

- Docker
- `kind`
- `kubectl`
- `liqoctl` (v1.0 or higher)
- Go (v1.21+)

---

## Phase 1: Infrastructure Setup

Use `test/1-setup.sh` to create two local Kind clusters, install Liqo in both, and peer them with a NodePort gateway.

Create `test/1-setup.sh` with the following content:

```bash
#!/bin/bash
set -e

echo "\033[1;32m Step 1: Creating local and remote Kind clusters...\033[0m"
kind create cluster --name cluster-local || true
kind create cluster --name cluster-remote || true

echo "\033[1;32m Step 2: Installing Liqo (v1.0+)...\033[0m"
kubectl config use-context kind-cluster-local
liqoctl install kind --cluster-id cluster-local

kubectl config use-context kind-cluster-remote
liqoctl install kind --cluster-id cluster-remote

echo "\033[1;32m Step 3: Peering clusters using NodePort...\033[0m"
liqoctl peer --context kind-cluster-local --remote-context kind-cluster-remote --gw-server-service-type NodePort

echo "\033[1;32mWaiting for Liqo virtual node to be created by the controller...\033[0m"
while [ -z "$(kubectl get nodes -l liqo.io/type=virtual-node --context kind-cluster-local -o custom-columns=NAME:.metadata.name --no-headers 2>/dev/null)" ]; do
  sleep 2
done

echo "\033[1;32mVirtual node detected! Waiting for it to become Ready...\033[0m"
kubectl wait --for=condition=Ready node -l liqo.io/type=virtual-node --timeout=120s --context kind-cluster-local

echo "\033[1;32mSetup complete! Virtual node is ready. NO namespaces are offloaded.\033[0m"
kubectl get nodes --context kind-cluster-local
```

Make it executable and run it:

```bash
chmod +x test/1-setup.sh
./test/1-setup.sh
```

---

## Phase 2: The Advanced Trap Demo

The advanced demo now deploys a `Deployment` rather than a raw `Pod` and monitors Liqo events while the deployment attempts to use the remote virtual node.

Create `test/2-demo.sh` with this content:

```bash
#!/bin/bash

kubectl config use-context kind-cluster-local >/dev/null 2>&1

echo "Step 1: Generating the Deep Trap Deployment YAML..."
cat <<EOF > /tmp/trap-deployment.yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: liqo-trap-deployment
  namespace: default
spec:
  replicas: 1
  selector:
    matchLabels:
      app: liqo-trap
  template:
    metadata:
      labels:
        app: liqo-trap
    spec:
      containers:
      - name: nginx
        image: nginx:alpine
      nodeSelector:
        kubernetes.io/hostname: cluster-remote
      tolerations:
      - key: "virtual-node.liqo.io/not-allowed"
        operator: "Exists"
EOF

echo "Cleaning up any previous runs..."
kubectl delete deployment liqo-trap-deployment -n default --ignore-not-found=true >/dev/null 2>&1
kubectl delete namespaceoffloading offloading -n default --ignore-not-found=true >/dev/null 2>&1

sleep 2

echo "👀 Step 2: Starting Telemetry Monitors in the background..."
echo "---------------------------------------------------------------------"

trap 'echo -e "\n Demo terminated. Cleaning up background processes..."; kill $(jobs -p) 2>/dev/null; exit' SIGINT SIGTERM

# Watch the pods normally so the audience clearly sees the offloading status.
kubectl get pods -w | grep --line-buffered "liqo-trap" &

kubectl get events --field-selector involvedObject.kind=Pod --watch-only \
  -o custom-columns="REASON:.reason,MESSAGE:.message" | \
  awk '
    /ReflectionDisabled/ {
      print "\n\033[1;31m VIRTUAL KUBELET REJECTION DETECTED \033[0m"
      print "\033[1;31mReason:\033[0m  ReflectionDisabled"
      print "\033[1;31mMessage:\033[0m " substr($0, index($0, $2)) "\n"
    }
  ' &

sleep 2

echo -e "\n Step 3: Springing the trap (Deploying pod to non-offloaded namespace)..."
kubectl apply -f /tmp/trap-deployment.yaml

echo -e "\nWaiting for the Virtual Kubelet to react (Press CTRL+C to exit demo)...\n"

wait
```

Make it executable:

```bash
chmod +x test/2-demo.sh
```

---

## Phase 3: Build the Advanced Go Operator

The new `test` folder includes a two-controller operator that:

- detects Liqo rejection events,
- auto-applies `NamespaceOffloading`,
- deletes the stuck pod to allow retry,
- and removes the offloading policy when the namespace becomes idle.

### 3.1 Initialize the project

From `test/`:

```bash
go mod init liqo-demo/operator
```

### 3.2 `main.go`

Create `test/main.go` with this content:

```go
package main

import (
  "os"

  "liqo-demo/operator/controllers"

  _ "k8s.io/client-go/plugin/pkg/client/auth"
  ctrl "sigs.k8s.io/controller-runtime"
  "sigs.k8s.io/controller-runtime/pkg/log/zap"
)

func main() {
  ctrl.SetLogger(zap.New(zap.UseDevMode(true)))
  setupLog := ctrl.Log.WithName("setup")

  mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{})
  if err != nil {
    setupLog.Error(err, "unable to start manager")
    os.Exit(1)
  }

  if err = (&controllers.LiqoTrapReconciler{
    Client: mgr.GetClient(),
  }).SetupWithManager(mgr); err != nil {
    setupLog.Error(err, "unable to create controller", "controller", "LiqoTrap")
    os.Exit(1)
  }

  if err = (&controllers.LiqoCleanupReconciler{
    Client: mgr.GetClient(),
  }).SetupWithManager(mgr); err != nil {
    setupLog.Error(err, "unable to create controller", "controller", "LiqoCleanup")
    os.Exit(1)
  }

  setupLog.Info("Starting controller with both LiqoTrap and LiqoCleanup")
  if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
    setupLog.Error(err, "problem running controller manager")
    os.Exit(1)
  }
}
```

### 3.3 `controllers/liqo_trap_controller.go`

Create `test/controllers/liqo_trap_controller.go` with this content:

```go
package controllers

import (
    "context"
    "time"

    corev1 "k8s.io/api/core/v1"
    apierrors "k8s.io/apimachinery/pkg/api/errors"
    metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
    "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
    "k8s.io/apimachinery/pkg/runtime/schema"
    ctrl "sigs.k8s.io/controller-runtime"
    "sigs.k8s.io/controller-runtime/pkg/client"
    "sigs.k8s.io/controller-runtime/pkg/event"
    "sigs.k8s.io/controller-runtime/pkg/log"
    "sigs.k8s.io/controller-runtime/pkg/predicate"
)

// LiqoTrapReconciler monitors for specific Kubernetes events indicating a pod 
// scheduling trap due to missing Liqo offloading configurations.
type LiqoTrapReconciler struct {
    client.Client
}

// +kubebuilder:rbac:groups="",resources=events,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups=offloading.liqo.io,resources=namespaceoffloadings,verbs=get;list;watch;create;update;patch

func (r *LiqoTrapReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
    logger := log.FromContext(ctx)

    // Retrieve the specific Event triggering the reconciliation.
    var evt corev1.Event
    if err := r.Get(ctx, req.NamespacedName, &evt); err != nil {
        return ctrl.Result{}, client.IgnoreNotFound(err)
    }

    namespace := evt.InvolvedObject.Namespace
    podName := evt.InvolvedObject.Name

    // Exclude critical system namespaces to avoid inadvertently applying 
    // offloading policies to core control plane or storage components.
    if namespace == "kube-system" || namespace == "liqo-system" || namespace == "local-path-storage" || namespace == "crownlabs-system" {
        return ctrl.Result{}, nil
    }

    logger.Info("Missing NamespaceOffloading detected for pod. Initiating remediation.",
        "namespace", namespace,
        "pod", podName,
    )

    // Construct the unstructured NamespaceOffloading Custom Resource 
    // to define the remote execution policy for this namespace.
    offloadCR := &unstructured.Unstructured{}
    offloadCR.SetGroupVersionKind(schema.GroupVersionKind{
        Group:   "offloading.liqo.io",
        Version: "v1beta1",
        Kind:    "NamespaceOffloading",
    })
    offloadCR.SetName("offloading")
    offloadCR.SetNamespace(namespace)
    offloadCR.Object["spec"] = map[string]interface{}{
        "namespaceMappingStrategy": "DefaultName",
        "podOffloadingStrategy":    "LocalAndRemote",
        "clusterSelector": map[string]interface{}{
            "nodeSelectorTerms": []interface{}{
                map[string]interface{}{
                    "matchExpressions": []interface{}{
                        map[string]interface{}{
                            "key":      "liqo.io/remote-cluster-id",
                            "operator": "In",
                            "values":   []interface{}{"cluster-remote"},
                        },
                    },
                },
            },
        },
    }

    // Apply the NamespaceOffloading policy to the target namespace.
    err := r.Create(ctx, offloadCR)
    if err != nil {
        if apierrors.IsAlreadyExists(err) {
            logger.Info("NamespaceOffloading already exists. Proceeding to delete the trapped pod.", "namespace", namespace)
        } else {
            logger.Error(err, "Failed to create NamespaceOffloading policy")
            return ctrl.Result{}, err
        }
    } else {
        // Requeue the request after creation rather than sleeping. This yields the thread
        // and provides Liqo's mutating webhooks sufficient time to process the new policy 
        // before we attempt to delete the pod.
        logger.Info("Successfully applied NamespaceOffloading. Requeueing to allow webhook registration.", "namespace", namespace)
        return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
    }

    // Delete the trapped pod. This step is reached either if the policy already existed 
    // or during the requeue following a successful policy creation.
    var stuckPod corev1.Pod
    if err := r.Get(ctx, client.ObjectKey{Name: podName, Namespace: namespace}, &stuckPod); err == nil {

        // Utilize Background deletion propagation to ensure the API server handles 
        // garbage collection asynchronously, preventing the client from hanging.
        deletePolicy := metav1.DeletePropagationBackground
        deleteOpts := &client.DeleteOptions{
            PropagationPolicy: &deletePolicy,
        }

        if err := r.Delete(ctx, &stuckPod, deleteOpts); err != nil {
            logger.Error(err, "Failed to delete the stuck pod", "pod", podName)
            // Returning the error triggers the controller's backoff retry mechanism.
            return ctrl.Result{}, err
        }
        logger.Info("Successfully deleted the stuck pod. The managing controller (e.g., ReplicaSet) should provision a replacement.", "pod", podName)
    } else if !apierrors.IsNotFound(err) {
        logger.Error(err, "Failed to fetch the stuck pod for deletion", "pod", podName)
        return ctrl.Result{}, err
    }

    return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *LiqoTrapReconciler) SetupWithManager(mgr ctrl.Manager) error {
    // Optimize the controller by filtering events at the source. This prevents 
    // the controller from processing the entire cluster event stream, focusing 
    // exclusively on the specific reflection disablement trap condition.
    liqoTrapFilter := predicate.Funcs{
        CreateFunc: func(e event.CreateEvent) bool {
            evt, ok := e.Object.(*corev1.Event)
            if !ok {
                return false
            }
            return evt.Reason == "ReflectionDisabled" && evt.InvolvedObject.Kind == "Pod"
        },
        // Ignore update and delete events as this remediation logic is only 
        // triggered by the initial creation of the targeted Event.
        UpdateFunc: func(e event.UpdateEvent) bool { return false },
        DeleteFunc: func(e event.DeleteEvent) bool { return false },
    }

    return ctrl.NewControllerManagedBy(mgr).
        For(&corev1.Event{}).
        WithEventFilter(liqoTrapFilter).
        Complete(r)
}
```

### 3.4 `controllers/liqo_cleanup_controller.go`

Create `test/controllers/liqo_cleanup_controller.go` with this content:

```go
package controllers

import (
    "context"

    corev1 "k8s.io/api/core/v1"
    "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
    "k8s.io/apimachinery/pkg/runtime/schema"
    ctrl "sigs.k8s.io/controller-runtime"
    "sigs.k8s.io/controller-runtime/pkg/client"
    "sigs.k8s.io/controller-runtime/pkg/event"
    "sigs.k8s.io/controller-runtime/pkg/log"
    "sigs.k8s.io/controller-runtime/pkg/predicate"
)

type LiqoCleanupReconciler struct {
    client.Client
}

// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=offloading.liqo.io,resources=namespaceoffloadings,verbs=get;list;watch;delete

func (r *LiqoCleanupReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
    logger := log.FromContext(ctx)
    namespace := req.Namespace

    // Exclude system and critical namespaces to prevent accidental deletion of their offloading configurations.
    if namespace == "kube-system" || namespace == "liqo-system" || namespace == "local-path-storage" || namespace == "crownlabs-system" {
        return ctrl.Result{}, nil
    }

    // Verify if a NamespaceOffloading resource exists for the current namespace.
    offloadCR := &unstructured.Unstructured{}
    offloadCR.SetGroupVersionKind(schema.GroupVersionKind{
        Group:   "offloading.liqo.io",
        Version: "v1beta1",
        Kind:    "NamespaceOffloading",
    })

    if err := r.Get(ctx, client.ObjectKey{Name: "offloading", Namespace: namespace}, offloadCR); err != nil {
        // If the resource is not found, the cleanup is already complete.
        return ctrl.Result{}, client.IgnoreNotFound(err)
    }

    // Retrieve all Pods within the namespace to evaluate their current scheduling and execution status.
    var podList corev1.PodList
    if err := r.List(ctx, &podList, client.InNamespace(namespace)); err != nil {
        logger.Error(err, "Failed to list pods in namespace")
        return ctrl.Result{}, err
    }

    // Calculate the number of active Pods that rely on the remote cluster.
    activeOffloadedPods := 0
    for _, pod := range podList.Items {
        // Exclude pods that have reached a terminal state.
        if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
            continue
        }

        // Condition A: The pod explicitly requests scheduling on the remote cluster via NodeSelector.
        explicitTarget := pod.Spec.NodeSelector != nil && pod.Spec.NodeSelector["kubernetes.io/hostname"] == "cluster-remote"

        // Condition B: The pod has already been scheduled and is running on the remote node.
        scheduledRemote := pod.Spec.NodeName == "cluster-remote"

        // Condition C: The pod is pending scheduling specifically for the remote cluster.
        // This ensures we do not block cleanup for pending pods intended for local nodes.
        isPendingRemote := pod.Status.Phase == corev1.PodPending && pod.Spec.NodeName == "" && explicitTarget

        if explicitTarget || scheduledRemote || isPendingRemote {
            activeOffloadedPods++
        }
    }   

    // If no active remote pods remain, proceed to revoke the offloading configuration.
    if activeOffloadedPods == 0 {
        // Confirm the resource was successfully fetched before attempting deletion.
        if offloadCR.GetUID() != "" {
            logger.Info("Zero active remote pods remain. Initiating offload cleanup.", "namespace", namespace)
            
            if err := r.Delete(ctx, offloadCR); client.IgnoreNotFound(err) != nil {
                logger.Error(err, "Failed to delete NamespaceOffloading")
                return ctrl.Result{}, err
            }
            logger.Info("Successfully deleted NamespaceOffloading. Namespace isolation restored.", "namespace", namespace)
        }
    }
    return ctrl.Result{}, nil
}

func (r *LiqoCleanupReconciler) SetupWithManager(mgr ctrl.Manager) error {
    // Optimize the controller by filtering events to reduce unnecessary Reconcile calls.
    // We only trigger reconciliation when pod state changes affect our remote counting logic.
    podStateChangePredicate := predicate.Funcs{
        CreateFunc: func(e event.CreateEvent) bool { return true },
        DeleteFunc: func(e event.DeleteEvent) bool { return true },
        UpdateFunc: func(e event.UpdateEvent) bool {
            oldPod, okOld := e.ObjectOld.(*corev1.Pod)
            newPod, okNew := e.ObjectNew.(*corev1.Pod)
            if !okOld || !okNew {
                return false
            }
            // Trigger reconciliation only if the Pod's Phase or assigned Node changes.
            // This safely ignores updates to annotations, labels, or readiness probes.
            return oldPod.Status.Phase != newPod.Status.Phase || oldPod.Spec.NodeName != newPod.Spec.NodeName
        },
    }

    return ctrl.NewControllerManagedBy(mgr).
        For(&corev1.Pod{}).
        WithEventFilter(podStateChangePredicate).
        Complete(r)
}
```

---

## Phase 4: Run the Advanced Demo

1. Start the operator from the `test/` directory:

```bash
cd test
go run .
```

2. In another shell, run the advanced trap demo:

```bash
./test/2-demo.sh
```

You should see:

- the `liqo-trap-deployment` entering a blocked/offloaded state,
- a `ReflectionDisabled` event emitted by the Virtual Kubelet,
- the Go operator auto-creating `NamespaceOffloading`,
- the stuck pod being deleted so the deployment can retry.

---

## Optional Reset

Use `test/0-reset.sh` to clean the demo state and restore strict isolation for the `default` namespace:

```bash
chmod +x test/0-reset.sh
./test/0-reset.sh
```

This advanced demo file now incorporates the full `test/` folder workflow: the improved setup scripts, the trap deployment, the real-time rejection telemetry, and the auto-healing Go operator with cleanup logic.
