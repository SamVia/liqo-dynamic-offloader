/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// PodReconciler reconciles a Pod object
type PodReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=pods/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core,resources=pods/finalizers,verbs=update
// +kubebuilder:rbac:groups=core,resources=namespaces,verbs=get;list;watch
// +kubebuilder:rbac:groups=offloading.liqo.io,resources=namespaceoffloadings,verbs=get;list;watch;create;update;patch

// Reconcile is part of the main kubernetes reconciliation loop.
func (r *PodReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// 1. Recuperiamo il Pod dal cluster
	var pod corev1.Pod
	if err := r.Get(ctx, req.NamespacedName, &pod); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Se il pod è già in fase di terminazione, lo ignoriamo
	if !pod.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	// 2. Controllo Whitelisting: Recuperiamo il Namespace del Pod
	var ns corev1.Namespace
	if err := r.Get(ctx, client.ObjectKey{Name: pod.Namespace}, &ns); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Se il namespace non ha la label, interrompiamo subito (Whitelisting a posteriori)
	if ns.Labels["liqo-helper.io/monitor"] != "true" {
		return ctrl.Result{}, nil
	}

	// 3. Analizziamo lo status del Pod a tutti i livelli (spesso i BackOff sono nei container)
	isOffloadingBackOff := false
	if pod.Status.Reason == "OffloadingBackOff" {
		isOffloadingBackOff = true
	} else {
		for _, containerStatus := range pod.Status.ContainerStatuses {
			if containerStatus.State.Waiting != nil && containerStatus.State.Waiting.Reason == "OffloadingBackOff" {
				isOffloadingBackOff = true
				break
			}
		}
		// Controllo anche gli init containers per sicurezza
		for _, initStatus := range pod.Status.InitContainerStatuses {
			if initStatus.State.Waiting != nil && initStatus.State.Waiting.Reason == "OffloadingBackOff" {
				isOffloadingBackOff = true
				break
			}
		}
	}

	// 4. Se troviamo l'errore, eseguiamo la remediation
	if isOffloadingBackOff {
		log.Info("TRIGGER FAIL-FAST: Pod in OffloadingBackOff intercettato!",
			"pod", pod.Name,
			"namespace", pod.Namespace,
		)

		// Exclude critical system namespaces
		if pod.Namespace == "kube-system" || pod.Namespace == "liqo-system" || pod.Namespace == "local-path-storage" || pod.Namespace == "crownlabs-system" {
			log.Info("DEBUG: System namespace ignored", "namespace", pod.Namespace)
			return ctrl.Result{}, nil
		}

		log.Info("DEBUG: Attempting to create NamespaceOffloading...")

		// Construct the unstructured NamespaceOffloading Custom Resource
		offloadCR := &unstructured.Unstructured{}
		offloadCR.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   "offloading.liqo.io",
			Version: "v1beta1",
			Kind:    "NamespaceOffloading",
		})
		offloadCR.SetName("offloading")
		offloadCR.SetNamespace(pod.Namespace)
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
				log.Info("NamespaceOffloading already exists. Proceeding to delete the trapped pod.", "namespace", pod.Namespace)
			} else {
				// ERROR CAPTURE: This will print exactly why the API server rejected the payload
				log.Error(err, "DEBUG: Failed to create NamespaceOffloading policy! Check RBAC or Schema.")
				return ctrl.Result{}, err
			}
		} else {
			log.Info("Successfully applied NamespaceOffloading. Requeueing to allow webhook registration.", "namespace", pod.Namespace)
			return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
		}

		// Delete the trapped pod.
		deletePolicy := metav1.DeletePropagationBackground
		deleteOpts := &client.DeleteOptions{
			PropagationPolicy: &deletePolicy,
		}

		log.Info("DEBUG: Attempting to delete stuck pod...")
		if err := r.Delete(ctx, &pod, deleteOpts); err != nil {
			log.Error(err, "Failed to delete the stuck pod", "pod", pod.Name)
			return ctrl.Result{}, err
		}
		log.Info("Successfully deleted the stuck pod. The managing controller should provision a replacement.", "pod", pod.Name)
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *PodReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Pod{}).
		WithEventFilter(predicate.ResourceVersionChangedPredicate{}).
		Named("pod").
		Complete(r)
}
