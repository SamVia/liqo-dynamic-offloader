package controllers

import (
	"context"
	"path/filepath"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// LiqoTrapReconciler watches for Pods stuck in OffloadingBackOff and
// provisions the NamespaceOffloading policy required to rescue them.
type LiqoTrapReconciler struct {
	client.Client
	TargetClusters     []string
	ExcludedNamespaces []string
	WhitelistLabels    map[string]string
	BlacklistLabels    map[string]string
	BackoffDuration    time.Duration
	DryRun             bool
}

// Annotations used to coordinate remediation attempts with Liqo's asynchronous
// offloading behavior.
const lastRemediationAnnotation = "dynamic-offloader.liqo.io/last-remediation"
const forceSyncAnnotation = "dynamic-offloader.liqo.io/force-sync"

// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=offloading.liqo.io,resources=namespaceoffloadings,verbs=get;list;watch;create;update;patch

// Reconcile handles pods trapped in Liqo's OffloadingBackOff state. It first
// ensures that the namespace has an offloading policy, then waits for Liqo's
// asynchronous admission and scheduling logic to process that policy.
//
// The pod is re-read after the wait because Liqo may recover it without further
// intervention. If it remains trapped, the force-sync annotation requests a
// fresh synchronization attempt. A namespace-level remediation timestamp
// prevents repeated interventions and reconciliation hot-loops.
func (r *LiqoTrapReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	namespaceName := req.Namespace
	podName := req.Name

	var ns corev1.Namespace
	if err := r.Get(ctx, client.ObjectKey{Name: namespaceName}, &ns); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !r.isNamespaceAllowed(&ns) {
		// Protected namespaces must never be changed by automatic remediation.
		return ctrl.Result{}, nil
	}

	var stuckPod corev1.Pod
	if err := r.Get(ctx, req.NamespacedName, &stuckPod); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !stuckPod.DeletionTimestamp.IsZero() {
		// A terminating pod is already being removed; do not restart remediation
		// work or create a hot-loop while Kubernetes completes deletion.
		return ctrl.Result{}, nil
	}

	logger.Info("TRIGGER FAIL-FAST: Pod in OffloadingBackOff intercepted!", "namespace", namespaceName, "pod", podName)

	// Enforce the persisted namespace-level remediation cooldown before mutating
	// resources. Persisting this timestamp makes the cooldown survive restarts
	// and coordinate workers processing multiple trapped pods in one namespace.
	if lastRemediation := ns.GetAnnotations()[lastRemediationAnnotation]; lastRemediation != "" {
		remediationTime, parseErr := time.Parse(time.RFC3339Nano, lastRemediation)
		if parseErr == nil {
			remaining := r.BackoffDuration - time.Since(remediationTime)
			if remaining > 0 {
				logger.V(1).Info("Remediation backoff is active for namespace", "namespace", namespaceName, "remaining", remaining.Round(time.Millisecond))
				return ctrl.Result{RequeueAfter: remaining}, nil
			}
		}
	}

	offloadCR := &unstructured.Unstructured{}
	offloadCR.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "offloading.liqo.io",
		Version: "v1beta1",
		Kind:    "NamespaceOffloading",
	})

	err := r.Get(ctx, client.ObjectKey{Name: "offloading", Namespace: namespaceName}, offloadCR)

	if apierrors.IsNotFound(err) {
		// Create the minimum policy required to let Liqo establish remote
		// placement before the trapped pod is checked again.
		offloadCR.SetName("offloading")
		offloadCR.SetNamespace(namespaceName)
		spec := map[string]interface{}{
			"namespaceMappingStrategy": "DefaultName",
			"podOffloadingStrategy":    "LocalAndRemote",
		}

		if len(r.TargetClusters) > 0 {
			selector := corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{
				MatchExpressions: []corev1.NodeSelectorRequirement{{
					Key: "liqo.io/remote-cluster-id", Operator: corev1.NodeSelectorOpIn,
					Values: append([]string(nil), r.TargetClusters...),
				}},
			}}}
			selectorMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&selector)
			if err != nil {
				return ctrl.Result{}, err
			}
			spec["clusterSelector"] = selectorMap
		}
		offloadCR.Object["spec"] = spec

		if !r.DryRun {
			if err := r.Create(ctx, offloadCR); err != nil {
				logger.Error(err, "Failed to create NamespaceOffloading policy")
				return ctrl.Result{}, err
			}
			logger.Info("Policy created. Yielding thread to allow Liqo webhooks to register.", "namespace", namespaceName)
		} else {
			logger.Info("[DRY RUN] Would create NamespaceOffloading", "namespace", namespaceName)
		}

		// Yield to Liqo's mutating webhooks and asynchronous scheduling logic.
		// The context-aware wait allows shutdown to interrupt the delay cleanly.
		select {
		case <-time.After(r.BackoffDuration):
		case <-ctx.Done():
			return ctrl.Result{}, ctx.Err()
		}
	} else if err != nil {
		logger.Error(err, "Failed to fetch NamespaceOffloading policy")
		return ctrl.Result{}, err
	}

	// Re-read the pod because Liqo may have recovered it during the backoff.
	// Avoid annotating a pod that is no longer trapped.
	var latestPod corev1.Pod
	if err := r.Get(ctx, req.NamespacedName, &latestPod); err == nil {
		if !isPodTrapped(&latestPod) {
			logger.Info("Pod successfully auto-recovered by Liqo during backoff. Skipping edit.", "pod", podName)
			return ctrl.Result{}, nil
		}

		if !r.DryRun {
			annotations := latestPod.GetAnnotations()
			if annotations == nil {
				annotations = make(map[string]string)
			}
			// The timestamp changes the pod object and asks downstream Liqo
			// components to process a new synchronization attempt.
			annotations[forceSyncAnnotation] = time.Now().UTC().Format(time.RFC3339Nano)
			latestPod.SetAnnotations(annotations)

			if err := r.Update(ctx, &latestPod); err != nil {
				logger.Error(err, "Failed to edit the stuck pod for forced sync", "pod", podName)
				return ctrl.Result{}, err
			}

			if err := r.persistRemediationBackoff(ctx, namespaceName); err != nil {
				logger.Error(err, "Failed to persist namespace remediation backoff", "namespace", namespaceName)
				return ctrl.Result{}, err
			}
			logger.Info("Successfully patched the stuck pod to trigger kubelet sync.", "pod", podName)
		} else {
			// Dry-run evaluates the same recovery path but does not create,
			// update, or annotate any cluster resource.
			logger.Info("[DRY RUN] Would patch the stuck pod with force-sync annotation", "pod", podName)
		}

	} else if !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// isNamespaceAllowed applies namespace exclusions and label policy before
// remediation can create or mutate cluster resources.
func (r *LiqoTrapReconciler) isNamespaceAllowed(ns *corev1.Namespace) bool {
	for _, pattern := range r.ExcludedNamespaces {
		if matched, _ := filepath.Match(pattern, ns.Name); matched {
			return false
		}
	}
	for k, v := range r.BlacklistLabels {
		if val, exists := ns.Labels[k]; exists && (v == "" || val == v) {
			return false
		}
	}
	if len(r.WhitelistLabels) > 0 {
		for k, v := range r.WhitelistLabels {
			if val, exists := ns.Labels[k]; !exists || (v != "" && val != v) {
				return false
			}
		}
	}
	return true
}

// persistRemediationBackoff safely records the remediation time using
// optimistic-lock conflict retries. This preserves namespace metadata when
// another controller or worker updates the object concurrently.
func (r *LiqoTrapReconciler) persistRemediationBackoff(ctx context.Context, namespace string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var ns corev1.Namespace
		if err := r.Get(ctx, client.ObjectKey{Name: namespace}, &ns); err != nil {
			return err
		}

		annotations := ns.GetAnnotations()
		if annotations == nil {
			annotations = make(map[string]string)
		}
		annotations[lastRemediationAnnotation] = time.Now().UTC().Format(time.RFC3339Nano)
		ns.SetAnnotations(annotations)

		return r.Update(ctx, &ns)
	})
}

// isPodTrapped checks the pod reason and both application and init-container
// waiting states because Liqo may report OffloadingBackOff at any of them.
func isPodTrapped(pod *corev1.Pod) bool {
	if pod.Status.Reason == "OffloadingBackOff" {
		return true
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.State.Waiting != nil && cs.State.Waiting.Reason == "OffloadingBackOff" {
			return true
		}
	}
	for _, cs := range pod.Status.InitContainerStatuses {
		if cs.State.Waiting != nil && cs.State.Waiting.Reason == "OffloadingBackOff" {
			return true
		}
	}
	return false
}

// LiqoTrapPredicate filters pod events to current OffloadingBackOff states.
// Delete events are ignored because a deleted pod no longer needs remediation.
func LiqoTrapPredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			pod, ok := e.Object.(*corev1.Pod)
			return ok && isPodTrapped(pod)
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			newPod, ok := e.ObjectNew.(*corev1.Pod)
			return ok && isPodTrapped(newPod)
		},
		DeleteFunc: func(e event.DeleteEvent) bool { return false },
	}
}

// SetupWithManager registers the reconciler for pod events and applies the
// predicate before requests enter the controller work queue.
func (r *LiqoTrapReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Pod{}).
		WithEventFilter(LiqoTrapPredicate()).
		Complete(r)
}
