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

// LiqoTrapReconciler reconciles a Liqo Pod trap event
type LiqoTrapReconciler struct {
	client.Client
}

// +kubebuilder:rbac:groups="",resources=events,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups=offloading.liqo.io,resources=namespaceoffloadings,verbs=get;list;watch;create;update;patch

func (r *LiqoTrapReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// 1. Fetch the Event
	var evt corev1.Event
	if err := r.Get(ctx, req.NamespacedName, &evt); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	namespace := evt.InvolvedObject.Namespace
	podName := evt.InvolvedObject.Name

	// 2. Ignore critical system namespaces
	if namespace == "kube-system" || namespace == "liqo-system" || namespace == "local-path-storage" || namespace == "crownlabs-system" {
		return ctrl.Result{}, nil
	}

	logger.Info("🚨 ERROR DETECTED: Missing NamespaceOffloading! Initiating Auto-Heal...",
		"namespace", namespace,
		"pod", podName,
	)

	// 3. Construct the unstructured NamespaceOffloading CR
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

	// 4. Apply the Offloading Policy
	err := r.Create(ctx, offloadCR)
	if err != nil {
		if apierrors.IsAlreadyExists(err) {
			logger.Info("⚠️ NamespaceOffloading already exists. Proceeding to pod kick...", "namespace", namespace)
		} else {
			logger.Error(err, "❌ Failed to create NamespaceOffloading policy")
			return ctrl.Result{}, err
		}
	} else {
		// THE FIX: Requeue instead of time.Sleep
		// We successfully created the policy. We now put this event back in the queue
		// to give Liqo's mutating webhooks time to register the new policy before we delete the pod.
		logger.Info("✅ SUCCESS: Auto-applied NamespaceOffloading! Requeueing to allow webhook registration...", "namespace", namespace)
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}

	// 5. Kick the trapped pod (Only reached if the policy already existed or we just came back from a Requeue)
	var stuckPod corev1.Pod
	if err := r.Get(ctx, client.ObjectKey{Name: podName, Namespace: namespace}, &stuckPod); err == nil {

		// Production Best Practice: Use Background Deletion so the API server doesn't hang our client
		deletePolicy := metav1.DeletePropagationBackground
		deleteOpts := &client.DeleteOptions{
			PropagationPolicy: &deletePolicy,
		}

		if err := r.Delete(ctx, &stuckPod, deleteOpts); err != nil {
			logger.Error(err, "❌ Failed to kick the stuck pod", "pod", podName)
			// Return error to trigger a rapid retry
			return ctrl.Result{}, err
		}
		logger.Info("♻️ Kicked the stuck pod. The Deployment/ReplicaSet will now spawn a new one!", "pod", podName)
	} else if !apierrors.IsNotFound(err) {
		logger.Error(err, "❌ Failed to fetch the stuck pod for deletion", "pod", podName)
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *LiqoTrapReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// THE FIX: Filter events at the source to prevent the controller from processing the cluster's entire event firehose.
	liqoTrapFilter := predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			evt, ok := e.Object.(*corev1.Event)
			if !ok {
				return false
			}
			return evt.Reason == "ReflectionDisabled" && evt.InvolvedObject.Kind == "Pod"
		},
		// We generally don't care about updates or deletes for this specific trap logic
		UpdateFunc: func(e event.UpdateEvent) bool { return false },
		DeleteFunc: func(e event.DeleteEvent) bool { return false },
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Event{}).
		WithEventFilter(liqoTrapFilter).
		Complete(r)
}