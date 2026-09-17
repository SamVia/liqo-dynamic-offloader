package controllers

import (
	"context"
	"testing"
	"time"

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

// Helper function to mock a Pod stuck in OffloadingBackOff state targeting a remote cluster.
func createStuckPod(name, namespace string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: corev1.PodSpec{
			NodeSelector: map[string]string{"kubernetes.io/hostname": "cluster-remote"},
		},
		Status: corev1.PodStatus{
			Reason: "OffloadingBackOff",
		},
	}
}

// Helper function to mock a pod that is undergoing graceful shutdown (terminating).
func createTerminatingPod(name, namespace string) *corev1.Pod {
	now := metav1.Now()
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         namespace,
			DeletionTimestamp: &now, // Simulates graceful shutdown
			Finalizers:        []string{"dummy-finalizer"},
		},
		Spec: corev1.PodSpec{
			NodeSelector: map[string]string{"kubernetes.io/hostname": "cluster-remote"},
		},
		Status: corev1.PodStatus{
			Reason: "OffloadingBackOff",
		},
	}
}

// Helper function to mock the NamespaceOffloading CR created by the trap reconciler.
func createTrapOffloadingCR(namespace string) *unstructured.Unstructured {
	offloadCR := &unstructured.Unstructured{}
	offloadCR.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "offloading.liqo.io",
		Version: "v1beta1",
		Kind:    "NamespaceOffloading",
	})
	offloadCR.SetName("offloading")
	offloadCR.SetNamespace(namespace)
	// Assigned a fake UID to pass validation checks
	offloadCR.SetUID(types.UID("mock-uid-trap"))
	return offloadCR
}

func TestLiqoTrapReconciler_Reconcile(t *testing.T) {
	ctx := context.TODO()

	// 1. Register core Kubernetes types to the test scheme
	s := scheme.Scheme
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("Failed to add corev1 to scheme: %v", err)
	}

	// 2. Define the trap matrix
	// This ensures we test stuck pods, idempotency, system namespaces, and terminating pods.
	tests := []struct {
		name                string
		namespace           string
		podName             string
		existingObjects     []client.Object // Pre-populate the cluster state
		expectPolicyCreated bool            // Should the policy be generated
		expectPodDeleted    bool            // Should the stuck pod be deleted
	}{
		{
			name:      "Happy Path: Creates policy, sleeps synchronously, deletes pod atomically",
			namespace: "default",
			podName:   "stuck-pod",
			existingObjects: []client.Object{
				createStuckPod("stuck-pod", "default"),
			},
			expectPolicyCreated: true,
			expectPodDeleted:    true,
		},
		{
			name:      "Idempotency: Policy already exists, skips delay and deletes pod immediately",
			namespace: "default",
			podName:   "stuck-pod",
			existingObjects: []client.Object{
				createStuckPod("stuck-pod", "default"),
				createTrapOffloadingCR("default"),
			},
			expectPolicyCreated: true,
			expectPodDeleted:    true,
		},
		{
			name:      "Excluded Namespace: System namespace is ignored entirely",
			namespace: "kube-system",
			podName:   "system-pod",
			existingObjects: []client.Object{
				createStuckPod("system-pod", "kube-system"),
			},
			expectPolicyCreated: false, // Critical systems must not be offloaded automatically
			expectPodDeleted:    false,
		},
		{
			name:      "Terminating Pod: Early exit prevents hot-loops on dying pods",
			namespace: "default",
			podName:   "dying-pod",
			existingObjects: []client.Object{
				createTerminatingPod("dying-pod", "default"),
			},
			expectPolicyCreated: false, // Dying pods shouldn't trigger new offloading policies
			expectPodDeleted:    false, // Wait for complete deletion organically
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

			reconciler := &LiqoTrapReconciler{
				Client:         fakeClient,
				TargetClusters: []string{"cluster-remote"}, // Dynamically injected parameter
				ExcludedNamespaces: map[string]bool{ // Dynamically injected parameter
					"kube-system":        true,
					"liqo-system":        true,
					"local-path-storage": true,
					"crownlabs-system":   true,
				},
				BackoffDuration: 2 * time.Second,
			}

			// Trigger the Reconcile loop targeting our stuck pod
			req := ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      tt.podName,
					Namespace: tt.namespace,
				},
			}

			res, err := reconciler.Reconcile(ctx, req)
			if err != nil {
				t.Fatalf("Reconcile returned an unexpected error: %v", err)
			}

			if res.RequeueAfter != 0 {
				t.Errorf("Expected RequeueAfter 0, got %v", res.RequeueAfter)
			}

			// 4. Assertions: Verify if the NamespaceOffloading was created and the pod deleted
			checkCR := &unstructured.Unstructured{}
			checkCR.SetGroupVersionKind(schema.GroupVersionKind{
				Group:   "offloading.liqo.io",
				Version: "v1beta1",
				Kind:    "NamespaceOffloading",
			})

			err = fakeClient.Get(ctx, types.NamespacedName{Name: "offloading", Namespace: tt.namespace}, checkCR)
			if tt.expectPolicyCreated && apierrors.IsNotFound(err) {
				t.Errorf("Expected NamespaceOffloading to exist, but it was not found")
			} else if !tt.expectPolicyCreated && err == nil {
				t.Errorf("Expected NO NamespaceOffloading, but one was created")
			}

			err = fakeClient.Get(ctx, types.NamespacedName{Name: tt.podName, Namespace: tt.namespace}, &corev1.Pod{})
			if tt.expectPodDeleted && err == nil {
				t.Errorf("Expected Pod to be deleted, but it still exists")
			} else if !tt.expectPodDeleted && apierrors.IsNotFound(err) {
				t.Errorf("Expected Pod to remain, but it was deleted")
			}
		})
	}
}
