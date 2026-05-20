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

// LiqoCleanupPredicate optimizes the controller by filtering events to reduce unnecessary Reconcile calls.
// We only trigger reconciliation when pod state changes affect our remote counting logic.
// Exposed publicly to allow for unit testing.
func LiqoCleanupPredicate() predicate.Predicate {
	return predicate.Funcs{
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
}

// SetupWithManager sets up the controller with the Manager.
func (r *LiqoCleanupReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Pod{}).
		WithEventFilter(LiqoCleanupPredicate()). // Uses the extracted predicate
		Complete(r)
}
