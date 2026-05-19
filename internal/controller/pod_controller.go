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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
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

// Reconcile is part of the main kubernetes reconciliation loop.
func (r *PodReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// 1. Recuperiamo il Pod dal cluster
	var pod corev1.Pod
	if err := r.Get(ctx, req.NamespacedName, &pod); err != nil {
		// Ignoriamo gli errori NotFound (es. il pod è stato appena cancellato)
		return ctrl.Result{}, client.IgnoreNotFound(err)
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

	// 3. Analizziamo lo status globale del Pod (non dei container, perché il pod non è ancora schedulato!)
	isOffloadingBackOff := pod.Status.Reason == "OffloadingBackOff"

	// 4. Se troviamo l'errore, stampiamo il log ad alta precisione!
	if isOffloadingBackOff {
		log.Info("TRIGGER FAIL-FAST: Pod in OffloadingBackOff intercettato!",
			"pod", pod.Name,
			"namespace", pod.Namespace,
		)
		// TODO: Qui in futuro creeremo la risorsa NamespaceOffloading
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *PodReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Pod{}).
		// Usiamo un Predicate per ignorare tutti gli eventi in cui lo status del Pod non è cambiato.
		// Questo taglia drasticamente il traffico per eventi inutili.
		WithEventFilter(predicate.ResourceVersionChangedPredicate{}).
		Named("pod").
		Complete(r)
}
