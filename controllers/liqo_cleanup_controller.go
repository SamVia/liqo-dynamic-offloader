package controllers

import (
	"context"
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

var (
	// liqoCleanupNamespacesTotal tracks the total number of offloading policies successfully revoked.
	liqoCleanupNamespacesTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "liqo_cleanup_namespaces_total",
			Help: "Total number of namespace offloading policies successfully cleaned up",
		},
	)

	// liqoCleanupActiveRemotePods tracks the current number of active remote pods per namespace.
	// We use a GaugeVec to label by namespace, avoiding global overwrite issues across different reconciliations.
	liqoCleanupActiveRemotePods = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "liqo_cleanup_active_remote_pods",
			Help: "Number of active or terminating pods relying on the remote cluster",
		},
		[]string{"namespace"},
	)
)

func init() {
	// Register custom metrics with the global controller-runtime metrics registry
	metrics.Registry.MustRegister(liqoCleanupNamespacesTotal, liqoCleanupActiveRemotePods)
}

type LiqoCleanupReconciler struct {
	client.Client
	TargetClusters     []string
	ExcludedNamespaces map[string]bool
	Recorder           record.EventRecorder
	GracePeriod        time.Duration
}

// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch
// +kubebuilder:rbac:groups=offloading.liqo.io,resources=namespaceoffloadings,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

func (r *LiqoCleanupReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	namespace := req.Namespace

	// Exclude system and critical namespaces to prevent accidental deletion of their offloading configurations.
	if r.ExcludedNamespaces[namespace] {
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

	// Check on policy age with a grace period to prevent deletion from Cleanup Controller before either liqo or the Trap Controller have kicked and rescheduled the pod
	policyAge := time.Since(offloadCR.GetCreationTimestamp().Time)
	if policyAge < r.GracePeriod {
		logger.V(1).Info("Policy is too young for cleanup. Respecting grace period.", "namespace", namespace, "age", policyAge.Round(time.Second))
		return ctrl.Result{RequeueAfter: r.GracePeriod}, nil
	}

	// Retrieve all Pods within the namespace to evaluate their current scheduling and execution status.
	var podList corev1.PodList
	if err := r.List(ctx, &podList, client.InNamespace(namespace)); err != nil {
		logger.Error(err, "Failed to list pods in namespace")
		return ctrl.Result{}, err
	}
	var nodeList corev1.NodeList
	if err := r.List(ctx, &nodeList); err != nil {
		logger.Error(err, "Failed to list nodes")
		return ctrl.Result{}, err
	}
	nodes := make(map[string]corev1.Node, len(nodeList.Items))
	for _, node := range nodeList.Items {
		nodes[node.Name] = node
	}

	// Calculate the number of active Pods that rely on the remote cluster.
	activeOffloadedPods := 0
	for _, pod := range podList.Items {
		// Condition A: The pod explicitly requests scheduling on the remote cluster via NodeSelector.
		explicitTarget := r.podTargetsAllowedCluster(&pod, nodes)

		// Condition B: The pod has already been scheduled and is running on the remote node.
		scheduledRemote := r.isLiqoRemotePod(&pod, nodes)

		// Condition C: The pod is pending scheduling specifically for the remote cluster.
		// This ensures we do not block cleanup for pending pods intended for local nodes.
		isPendingRemote := pod.Status.Phase == corev1.PodPending && pod.Spec.NodeName == "" && explicitTarget

		// If the pod is tied to the remote cluster, check if it's active or gracefully shutting down
		if explicitTarget || scheduledRemote || isPendingRemote {
			isTerminating := !pod.DeletionTimestamp.IsZero()
			isTerminalPhase := pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed

			// Consider the pod active if it's terminating (graceful shutdown) OR if it hasn't reached a terminal state
			if isTerminating || !isTerminalPhase {
				activeOffloadedPods++
			}
		}
	}

	// Update the Prometheus Gauge with the current count for this namespace
	liqoCleanupActiveRemotePods.WithLabelValues(namespace).Set(float64(activeOffloadedPods))

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

			// Increment the Prometheus Counter on successful deletion
			liqoCleanupNamespacesTotal.Inc()

			// Emit a native Kubernetes event to the cluster
			r.Recorder.Event(offloadCR, corev1.EventTypeNormal, "Successful Cleanup", "Namespace isolation restored: offloading policy revoked due to zero active remote pods.")
		}
	}

	return ctrl.Result{}, nil
}

func (r *LiqoCleanupReconciler) podTargetsAllowedCluster(pod *corev1.Pod, nodes map[string]corev1.Node) bool {
	if pod.Spec.NodeSelector == nil {
		return false
	}
	target := pod.Spec.NodeSelector["kubernetes.io/hostname"]
	if len(r.TargetClusters) > 0 {
		return r.clusterAllowed(target)
	}
	node, hasNode := nodes[target]
	return (hasNode && isLiqoRemoteNode(node)) || strings.HasPrefix(target, "virtual-node-")
}

func (r *LiqoCleanupReconciler) clusterAllowed(clusterID string) bool {
	for _, allowed := range r.TargetClusters {
		if allowed == clusterID {
			return true
		}
	}
	return false
}

func (r *LiqoCleanupReconciler) isLiqoRemotePod(pod *corev1.Pod, nodes map[string]corev1.Node) bool {
	node, hasNode := nodes[pod.Spec.NodeName]
	remoteClusterID := node.Labels["liqo.io/remote-cluster-id"]
	if remoteClusterID == "" {
		remoteClusterID = pod.Labels["liqo.io/remote-cluster-id"]
	}
	if remoteClusterID == "" {
		remoteClusterID = pod.Annotations["liqo.io/remote-cluster-id"]
	}
	if len(r.TargetClusters) == 0 {
		return remoteClusterID != "" || (hasNode && isLiqoRemoteNode(node)) || strings.HasPrefix(pod.Spec.NodeName, "virtual-node-")
	}
	return r.clusterAllowed(remoteClusterID) || r.clusterAllowed(pod.Spec.NodeName) || r.clusterAllowed(pod.Spec.NodeSelector["kubernetes.io/hostname"])
}

func isLiqoRemoteNode(node corev1.Node) bool {
	return node.Labels["liqo.io/remote-cluster-id"] != "" || strings.HasPrefix(node.Name, "virtual-node-")
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
			// Trigger reconciliation only if the Pod's Phase, assigned Node, or DeletionTimestamp changes.
			// Added DeletionTimestamp check to intercept pods entering graceful shutdown phase.
			return oldPod.Status.Phase != newPod.Status.Phase ||
				oldPod.Spec.NodeName != newPod.Spec.NodeName ||
				oldPod.DeletionTimestamp.IsZero() != newPod.DeletionTimestamp.IsZero()
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
