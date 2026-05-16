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

	// Ignore system namespaces
	if namespace == "kube-system" || namespace == "liqo-system" || namespace == "local-path-storage" || namespace == "crownlabs-system" {
		return ctrl.Result{}, nil
	}

	// 1. Check if the NamespaceOffloading exists.
	offloadCR := &unstructured.Unstructured{}
	offloadCR.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "offloading.liqo.io",
		Version: "v1beta1",
		Kind:    "NamespaceOffloading",
	})

	if err := r.Get(ctx, client.ObjectKey{Name: "offloading", Namespace: namespace}, offloadCR); err != nil {
		// If it's already gone, our job is done.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// 2. List all Pods currently in this namespace
	var podList corev1.PodList
	if err := r.List(ctx, &podList, client.InNamespace(namespace)); err != nil {
		logger.Error(err, "Failed to list pods in namespace")
		return ctrl.Result{}, err
	}

	// 3. Count active remote pods
	activeOffloadedPods := 0
	for _, pod := range podList.Items {
		// Ignore pods that have cleanly finished or died
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}

		// CONDITION A: Did the pod explicitly ask for the remote node?
		explicitTarget := pod.Spec.NodeSelector != nil && pod.Spec.NodeSelector["kubernetes.io/hostname"] == "cluster-remote"

		// CONDITION B: Is Liqo actively running this pod on the remote node?
		scheduledRemote := pod.Spec.NodeName == "cluster-remote"

		// CONDITION C (FIXED): Is the pod Pending, AND it explicitly wants the remote cluster?
		// We no longer trigger on purely local pending pods.
		isPendingRemote := pod.Status.Phase == corev1.PodPending && pod.Spec.NodeName == "" && explicitTarget

		if explicitTarget || scheduledRemote || isPendingRemote {
			activeOffloadedPods++
		}
	}	

	// 4. The Action: Lock the door if empty
	if activeOffloadedPods == 0 {
		// Only try to delete if we successfully fetched it in step 1
		if offloadCR.GetUID() != "" {
			logger.Info("🧹 ZERO active remote pods remain. Initiating lockdown...", "namespace", namespace)
			
			if err := r.Delete(ctx, offloadCR); client.IgnoreNotFound(err) != nil {
				logger.Error(err, "❌ Failed to delete NamespaceOffloading")
				return ctrl.Result{}, err
			}
			logger.Info("🔒 SUCCESS: NamespaceOffloading destroyed. Namespace is strictly isolated again.", "namespace", namespace)
		}
	}
	return ctrl.Result{}, nil
}

func (r *LiqoCleanupReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// THE FIX: Event filtering to save CPU.
	// We only care about Pod state changes that affect our counting logic.
	podStateChangePredicate := predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool { return true },
		DeleteFunc: func(e event.DeleteEvent) bool { return true },
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldPod, okOld := e.ObjectOld.(*corev1.Pod)
			newPod, okNew := e.ObjectNew.(*corev1.Pod)
			if !okOld || !okNew {
				return false
			}
			// Only trigger if Phase or NodeName changed (ignore readiness probe updates, annotations, etc.)
			return oldPod.Status.Phase != newPod.Status.Phase || oldPod.Spec.NodeName != newPod.Spec.NodeName
		},
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Pod{}).
		WithEventFilter(podStateChangePredicate).
		Complete(r)
}