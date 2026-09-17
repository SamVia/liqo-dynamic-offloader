# Liqo Strict Isolation & Custom Controller Demo

> The runnable scripts for this repository are in `scripts/`. Run commands
> from the repository root with Bash, Git Bash, or WSL. The examples below
> explain the demo phases; use the checked-in scripts when running them.

This demo proves the core security boundaries of [Liqo](https://liqo.io/), a multi-cluster networking and offloading mesh.

Specifically, it demonstrates that **peering is not enough** to allow cross-cluster execution. By forcefully bypassing the Kube-scheduler, we prove that Liqo's Virtual Kubelet acts as a hard security boundary, actively rejecting workloads unless a namespace is explicitly authorized via a `NamespaceOffloading` policy. Finally, we build a custom Kubernetes Operator in Go to detect this exact security rejection in real-time.

## Prerequisites

Ensure you have the following installed on your machine:

* Docker
* `kind` (Kubernetes IN Docker)
* `kubectl`
* `liqoctl` (v1.0 or higher)
* Go (v1.21+)

---

## Phase 1: Infrastructure Setup

Use [`scripts/1-setup.sh`](../scripts/1-setup.sh) to spin up two local Kind
clusters, install the Liqo control plane, and peer them together securely
using a NodePort gateway.

```bash
#!/bin/bash
set -e

echo "Step 1: Creating local and remote Kind clusters..."
kind create cluster --name cluster-local || true
kind create cluster --name cluster-remote || true

echo "Step 2: Installing Liqo (v1.0+)..."
kubectl config use-context kind-cluster-local
liqoctl install kind --cluster-id cluster-local

kubectl config use-context kind-cluster-remote
liqoctl install kind --cluster-id cluster-remote

echo "Step 3: Peering clusters using NodePort..."
liqoctl peer --context kind-cluster-local --remote-context kind-cluster-remote --gw-server-service-type NodePort

echo "Waiting for Liqo virtual node to be created by the controller..."
while [ -z "$(kubectl get nodes -l liqo.io/type=virtual-node --context kind-cluster-local -o custom-columns=NAME:.metadata.name --no-headers 2>/dev/null)" ]; do
  sleep 2
done

echo "Virtual node detected! Waiting for it to become Ready..."
kubectl wait --for=condition=Ready node -l liqo.io/type=virtual-node --timeout=120s --context kind-cluster-local

echo "Setup complete! Virtual node is ready. NO namespaces are offloaded."
kubectl get nodes --context kind-cluster-local

```

Make it executable and run it:

```bash
chmod +x scripts/1-setup.sh
./scripts/1-setup.sh

```

---

## Phase 2: The Trap & Telemetry

Use [`scripts/2-demo.sh`](../scripts/2-demo.sh). This script generates a
Deployment that attempts to bypass the Kube-scheduler using a specific
toleration, forcing it into the Virtual Kubelet's domain. It also starts
background monitors to catch the precise moment the Virtual Kubelet rejects
it.

```bash
#!/bin/bash

kubectl config use-context kind-cluster-local >/dev/null 2>&1

echo "Step 1: Generating the Deep Trap YAML..."
cat <<EOF > /tmp/trap-pod.yaml
apiVersion: v1
kind: Pod
metadata:
  name: liqo-trap-pod
  namespace: default
spec:
  containers:
  - name: nginx
    image: nginx:alpine
  nodeSelector:
    liqo.io/type: virtual-node
  # THE VIP PASS: We tolerate the taint to bypass Kube-scheduler 
  # and force the pod directly into the Virtual Kubelet's domain.
  tolerations:
  - key: "virtual-node.liqo.io/not-allowed"
    operator: "Exists"
EOF

echo "Cleaning up any previous runs..."
kubectl delete pod liqo-trap-pod --ignore-not-found=true --wait=true >/dev/null 2>&1

echo "Step 2: Starting Telemetry Monitors in the background..."
echo "---------------------------------------------------------------------"

trap 'echo -e "\nDemo terminated. Cleaning up background processes..."; kill $(jobs -p) 2>/dev/null; exit' SIGINT SIGTERM

# Task A: Watch ALL pods to prevent "NotFound" crashes, filter for our trap
kubectl get pods -w \
  -o custom-columns="TIME:.metadata.creationTimestamp,NAME:.metadata.name,PHASE:.status.phase" | grep --line-buffered "liqo-trap-pod" &

# Task B: Watch events for the Virtual Kubelet's ReflectionDisabled event
kubectl get events --field-selector involvedObject.name=liqo-trap-pod --watch-only \
  -o custom-columns="REASON:.reason,MESSAGE:.message" | \
  awk '
    /ReflectionDisabled/ {
      print "\n\033[1;31m VIRTUAL KUBELET REJECTION DETECTED \033[0m"
      print "\033[1;31mReason:\033[0m  ReflectionDisabled"
      print "\033[1;31mMessage:\033[0m " substr($0, index($0, $2)) "\n"
    }
    /FailedScheduling/ {
      print "\n\033[1;33m  SCHEDULER WARNING \033[0m"
      print "\033[1;33mReason:\033[0m  FailedScheduling"
      print "\033[1;33mMessage:\033[0m " substr($0, index($0, $2)) "\n"
    }
    !/ReflectionDisabled/ && !/FailedScheduling/ {
      if (NR>1) print "  " $0
    }
  ' &

sleep 2

echo -e "\n Step 3: Springing the trap (Deploying pod to non-offloaded namespace)..."
kubectl apply -f /tmp/trap-pod.yaml

echo -e "\n Waiting for the Virtual Kubelet to react (Press CTRL+C to exit demo)...\n"
wait

```

Make it executable with `chmod +x scripts/2-demo.sh` *(do not run it just yet).* 

---

## Phase 3: The Custom Controller

We will build a Go-based Kubernetes controller that listens to the Event stream, filters out cluster noise (like `kube-system` DaemonSets), and triggers a custom alert when a user workload is rejected by Liqo.

### 1. Initialize the Project

Open a new terminal and set up the Go module:

```bash
mkdir -p ./controllers
go mod init liqo-demo/operator

```

### 2. Create the Reconciler

Create `controllers/liqo_trap_controller.go` and paste the following:

```go
package controllers

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

type LiqoTrapReconciler struct {
	client.Client
}

// +kubebuilder:rbac:groups=core,resources=events,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch

func (r *LiqoTrapReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var event corev1.Event
	if err := r.Get(ctx, req.NamespacedName, &event); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Filter for the Liqo Virtual Kubelet trap
	if event.Reason == "ReflectionDisabled" && event.InvolvedObject.Kind == "Pod" {
		namespace := event.InvolvedObject.Namespace

		// Filter: Ignore standard system namespaces
		if namespace == "kube-system" || namespace == "liqo-system" || namespace == "local-path-storage" {
			return ctrl.Result{}, nil
		}

		podName := event.InvolvedObject.Name

		// Output our custom detection alert!
		logger.Info("ERROR DETECTED: We are missing the NamespaceOffloading!", 
			"namespace", namespace, 
			"pod", podName,
		)
	}

	return ctrl.Result{}, nil
}

func (r *LiqoTrapReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Event{}). // Watch Events, not Pods!
		Complete(r)
}

```

### 3. Create the Main Application

Create `main.go` in the root of your project:

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

	setupLog.Info("Starting Liqo Trap Controller...")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}

```

### 4. Install Dependencies

```bash
go get k8s.io/client-go@v0.29.2
go get sigs.k8s.io/controller-runtime@v0.17.2
go mod tidy

```

---

## Execution: The Final Demo

You will need two terminal windows open to execute this demo smoothly.

**In Terminal 1 (The Controller):**
Ensure you are pointing at the local cluster and run your Go application.

```bash
kubectl config use-context kind-cluster-local
go run main.go

```

*Wait for it to say ` Starting Liqo Trap Controller...*`

**In Terminal 2 (The Trap):**
Execute your telemetry and deployment script.

```bash
./scripts/2-demo.sh

```

### Expected Outcome

1. In **Terminal 2**, you will see the pod bypass the scheduler (`Successfully assigned`) and then violently crash into the Virtual Kubelet wall, outputting a red ` VIRTUAL KUBELET REJECTION DETECTED ` alert.
2. In **Terminal 1**, your custom Go controller will instantly catch the event, bypass the system noise, and print your custom log:
`INFO  ERROR DETECTED: We are missing the NamespaceOffloading!  {"namespace": "default", "pod": "liqo-trap-pod"}`