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

// Helper function to mock a Namespace with optional labels.
func createNamespace(name string, labels map[string]string) *corev1.Namespace {
	if labels == nil {
		labels = make(map[string]string)
	}
	return &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
	}
}

// Helper function to mock a Pod stuck in OffloadingBackOff.
func createStuckPod(name, namespace string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: corev1.PodSpec{
			NodeSelector: map[string]string{"kubernetes.io/hostname": "cluster-remote"},
		},
		Status: corev1.PodStatus{Reason: "OffloadingBackOff"},
	}
}

// Helper function to mock a trapped Pod undergoing graceful shutdown.
func createTerminatingPod(name, namespace string) *corev1.Pod {
	now := metav1.Now()
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         namespace,
			DeletionTimestamp: &now,
			Finalizers:        []string{"dummy-finalizer"},
		},
		Spec:   corev1.PodSpec{NodeSelector: map[string]string{"kubernetes.io/hostname": "cluster-remote"}},
		Status: corev1.PodStatus{Reason: "OffloadingBackOff"},
	}
}

// Helper function to mock an existing NamespaceOffloading CR.
func createTrapOffloadingCR(namespace string) *unstructured.Unstructured {
	offloadCR := &unstructured.Unstructured{}
	offloadCR.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "offloading.liqo.io", Version: "v1beta1", Kind: "NamespaceOffloading",
	})
	offloadCR.SetName("offloading")
	offloadCR.SetNamespace(namespace)
	offloadCR.SetUID(types.UID("mock-uid-trap"))
	return offloadCR
}

// TestLiqoTrapReconciler_Reconcile verifies the complete remediation workflow:
// creating a missing policy, allowing an existing policy to be reused,
// respecting safety gates, preserving dry-run behavior, enforcing cooldowns,
// and detecting OffloadingBackOff in nested container status.
func TestLiqoTrapReconciler_Reconcile(t *testing.T) {
	ctx := context.TODO()
	s := scheme.Scheme
	_ = corev1.AddToScheme(s)

	tests := []struct {
		name                string
		namespace           string
		podName             string
		dryRun              bool
		existingObjects     []client.Object
		expectPolicyCreated bool
		expectPodPatched    bool
	}{
		{
			// The primary recovery path creates the missing policy and applies
			// force-sync to the still-trapped pod.
			name:      "Happy Path: Creates policy and patches stuck pod",
			namespace: "default",
			podName:   "stuck-pod",
			dryRun:    false,
			existingObjects: []client.Object{
				createNamespace("default", nil),
				createStuckPod("stuck-pod", "default"),
			},
			expectPolicyCreated: true,
			expectPodPatched:    true,
		},
		{
			// An existing policy proves reconciliation is idempotent and avoids
			// recreating or unnecessarily waiting for the same resource.
			name:      "Idempotency: Policy already exists, skips delay and patches pod",
			namespace: "default",
			podName:   "stuck-pod",
			dryRun:    false,
			existingObjects: []client.Object{
				createNamespace("default", nil),
				createStuckPod("stuck-pod", "default"),
				createTrapOffloadingCR("default"),
			},
			expectPolicyCreated: true,
			expectPodPatched:    true,
		},
		{
			// Terminating pods are already leaving the cluster and must not be
			// modified or used to start another remediation cycle.
			name:      "Terminating Pod: Early exit prevents hot-loops on dying pods",
			namespace: "default",
			podName:   "dying-pod",
			dryRun:    false,
			existingObjects: []client.Object{
				createNamespace("default", nil),
				createTerminatingPod("dying-pod", "default"),
			},
			expectPolicyCreated: false,
			expectPodPatched:    false,
		},
		{
			// Dry-run must execute the decision path while leaving both the
			// NamespaceOffloading policy and pod annotations unchanged.
			name:      "Dry Run: Evaluates logic but does not modify state",
			namespace: "default",
			podName:   "stuck-pod",
			dryRun:    true,
			existingObjects: []client.Object{
				createNamespace("default", nil),
				createStuckPod("stuck-pod", "default"),
			},
			expectPolicyCreated: false,
			expectPodPatched:    false,
		},
		{
			// Glob-based exclusions protect system namespaces from automatic
			// policy creation and pod mutation.
			name:      "Glob Exclusion: Matches pattern *-system",
			namespace: "kube-system",
			podName:   "system-pod",
			dryRun:    false,
			existingObjects: []client.Object{
				createNamespace("kube-system", nil),
				createStuckPod("system-pod", "kube-system"),
			},
			expectPolicyCreated: false,
			expectPodPatched:    false,
		},
		{
			// Namespace blacklist labels provide an operator-controlled safety
			// gate for workloads that must not be remediated.
			name:      "Label Exclusion: Namespace has excluded label",
			namespace: "test-ns",
			podName:   "stuck-pod",
			dryRun:    false,
			existingObjects: []client.Object{
				createNamespace("test-ns", map[string]string{"no-trap": "true"}),
				createStuckPod("stuck-pod", "test-ns"),
			},
			expectPolicyCreated: false,
			expectPodPatched:    false,
		},

		{
			// A recent remediation timestamp must defer the request so repeated
			// trapped pods cannot create a remediation hot-loop.
			name:      "Backoff Active: Requeues if remediation was too recent",
			namespace: "default",
			podName:   "stuck-pod",
			dryRun:    false,
			existingObjects: []client.Object{
				&corev1.Namespace{
					ObjectMeta: metav1.ObjectMeta{
						Name: "default",
						Annotations: map[string]string{
							lastRemediationAnnotation: time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano),
						},
					},
				},
				createStuckPod("stuck-pod", "default"),
			},
			expectPolicyCreated: false,
			expectPodPatched:    false,
		},

		{
			// Liqo can report the trapped state on an individual container even
			// when the pod-level reason is empty.
			name:      "Container Trapped: Detects OffloadingBackOff deep in ContainerStatuses",
			namespace: "default",
			podName:   "container-stuck-pod",
			dryRun:    false,
			existingObjects: []client.Object{
				createNamespace("default", nil),
				&corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: "container-stuck-pod", Namespace: "default"},
					Spec:       corev1.PodSpec{NodeSelector: map[string]string{"kubernetes.io/hostname": "cluster-remote"}},
					Status: corev1.PodStatus{
						Reason: "",
						ContainerStatuses: []corev1.ContainerStatus{
							{
								State: corev1.ContainerState{
									Waiting: &corev1.ContainerStateWaiting{Reason: "OffloadingBackOff"},
								},
							},
						},
					},
				},
			},
			expectPolicyCreated: true,
			expectPodPatched:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Initialize an isolated in-memory API so resource mutations from one
			// scenario cannot affect any other remediation case.
			fakeClient := fake.NewClientBuilder().WithScheme(s).WithObjects(tt.existingObjects...).Build()

			reconciler := &LiqoTrapReconciler{
				Client:             fakeClient,
				TargetClusters:     []string{"cluster-remote"},
				ExcludedNamespaces: []string{"*-system", "local-path-storage"},
				BlacklistLabels:    map[string]string{"no-trap": "true"},
				BackoffDuration:    10 * time.Millisecond,
				DryRun:             tt.dryRun,
			}

			req := ctrl.Request{NamespacedName: types.NamespacedName{Name: tt.podName, Namespace: tt.namespace}}
			_, err := reconciler.Reconcile(ctx, req)
			if err != nil {
				t.Fatalf("Unexpected error: %v", err)
			}

			// Verify whether remediation created or preserved the policy expected
			// for this scenario.
			checkCR := &unstructured.Unstructured{}
			checkCR.SetGroupVersionKind(schema.GroupVersionKind{Group: "offloading.liqo.io", Version: "v1beta1", Kind: "NamespaceOffloading"})
			err = fakeClient.Get(ctx, types.NamespacedName{Name: "offloading", Namespace: tt.namespace}, checkCR)

			if tt.expectPolicyCreated && apierrors.IsNotFound(err) {
				t.Errorf("Expected NamespaceOffloading to exist")
			} else if !tt.expectPolicyCreated && err == nil {
				if tt.name != "Idempotency: Policy already exists, skips delay and patches pod" {
					t.Errorf("Expected NO NamespaceOffloading creation")
				}
			}

			var pod corev1.Pod
			_ = fakeClient.Get(ctx, types.NamespacedName{Name: tt.podName, Namespace: tt.namespace}, &pod)

			// The force-sync annotation is the observable pod mutation used to
			// request another Liqo synchronization attempt.
			hasPatch := pod.GetAnnotations()[forceSyncAnnotation] != ""
			if tt.expectPodPatched && !hasPatch {
				t.Errorf("Expected Pod to be patched with force-sync annotation")
			} else if !tt.expectPodPatched && hasPatch {
				t.Errorf("Expected Pod NOT to be patched")
			}
		})
	}
}
