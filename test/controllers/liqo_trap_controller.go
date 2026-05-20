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

// LiqoTrapPredicate optimizes the controller by filtering events at the source.
// This prevents the controller from processing the entire cluster event stream,
// focusing exclusively on the specific reflection disablement trap condition.
// Exposed publicly to allow for unit testing.
func LiqoTrapPredicate() predicate.Predicate {
	return predicate.Funcs{
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
}

// SetupWithManager sets up the controller with the Manager.
func (r *LiqoTrapReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Event{}).
		WithEventFilter(LiqoTrapPredicate()). // Uses the extracted predicate
		Complete(r)
}
