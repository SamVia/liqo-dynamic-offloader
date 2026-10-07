package controllers

import (
	"context"
	"path/filepath"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// Metrics for Prometheus.
var (
	liqoCleanupNamespacesTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "liqo_cleanup_namespaces_total",
			Help: "Total number of namespace offloading policies successfully cleaned up",
		},
	)

	liqoCleanupActiveRemotePods = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "liqo_cleanup_active_remote_pods",
			Help: "Number of active or terminating pods relying on the remote cluster",
		},
		[]string{"namespace"},
	)
)

func init() {
	metrics.Registry.MustRegister(liqoCleanupNamespacesTotal, liqoCleanupActiveRemotePods)
}

// LiqoCleanupReconciler manages NamespaceOffloading resources based on active remote workloads.
type LiqoCleanupReconciler struct {
	client.Client
	TargetClusters     []string
	ExcludedNamespaces []string
	WhitelistLabels    map[string]string
	BlacklistLabels    map[string]string
	Recorder           record.EventRecorder
	CleanupDelay       time.Duration
	DryRun             bool
}

const emptySinceAnnotation = "dynamic-offloader.liqo.io/empty-since"

// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch
// +kubebuilder:rbac:groups=offloading.liqo.io,resources=namespaceoffloadings,verbs=get;list;watch;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile removes a NamespaceOffloading policy after the namespace has
// remained free of active remote workloads for the configured cleanup delay.
//
// The empty-since annotation persists the countdown in the API so the cleanup
// decision survives controller restarts and is based on observed cluster state
// rather than an in-memory timer.
func (r *LiqoCleanupReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	namespace := req.Namespace

	var ns corev1.Namespace
	if err := r.Get(ctx, client.ObjectKey{Name: namespace}, &ns); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !r.isNamespaceAllowed(&ns) {
		// Protected namespaces must not have their offloading policy changed.
		return ctrl.Result{}, nil
	}

	offloadCR := &unstructured.Unstructured{}
	offloadCR.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "offloading.liqo.io",
		Version: "v1beta1",
		Kind:    "NamespaceOffloading",
	})

	if err := r.Get(ctx, client.ObjectKey{Name: "offloading", Namespace: namespace}, offloadCR); err != nil {
		// Cleanup is already complete when the policy no longer exists.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	var podList corev1.PodList
	if err := r.List(ctx, &podList, client.InNamespace(namespace)); err != nil {
		logger.Error(err, "Failed to list pods in namespace")
		return ctrl.Result{}, err
	}

	// Count active pods relying on the remote cluster. Pending pods with an
	// explicit remote target are included because they still depend on the
	// NamespaceOffloading policy to complete scheduling.
	activeOffloadedPods := 0
	for _, pod := range podList.Items {
		explicitTarget := r.podTargetsAllowedCluster(ctx, &pod)
		scheduledRemote := r.isLiqoRemotePod(ctx, &pod)
		isPendingRemote := pod.Status.Phase == corev1.PodPending && pod.Spec.NodeName == "" && explicitTarget

		if explicitTarget || scheduledRemote || isPendingRemote {
			isTerminating := !pod.DeletionTimestamp.IsZero()
			isTerminalPhase := pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed

			// Terminating pods remain active until deletion completes, while
			// succeeded and failed pods no longer require remote placement.
			if isTerminating || !isTerminalPhase {
				activeOffloadedPods++
			}
		}
	}

	liqoCleanupActiveRemotePods.WithLabelValues(namespace).Set(float64(activeOffloadedPods))

	annotations := offloadCR.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string)
	}

	if activeOffloadedPods == 0 {
		if r.CleanupDelay == 0 {
			// A zero delay is an explicit operator choice to disable automatic
			// deletion while retaining metrics and reconciliation behavior.
			logger.Info("Cleanup delay is 0, offloading policy will never be deleted automatically.", "namespace", namespace)
			return ctrl.Result{}, nil
		}

		emptySinceStr, hasAnnotation := annotations[emptySinceAnnotation]
		if !hasAnnotation {
			// Start a durable countdown instead of deleting immediately. This
			// protects against short gaps between remote workload transitions.
			annotations[emptySinceAnnotation] = time.Now().UTC().Format(time.RFC3339)
			offloadCR.SetAnnotations(annotations)

			if !r.DryRun {
				if err := r.Update(ctx, offloadCR); err != nil {
					return ctrl.Result{}, err
				}
				logger.Info("Namespace is empty. Starting cleanup countdown.", "namespace", namespace, "delay", r.CleanupDelay)
			} else {
				logger.Info("[DRY RUN] Would set empty-since annotation", "namespace", namespace)
			}
			return ctrl.Result{RequeueAfter: r.CleanupDelay}, nil
		}

		emptySince, _ := time.Parse(time.RFC3339, emptySinceStr)
		if time.Since(emptySince) >= r.CleanupDelay {
			if !r.DryRun {
				// Delete only after the namespace has stayed empty for the full
				// delay, then emit a custom Kubernetes event.
				if err := r.Delete(ctx, offloadCR); client.IgnoreNotFound(err) != nil {
					logger.Error(err, "Failed to delete NamespaceOffloading")
					return ctrl.Result{}, err
				}
				logger.Info("Successfully deleted NamespaceOffloading. Namespace isolation restored.", "namespace", namespace)
				liqoCleanupNamespacesTotal.Inc()
				r.Recorder.Event(offloadCR, corev1.EventTypeNormal, "Successful Cleanup", "Namespace isolation restored: offloading policy revoked.")
			} else {
				// Dry-run preserves the countdown and reports the deletion that
				// would have occurred without changing cluster state.
				logger.Info("[DRY RUN] Would delete NamespaceOffloading", "namespace", namespace)
			}
		} else {
			return ctrl.Result{RequeueAfter: r.CleanupDelay - time.Since(emptySince)}, nil
		}

	} else {
		if _, hasAnnotation := annotations[emptySinceAnnotation]; hasAnnotation {
			// New remote work invalidates the countdown and keeps the policy in
			// place for subsequent scheduling and reconciliation.
			delete(annotations, emptySinceAnnotation)
			offloadCR.SetAnnotations(annotations)

			if !r.DryRun {
				if err := r.Update(ctx, offloadCR); err != nil {
					return ctrl.Result{}, err
				}
				logger.Info("New remote pods detected. Cancelled cleanup countdown.", "namespace", namespace)
			} else {
				logger.Info("[DRY RUN] Would remove empty-since annotation", "namespace", namespace)
			}
		}
	}

	return ctrl.Result{}, nil
}

// isNamespaceAllowed applies namespace exclusions and label policy before
// cleanup can mutate the NamespaceOffloading resource.
func (r *LiqoCleanupReconciler) isNamespaceAllowed(ns *corev1.Namespace) bool {
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

// checkRemoteNode verifies whether a scheduled node is a Liqo remote endpoint
// when no explicit target-cluster allowlist is configured.
func (r *LiqoCleanupReconciler) checkRemoteNode(ctx context.Context, nodeName string) bool {
	if nodeName == "" {
		return false
	}
	var node corev1.Node
	if err := r.Get(ctx, client.ObjectKey{Name: nodeName}, &node); err == nil {
		return isLiqoRemoteNode(node)
	}
	return false
}

// podTargetsAllowedCluster checks whether a pod explicitly targets an eligible
// remote cluster through its node selector.
func (r *LiqoCleanupReconciler) podTargetsAllowedCluster(ctx context.Context, pod *corev1.Pod) bool {
	if pod.Spec.NodeSelector == nil {
		return false
	}
	target := pod.Spec.NodeSelector["kubernetes.io/hostname"]
	if len(r.TargetClusters) > 0 {
		return r.clusterAllowed(target)
	}
	return r.checkRemoteNode(ctx, target) || strings.HasPrefix(target, "virtual-node-")
}

// clusterAllowed verifies that a cluster ID is in the configured allowlist.
func (r *LiqoCleanupReconciler) clusterAllowed(clusterID string) bool {
	for _, allowed := range r.TargetClusters {
		if allowed == clusterID {
			return true
		}
	}
	return false
}

// isLiqoRemotePod identifies pods associated with a Liqo remote cluster through
// Liqo metadata or remote scheduling information.
func (r *LiqoCleanupReconciler) isLiqoRemotePod(ctx context.Context, pod *corev1.Pod) bool {
	remoteClusterID := pod.Labels["liqo.io/remote-cluster-id"]
	if remoteClusterID == "" {
		remoteClusterID = pod.Annotations["liqo.io/remote-cluster-id"]
	}
	if len(r.TargetClusters) == 0 {
		return remoteClusterID != "" || r.checkRemoteNode(ctx, pod.Spec.NodeName) || strings.HasPrefix(pod.Spec.NodeName, "virtual-node-")
	}
	return r.clusterAllowed(remoteClusterID) || r.clusterAllowed(pod.Spec.NodeName) || r.clusterAllowed(pod.Spec.NodeSelector["kubernetes.io/hostname"])
}

// isLiqoRemoteNode identifies Liqo virtual nodes by metadata or name prefix.
func isLiqoRemoteNode(node corev1.Node) bool {
	return node.Labels["liqo.io/remote-cluster-id"] != "" || strings.HasPrefix(node.Name, "virtual-node-")
}

// LiqoCleanupPredicate filters events to pod lifecycle changes that can alter
// remote workload accounting and cleanup eligibility.
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
			return oldPod.Status.Phase != newPod.Status.Phase ||
				oldPod.Spec.NodeName != newPod.Spec.NodeName ||
				oldPod.DeletionTimestamp.IsZero() != newPod.DeletionTimestamp.IsZero()
		},
	}
}

// SetupWithManager registers the cleanup reconciler for pod events and applies
// the predicate before requests enter the controller work queue.
func (r *LiqoCleanupReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Pod{}).
		WithEventFilter(LiqoCleanupPredicate()).
		Complete(r)
}
