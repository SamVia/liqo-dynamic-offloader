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
