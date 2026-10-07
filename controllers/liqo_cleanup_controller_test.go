package controllers

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// Helper function to mock a Namespace used by the cleanup reconciler.
func createCleanupNamespace(name string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
}

// Helper function to mock the NamespaceOffloading CR with an optional timestamp.
func createCleanupOffloadingCR(namespace string, emptySince string) *unstructured.Unstructured {
	cr := &unstructured.Unstructured{}
	cr.SetGroupVersionKind(schema.GroupVersionKind{Group: "offloading.liqo.io", Version: "v1beta1", Kind: "NamespaceOffloading"})
	cr.SetName("offloading")
	cr.SetNamespace(namespace)
	if emptySince != "" {
		cr.SetAnnotations(map[string]string{emptySinceAnnotation: emptySince})
	}
	return cr
}

// TestLiqoCleanupReconciler_Reconcile verifies the cleanup state machine:
// starting and cancelling the durable empty-namespace countdown, deleting an
// expired policy, and retaining policies while remote work is still active.
func TestLiqoCleanupReconciler_Reconcile(t *testing.T) {
	ctx := context.TODO()
	s := scheme.Scheme
	_ = corev1.AddToScheme(s)

	tests := []struct {
		name            string
		namespace       string
		existingObjects []client.Object
		expectState     string // "annotated" starts the countdown; "deleted" completes cleanup;
		// "cleared" cancels the countdown; "unchanged" preserves state.
	}{
		{
			name:      "Step 1: Zero active pods triggers empty-since annotation",
			namespace: "default",
			existingObjects: []client.Object{
				createCleanupNamespace("default"),
				createCleanupOffloadingCR("default", ""),
			},
			expectState: "annotated",
		},
		{
			name:      "Step 2: Timeout elapsed, policy is deleted",
			namespace: "default",
			existingObjects: []client.Object{
				createCleanupNamespace("default"),
				createCleanupOffloadingCR("default", time.Now().Add(-2*time.Hour).UTC().Format(time.RFC3339)),
			},
			expectState: "deleted",
		},
		{
			name:      "Abort: Pod created during timeout clears annotation",
			namespace: "demo-ns",
			existingObjects: []client.Object{
				createCleanupNamespace("demo-ns"),
				createCleanupOffloadingCR("demo-ns", time.Now().UTC().Format(time.RFC3339)),
				&corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: "new-pod", Namespace: "demo-ns"},
					Spec:       corev1.PodSpec{NodeName: "cluster-remote"},
					Status:     corev1.PodStatus{Phase: corev1.PodRunning},
				},
			},
			expectState: "cleared",
		},
		{
			name:      "Blocking: Pending pod targeting remote cluster prevents cleanup and clears annotation",
			namespace: "demo-ns",
			existingObjects: []client.Object{
				createCleanupNamespace("demo-ns"),
				createCleanupOffloadingCR("demo-ns", time.Now().UTC().Format(time.RFC3339)),
				&corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: "pending-remote-pod", Namespace: "demo-ns"},
					Spec:       corev1.PodSpec{NodeSelector: map[string]string{"kubernetes.io/hostname": "cluster-remote"}},
					Status:     corev1.PodStatus{Phase: corev1.PodPending},
				},
			},
			expectState: "cleared",
		},
		{
			name:      "Graceful Shutdown: Terminating pod blocks cleanup",
			namespace: "demo-ns",
			existingObjects: []client.Object{
				createCleanupNamespace("demo-ns"),
				createCleanupOffloadingCR("demo-ns", ""),
				&corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:              "terminating-pod",
						Namespace:         "demo-ns",
						DeletionTimestamp: &metav1.Time{Time: time.Now()},
						Finalizers:        []string{"dummy"},
					},
					Spec:   corev1.PodSpec{NodeName: "cluster-remote"},
					Status: corev1.PodStatus{Phase: corev1.PodRunning},
				},
			},
			expectState: "unchanged",
		},
		{
			name:      "Local Pods: Running local pod does NOT block cleanup",
			namespace: "demo-ns",
			existingObjects: []client.Object{
				createCleanupNamespace("demo-ns"),
				createCleanupOffloadingCR("demo-ns", ""),
				&corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: "local-pod", Namespace: "demo-ns"},
					Spec:       corev1.PodSpec{NodeName: "cluster-local-worker"},
					Status:     corev1.PodStatus{Phase: corev1.PodRunning},
				},
			},
			expectState: "annotated", // It behaves as if there are zero remote pods
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Initialize an isolated in-memory API so each scenario validates
			// only its own resource state and reconciliation transition.
			fakeClient := fake.NewClientBuilder().WithScheme(s).WithObjects(tt.existingObjects...).Build()

			reconciler := &LiqoCleanupReconciler{
				Client:             fakeClient,
				TargetClusters:     []string{"cluster-remote"},
				ExcludedNamespaces: []string{"*-system"},
				Recorder:           record.NewFakeRecorder(100),
				CleanupDelay:       10 * time.Minute,
				DryRun:             false,
			}

			req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: tt.namespace}}
			_, err := reconciler.Reconcile(ctx, req)
			if err != nil {
				t.Fatalf("Unexpected error: %v", err)
			}

			checkCR := &unstructured.Unstructured{}
			checkCR.SetGroupVersionKind(schema.GroupVersionKind{Group: "offloading.liqo.io", Version: "v1beta1", Kind: "NamespaceOffloading"})
			err = fakeClient.Get(ctx, types.NamespacedName{Name: "offloading", Namespace: tt.namespace}, checkCR)

			switch tt.expectState {
			case "deleted":
				// Expired empty namespaces must lose their offloading policy.
				if err == nil {
					t.Errorf("Expected NamespaceOffloading to be deleted")
				}
			case "annotated":
				// The first empty observation starts the durable countdown.
				if err != nil {
					t.Fatalf("Expected NamespaceOffloading to exist, got error: %v", err)
				}
				if checkCR.GetAnnotations()[emptySinceAnnotation] == "" {
					t.Errorf("Expected empty-since annotation to be set")
				}
			case "cleared":
				// New remote work cancels a previously started countdown.
				if err != nil {
					t.Fatalf("Expected NamespaceOffloading to exist")
				}
				if checkCR.GetAnnotations()[emptySinceAnnotation] != "" {
					t.Errorf("Expected empty-since annotation to be cleared")
				}
			case "unchanged":
				// Terminating remote work must prevent cleanup from progressing.
				if err != nil {
					t.Fatalf("Expected NamespaceOffloading to exist")
				}
				if checkCR.GetAnnotations()[emptySinceAnnotation] != "" {
					t.Errorf("Expected annotation NOT to be set")
				}
			}
		})
	}
}
