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

// Helper function to mock the Virtual Kubelet rejection event
func createTrapEvent(name, namespace, podName string) *corev1.Event {
	return &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		InvolvedObject: corev1.ObjectReference{
			Kind:      "Pod",
			Namespace: namespace,
			Name:      podName,
		},
		Reason: "ReflectionDisabled",
	}
}

// Helper function to mock the stuck/trapped Pod
func createStuckPod(name, namespace string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: corev1.PodSpec{
			NodeSelector: map[string]string{"kubernetes.io/hostname": "cluster-remote"},
		},
	}
}

// Helper function to mock a pre-existing offloading policy for idempotency checks
func createTrapOffloadingCR(namespace string) *unstructured.Unstructured {
	offloadCR := &unstructured.Unstructured{}
	offloadCR.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "offloading.liqo.io",
		Version: "v1beta1",
		Kind:    "NamespaceOffloading",
	})
	offloadCR.SetName("offloading")
	offloadCR.SetNamespace(namespace)
	// We assign a mock UID to satisfy the fake client API requirements
	offloadCR.SetUID(types.UID("mock-uid-trap"))
	return offloadCR
}

func TestLiqoTrapReconciler_Reconcile(t *testing.T) {
	ctx := context.TODO()

	// 1. Register core Kubernetes types (like Pods and Events) to the test scheme
	s := scheme.Scheme
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("Failed to add corev1 to scheme: %v", err)
	}

	// 2. Define our Table-Driven Test Scenarios
	// This structure allows us to easily add new edge cases without rewriting boilerplate.
	tests := []struct {
		name                string
		eventName           string
		namespace           string
		podName             string
		existingObjects     []client.Object // What exists in the cluster before reconcile?
		expectPolicyCreated bool            // Should a new policy be created?
		expectPodDeleted    bool            // Should the pod be deleted?
		expectRequeueAfter  time.Duration   // Do we expect the controller to pause and requeue?
	}{
		{
			name:      "Happy Path: Creates policy and requeues",
			eventName: "trap-event",
			namespace: "default",
			podName:   "stuck-pod",
			existingObjects: []client.Object{
				createTrapEvent("trap-event", "default", "stuck-pod"),
				createStuckPod("stuck-pod", "default"),
			},
			expectPolicyCreated: true,
			expectPodDeleted:    false, // We wait for the requeue before deleting the pod
			expectRequeueAfter:  2 * time.Second,
		},
		{
			name:      "Idempotency: Policy exists, skips to pod deletion",
			eventName: "trap-event",
			namespace: "default",
			podName:   "stuck-pod",
			existingObjects: []client.Object{
				createTrapEvent("trap-event", "default", "stuck-pod"),
				createStuckPod("stuck-pod", "default"),
				createTrapOffloadingCR("default"), // Simulate the policy already existing
			},
			expectPolicyCreated: true, // It should still be there
			expectPodDeleted:    true, // Controller should immediately delete the pod
			expectRequeueAfter:  0,
		},
		{
			name:      "Excluded Namespace: kube-system is ignored entirely",
			eventName: "system-trap",
			namespace: "kube-system",
			podName:   "system-pod",
			existingObjects: []client.Object{
				createTrapEvent("system-trap", "kube-system", "system-pod"),
				createStuckPod("system-pod", "kube-system"),
			},
			expectPolicyCreated: false, // Core systems should never be auto-offloaded
			expectPodDeleted:    false,
			expectRequeueAfter:  0,
		},
		{
			name:      "Missing Pod: Handles gracefully without erroring",
			eventName: "trap-event",
			namespace: "default",
			podName:   "ghost-pod",
			existingObjects: []client.Object{
				createTrapEvent("trap-event", "default", "ghost-pod"),
				createTrapOffloadingCR("default"),
				// Note: The pod itself is explicitly omitted from the fake cluster here
			},
			expectPolicyCreated: true,
			expectPodDeleted:    true, // Treating a missing pod as a successful deletion
			expectRequeueAfter:  0,
		},
	}

	// 3. Execute the Scenarios
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Initialize the Fake Client with our mocked objects for this specific test
			fakeClient := fake.NewClientBuilder().
				WithScheme(s).
				WithObjects(tt.existingObjects...).
				Build()

			// Instantiate the Reconciler
			reconciler := &LiqoTrapReconciler{Client: fakeClient}

			// Trigger the Reconcile loop exactly as the manager would
			req := ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      tt.eventName,
					Namespace: tt.namespace,
				},
			}

			res, err := reconciler.Reconcile(ctx, req)
			if err != nil {
				t.Fatalf("Reconcile returned an unexpected error: %v", err)
			}

			// 4. Assertions: Requeue Behavior
			if res.RequeueAfter != tt.expectRequeueAfter {
				t.Errorf("Expected RequeueAfter %v, got %v", tt.expectRequeueAfter, res.RequeueAfter)
			}

			// 5. Assertions: Verify NamespaceOffloading Creation
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

			// 6. Assertions: Verify Pod Deletion
			err = fakeClient.Get(ctx, types.NamespacedName{Name: tt.podName, Namespace: tt.namespace}, &corev1.Pod{})
			if tt.expectPodDeleted && err == nil {
				t.Errorf("Expected Pod to be deleted, but it still exists")
			} else if !tt.expectPodDeleted && apierrors.IsNotFound(err) {
				t.Errorf("Expected Pod to remain, but it was deleted")
			}
		})
	}
}
