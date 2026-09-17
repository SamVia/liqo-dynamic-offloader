package controllers

import (
	"context"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

// LiqoTrapReconciler watches for Pods that are stuck in an "OffloadingBackOff"
// state and automatically provisions the missing NamespaceOffloading policies to rescue them.
type LiqoTrapReconciler struct {
	client.Client
	TargetClusters     []string
	ExcludedNamespaces map[string]bool
	BackoffDuration    time.Duration
}

const lastRemediationAnnotation = "liqo-dynamic-offloader.crownlabs.polito.it/last-remediation"

// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups=offloading.liqo.io,resources=namespaceoffloadings,verbs=get;list;watch;create;update;patch

func (r *LiqoTrapReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	namespace := req.Namespace
	podName := req.Name

	// 1. Exclude Protected Critical Namespaces (Early Exit Pattern)
	// System namespaces must not be automatically offloaded to prevent cluster instability.
	if r.ExcludedNamespaces[namespace] {
		return ctrl.Result{}, nil
	}

	// 2. Retrieve the Target Pod
	var stuckPod corev1.Pod
	if err := r.Get(ctx, req.NamespacedName, &stuckPod); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Ignore pods that are already gracefully shutting down to avoid hot-loops on dying resources.
	if !stuckPod.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	// 3. Verify OffloadingBackOff State (Early Exit Pattern)
	// We check both the main pod status reason and individual container waiting states.
	isTrapped := false
	if stuckPod.Status.Reason == "OffloadingBackOff" {
		isTrapped = true
	}
	for _, cs := range stuckPod.Status.ContainerStatuses {
		if cs.State.Waiting != nil && cs.State.Waiting.Reason == "OffloadingBackOff" {
			isTrapped = true
		}
	}
	for _, cs := range stuckPod.Status.InitContainerStatuses {
		if cs.State.Waiting != nil && cs.State.Waiting.Reason == "OffloadingBackOff" {
			isTrapped = true
		}
	}

	// If the pod is not currently trapped, interrupt the reconciliation immediately.
	if !isTrapped {
		return ctrl.Result{}, nil
	}

	// === REMEDIATION PHASE (Atomic) ===
	logger.Info("TRIGGER FAIL-FAST: Pod in OffloadingBackOff intercepted!", "namespace", namespace, "pod", podName)

	offloadCR := &unstructured.Unstructured{}
	offloadCR.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "offloading.liqo.io",
		Version: "v1beta1",
		Kind:    "NamespaceOffloading",
	})

	err := r.Get(ctx, client.ObjectKey{Name: "offloading", Namespace: namespace}, offloadCR)

	// 4. Create the NamespaceOffloading Policy if missing
	if apierrors.IsNotFound(err) {
		offloadCR.SetName("offloading")
		offloadCR.SetNamespace(namespace)
		spec := map[string]interface{}{
			"namespaceMappingStrategy": "DefaultName",
			"podOffloadingStrategy":    "LocalAndRemote",
		}

		// Inject target clusters dynamically if configured
		if len(r.TargetClusters) > 0 {
			selector := corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{
				MatchExpressions: []corev1.NodeSelectorRequirement{{
					Key: "liqo.io/remote-cluster-id", Operator: corev1.NodeSelectorOpIn,
					Values: append([]string(nil), r.TargetClusters...),
				}},
			}}}
			selectorMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&selector)
			if err != nil {
				logger.Error(err, "Failed to convert Liqo cluster selector")
				return ctrl.Result{}, err
			}
			spec["clusterSelector"] = selectorMap
		}
		offloadCR.Object["spec"] = spec

		if err := r.Create(ctx, offloadCR); err != nil {
			logger.Error(err, "Failed to create NamespaceOffloading policy")
			return ctrl.Result{}, err
		}

		// SYNCHRONOUS CONTEXT-AWARE BACKOFF
		// Yield the thread briefly to allow Liqo's mutating webhooks to process the new CR.
		logger.Info("Policy created. Yielding thread to allow Liqo webhooks to register.", "namespace", namespace, "backoff", r.BackoffDuration)

		select {
		case <-time.After(r.BackoffDuration):
			// The backoff period has elapsed, proceed with pod deletion.
		case <-ctx.Done():
			// Kubernetes sent a shutdown signal during our wait, exit cleanly.
			logger.Info("Reconciliation cancelled by cluster during backoff")
			return ctrl.Result{}, ctx.Err()
		}

	} else if err != nil {
		logger.Error(err, "Failed to fetch NamespaceOffloading policy")
		return ctrl.Result{}, err
	} else if lastRemediation := offloadCR.GetAnnotations()[lastRemediationAnnotation]; lastRemediation != "" {
		// Prevent infinite loops by honoring a cool-down period between remediation attempts.
		remediationTime, parseErr := time.Parse(time.RFC3339Nano, lastRemediation)
		if parseErr == nil {
			remaining := r.BackoffDuration - time.Since(remediationTime)
			if remaining > 0 {
				logger.V(1).Info("Remediation backoff is active", "namespace", namespace, "remaining", remaining.Round(time.Millisecond))
				return ctrl.Result{RequeueAfter: remaining}, nil
			}
		}
	}

	// 5. Physical Pod Deletion (Only if still trapped after sleep)
	var latestPod corev1.Pod
	if err := r.Get(ctx, req.NamespacedName, &latestPod); err == nil {

		// Verify if Liqo's background processes already auto-recovered the pod during the pause.
		isStillTrapped := latestPod.Status.Reason == "OffloadingBackOff"
		for _, cs := range latestPod.Status.ContainerStatuses {
			if cs.State.Waiting != nil && cs.State.Waiting.Reason == "OffloadingBackOff" {
				isStillTrapped = true
			}
		}
		for _, cs := range latestPod.Status.InitContainerStatuses {
			if cs.State.Waiting != nil && cs.State.Waiting.Reason == "OffloadingBackOff" {
				isStillTrapped = true
			}
		}

		if !isStillTrapped {
			logger.Info("Pod successfully auto-recovered by Liqo during backoff. Skipping deletion.", "pod", podName)
			return ctrl.Result{}, nil
		}

		// If it is still stuck, aggressively delete it to force the ReplicaSet/Deployment to spawn a fresh one.
		deletePolicy := metav1.DeletePropagationBackground
		deleteOpts := &client.DeleteOptions{
			PropagationPolicy: &deletePolicy,
		}

		if err := r.Delete(ctx, &latestPod, deleteOpts); err != nil {
			logger.Error(err, "Failed to delete the stuck pod", "pod", podName)
			return ctrl.Result{}, err
		}

		if err := r.persistRemediationBackoff(ctx, namespace); err != nil {
			logger.Error(err, "Failed to persist remediation backoff", "namespace", namespace)
			return ctrl.Result{}, err
		}

		logger.Info("Successfully deleted the stuck pod. The controller will provision a replacement.", "pod", podName)
	} else if !apierrors.IsNotFound(err) {
		logger.Error(err, "Failed to fetch the latest pod status")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// Helper function to safely update the remediation timestamp using optimistic locking and retries.
func (r *LiqoTrapReconciler) persistRemediationBackoff(ctx context.Context, namespace string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest := &unstructured.Unstructured{}
		latest.SetGroupVersionKind(schema.GroupVersionKind{
			Group: "offloading.liqo.io", Version: "v1beta1", Kind: "NamespaceOffloading",
		})
		if err := r.Get(ctx, client.ObjectKey{Name: "offloading", Namespace: namespace}, latest); apierrors.IsNotFound(err) {
			return nil
		} else if err != nil {
			return err
		}

		annotations := latest.GetAnnotations()
		if annotations == nil {
			annotations = make(map[string]string)
		}
		annotations[lastRemediationAnnotation] = time.Now().UTC().Format(time.RFC3339Nano)
		latest.SetAnnotations(annotations)

		return r.Update(ctx, latest)
	})
}

// LiqoTrapPredicate allows all Create/Update events to pass through.
// The actual filtering (Early Exit Pattern) happens safely inside the Reconcile loop.
// Deletions are ignored as they require no remediation.
func LiqoTrapPredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool { return true },
		UpdateFunc: func(e event.UpdateEvent) bool { return true },
		DeleteFunc: func(e event.DeleteEvent) bool { return false },
	}
}

func (r *LiqoTrapReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Pod{}).
		WithEventFilter(LiqoTrapPredicate()).
		Complete(r)
}
