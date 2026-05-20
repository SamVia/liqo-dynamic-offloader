package controllers

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// Helper function to mock the NamespaceOffloading CR that the controller attempts to clean up.
func createCleanupOffloadingCR(namespace string) *unstructured.Unstructured {
	offloadCR := &unstructured.Unstructured{}
	offloadCR.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "offloading.liqo.io",
		Version: "v1beta1",
		Kind:    "NamespaceOffloading",
	})
	offloadCR.SetName("offloading")
	offloadCR.SetNamespace(namespace)
	// Assigned a fake UID to pass the controller's deletion safety check
	offloadCR.SetUID(types.UID("mock-uid-cleanup"))
	return offloadCR
}

func TestLiqoCleanupReconciler_Reconcile(t *testing.T) {
	ctx := context.TODO()

	// 1. Register core Kubernetes types to the test scheme
	s := scheme.Scheme
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("Failed to add corev1 to scheme: %v", err)
	}

	// 2. Define the cleanup matrix
	// This ensures we test active, pending, terminal, and local pod permutations.
	tests := []struct {
		name            string
		namespace       string
		existingObjects []client.Object // Pre-populate the cluster state
		expectCleanup   bool            // Should the policy be deleted?
	}{
		{
			name:      "Happy Path: Zero active pods, policy is deleted",
			namespace: "default",
			existingObjects: []client.Object{
				createCleanupOffloadingCR("default"),
			},
			expectCleanup: true,
		},
		{
			name:      "Excluded Namespace: kube-system is ignored even with zero pods",
			namespace: "kube-system",
			existingObjects: []client.Object{
				createCleanupOffloadingCR("kube-system"),
			},
			expectCleanup: false, // Critical systems must maintain their settings
		},
		{
			name:      "Blocking: Pod running on remote cluster prevents cleanup",
			namespace: "demo-ns",
			existingObjects: []client.Object{
				createCleanupOffloadingCR("demo-ns"),
				&corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: "remote-pod", Namespace: "demo-ns"},
					Spec:       corev1.PodSpec{NodeName: "cluster-remote"},
					Status:     corev1.PodStatus{Phase: corev1.PodRunning},
				},
			},
			expectCleanup: false,
		},
		{
			name:      "Blocking: Pending pod targeting remote cluster prevents cleanup",
			namespace: "demo-ns",
			existingObjects: []client.Object{
				createCleanupOffloadingCR("demo-ns"),
				&corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: "pending-remote-pod", Namespace: "demo-ns"},
					Spec: corev1.PodSpec{
						NodeSelector: map[string]string{"kubernetes.io/hostname": "cluster-remote"},
					},
					Status: corev1.PodStatus{Phase: corev1.PodPending}, // Note: NodeName is empty here
				},
			},
			expectCleanup: false, // Pending pods still need the policy to eventually schedule
		},
		{
			name:      "Terminal Pods: Succeeded remote pod does NOT block cleanup",
			namespace: "demo-ns",
			existingObjects: []client.Object{
				createCleanupOffloadingCR("demo-ns"),
				&corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: "completed-pod", Namespace: "demo-ns"},
					Spec:       corev1.PodSpec{NodeName: "cluster-remote"},
					Status:     corev1.PodStatus{Phase: corev1.PodSucceeded},
				},
			},
			expectCleanup: true, // Completed pods don't need active offloading
		},
		{
			name:      "Local Pods: Running local pod does NOT block cleanup",
			namespace: "demo-ns",
			existingObjects: []client.Object{
				createCleanupOffloadingCR("demo-ns"),
				&corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: "local-pod", Namespace: "demo-ns"},
					Spec:       corev1.PodSpec{NodeName: "cluster-local-worker"},
					Status:     corev1.PodStatus{Phase: corev1.PodRunning},
				},
			},
			expectCleanup: true, // Local workloads don't care if the remote policy is revoked
		},
	}

	// 3. Run the Scenarios
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Initialize the fake client running entirely in-memory
			fakeClient := fake.NewClientBuilder().
				WithScheme(s).
				WithObjects(tt.existingObjects...).
				Build()

			reconciler := &LiqoCleanupReconciler{
				Client: fakeClient,
			}

			// Trigger the Reconcile loop targeting our namespace
			req := ctrl.Request{
				NamespacedName: types.NamespacedName{
					Namespace: tt.namespace,
				},
			}

			_, err := reconciler.Reconcile(ctx, req)
			if err != nil {
				t.Fatalf("Reconcile returned an unexpected error: %v", err)
			}

			// 4. Assertions: Verify if the NamespaceOffloading was successfully cleaned up
			checkCR := &unstructured.Unstructured{}
			checkCR.SetGroupVersionKind(schema.GroupVersionKind{
				Group:   "offloading.liqo.io",
				Version: "v1beta1",
				Kind:    "NamespaceOffloading",
			})

			err = fakeClient.Get(ctx, types.NamespacedName{Name: "offloading", Namespace: tt.namespace}, checkCR)

			if tt.expectCleanup {
				if err == nil {
					t.Errorf("Expected NamespaceOffloading to be deleted, but it still exists")
				} else if !apierrors.IsNotFound(err) {
					t.Errorf("Expected a NotFound error, but got: %v", err)
				}
			} else {
				if err != nil {
					t.Errorf("Expected NamespaceOffloading to persist, but got error (was it deleted?): %v", err)
				}
			}
		})
	}
}
