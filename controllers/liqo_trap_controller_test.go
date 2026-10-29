package controllers

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
			// Fail-fast test: the namespace does not even exist in the client,
			// proving r.Get is never called for excluded namespaces.
			name:      "Glob Exclusion: Fail-fast bypasses API server Get",
			namespace: "test-system",
			podName:   "system-pod",
			dryRun:    false,
			existingObjects: []client.Object{
				createStuckPod("system-pod", "test-system"),
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
			result, err := reconciler.Reconcile(ctx, req)
			if tt.dryRun && result.RequeueAfter > 0 {
				// The policy never exists in dry-run, so a requeue would loop forever.
				t.Errorf("Dry-run must not requeue, got RequeueAfter=%v", result.RequeueAfter)
			}
			if err == nil && result.RequeueAfter > 0 {
				// A newly created policy is handled on the next reconciliation,
				// after the controller yields instead of blocking its worker.
				_, err = reconciler.Reconcile(ctx, req)
			}
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

// TestLiqoTrapController_Metrics verifies that the Prometheus counter for
// NamespaceOffloading creations increments when a missing policy is created.
func TestLiqoTrapController_Metrics(t *testing.T) {
	ctx := context.TODO()
	s := scheme.Scheme
	_ = corev1.AddToScheme(s)

	initialCount := testutil.ToFloat64(liqoTrapOffloadingsCreatedTotal)

	fakeClient := fake.NewClientBuilder().WithScheme(s).WithObjects(
		createNamespace("metric-ns", nil),
		createStuckPod("stuck-pod", "metric-ns"),
	).Build()

	reconciler := &LiqoTrapReconciler{
		Client:          fakeClient,
		BackoffDuration: 1 * time.Millisecond,
		DryRun:          false,
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "stuck-pod", Namespace: "metric-ns"}}
	if _, err := reconciler.Reconcile(ctx, req); err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}

	newCount := testutil.ToFloat64(liqoTrapOffloadingsCreatedTotal)
	if newCount != initialCount+1 {
		t.Errorf("Expected liqo_trap_offloadings_created_total to increase by 1, got %v (from %v)", newCount, initialCount)
	}
}

// TestLiqoTrapReconciler_AllowedNamespaces verifies inclusion filtering using globbing.
func TestLiqoTrapReconciler_AllowedNamespaces(t *testing.T) {
	ctx := context.TODO()
	s := scheme.Scheme
	_ = corev1.AddToScheme(s)

	tests := []struct {
		name                string
		namespace           string
		allowedPatterns     []string
		expectPolicyCreated bool
	}{
		{
			name:                "Matches allowed pattern tenant-*",
			namespace:           "tenant-app",
			allowedPatterns:     []string{"tenant-*"},
			expectPolicyCreated: true,
		},
		{
			name:                "Fails to match allowed pattern tenant-* (fail-fast)",
			namespace:           "other-app",
			allowedPatterns:     []string{"tenant-*"},
			expectPolicyCreated: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeClient := fake.NewClientBuilder().WithScheme(s).WithObjects(
				createNamespace(tt.namespace, nil),
				createStuckPod("stuck-pod", tt.namespace),
			).Build()

			reconciler := &LiqoTrapReconciler{
				Client:            fakeClient,
				AllowedNamespaces: tt.allowedPatterns,
				BackoffDuration:   time.Millisecond,
			}

			req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "stuck-pod", Namespace: tt.namespace}}
			if _, err := reconciler.Reconcile(ctx, req); err != nil {
				t.Fatalf("Unexpected error: %v", err)
			}

			checkCR := &unstructured.Unstructured{}
			checkCR.SetGroupVersionKind(schema.GroupVersionKind{Group: "offloading.liqo.io", Version: "v1beta1", Kind: "NamespaceOffloading"})
			err := fakeClient.Get(ctx, types.NamespacedName{Name: "offloading", Namespace: tt.namespace}, checkCR)

			if tt.expectPolicyCreated && apierrors.IsNotFound(err) {
				t.Errorf("Expected NamespaceOffloading to be created for %s", tt.namespace)
			} else if !tt.expectPolicyCreated && err == nil {
				t.Errorf("Expected NO NamespaceOffloading for %s", tt.namespace)
			}
		})
	}
}
func TestLiqoTrapReconciler_InitContainerAndDryRun(t *testing.T) {
	ctx := context.TODO()
	s := scheme.Scheme
	_ = corev1.AddToScheme(s)

	namespace := "default"
	podName := "init-trapped-pod"

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}

	// Create a Pod that is stuck in the InitContainer phase
	trappedPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: namespace},
		Status: corev1.PodStatus{
			InitContainerStatuses: []corev1.ContainerStatus{
				{
					State: corev1.ContainerState{
						Waiting: &corev1.ContainerStateWaiting{Reason: "OffloadingBackOff"},
					},
				},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(s).WithObjects(ns, trappedPod).Build()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: podName, Namespace: namespace}}

	// Reconcile with DryRun enabled
	reconciler := &LiqoTrapReconciler{
		Client:          fakeClient,
		BackoffDuration: 2 * time.Second,
		DryRun:          true, // Coverage for DryRun logging
	}

	if _, err := reconciler.Reconcile(ctx, req); err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	// Because it was a DryRun, the pod should NOT have the force-sync annotation applied
	var checkPod corev1.Pod
	_ = fakeClient.Get(ctx, types.NamespacedName{Name: podName, Namespace: namespace}, &checkPod)
	if checkPod.GetAnnotations()["dynamic-offloader.liqo.io/force-sync"] != "" {
		t.Errorf("Expected force-sync annotation NOT to be set because DryRun is true")
	}
}

// TestLiqoTrapReconciler_ForceSyncLimits verifies that force-sync attempts on
// a pod are spaced out, counted and bounded, and that a policy being deleted
// is waited for instead of being used.
func TestLiqoTrapReconciler_ForceSyncLimits(t *testing.T) {
	ctx := context.TODO()
	s := scheme.Scheme
	_ = corev1.AddToScheme(s)

	const backoff = time.Second
	stuckPodWith := func(attempts string, lastSync time.Time) *corev1.Pod {
		pod := createStuckPod("stuck-pod", "default")
		pod.Annotations = map[string]string{
			forceSyncAttemptsAnnotation: attempts,
			forceSyncAnnotation:         lastSync.UTC().Format(time.RFC3339Nano),
		}
		return pod
	}
	deletingPolicy := func() *unstructured.Unstructured {
		policy := createTrapOffloadingCR("default")
		now := metav1.Now()
		policy.SetDeletionTimestamp(&now)
		policy.SetFinalizers([]string{"liqo.io/test-finalizer"})
		return policy
	}

	tests := []struct {
		name            string
		pod             *corev1.Pod
		policy          *unstructured.Unstructured
		expectAttempts  string
		expectRequeue   time.Duration
		expectEventPart string
	}{
		{
			// The first patch counts the attempt and schedules the next check.
			name:           "First attempt: patches the pod and counts it",
			pod:            createStuckPod("stuck-pod", "default"),
			policy:         createTrapOffloadingCR("default"),
			expectAttempts: "1",
			expectRequeue:  backoff,
		},
		{
			// After two attempts the next one waits twice the base backoff.
			name:           "Backoff active: waits before the next attempt",
			pod:            stuckPodWith("2", time.Now()),
			policy:         createTrapOffloadingCR("default"),
			expectAttempts: "2",
			expectRequeue:  2 * backoff,
		},
		{
			// Once the backoff has elapsed the pod is patched again.
			name:           "Backoff elapsed: patches again and doubles the wait",
			pod:            stuckPodWith("1", time.Now().Add(-time.Hour)),
			policy:         createTrapOffloadingCR("default"),
			expectAttempts: "2",
			expectRequeue:  2 * backoff,
		},
		{
			// At the limit the pod is left alone and a Warning event explains why.
			name:            "Limit reached: gives up and records a warning event",
			pod:             stuckPodWith("3", time.Now().Add(-time.Hour)),
			policy:          createTrapOffloadingCR("default"),
			expectAttempts:  "3",
			expectRequeue:   0,
			expectEventPart: "Warning ForceSyncLimitReached",
		},
		{
			// A policy being deleted must not be used to sync the pod.
			name:           "Policy being deleted: waits instead of syncing",
			pod:            createStuckPod("stuck-pod", "default"),
			policy:         deletingPolicy(),
			expectAttempts: "",
			expectRequeue:  backoff,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeClient := fake.NewClientBuilder().WithScheme(s).
				WithObjects(createNamespace("default", nil), tt.pod, tt.policy).Build()
			recorder := record.NewFakeRecorder(10)
			reconciler := &LiqoTrapReconciler{
				Client:               fakeClient,
				BackoffDuration:      backoff,
				MaxForceSyncAttempts: 3,
				Recorder:             recorder,
			}

			req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "stuck-pod", Namespace: "default"}}
			result, err := reconciler.Reconcile(ctx, req)
			if err != nil {
				t.Fatalf("Unexpected error: %v", err)
			}

			// Requeues measured from "now" can be a few milliseconds shorter.
			if result.RequeueAfter > tt.expectRequeue || result.RequeueAfter < tt.expectRequeue-100*time.Millisecond {
				t.Errorf("RequeueAfter = %v; want about %v", result.RequeueAfter, tt.expectRequeue)
			}

			var pod corev1.Pod
			_ = fakeClient.Get(ctx, req.NamespacedName, &pod)
			if got := pod.GetAnnotations()[forceSyncAttemptsAnnotation]; got != tt.expectAttempts {
				t.Errorf("force-sync attempts = %q; want %q", got, tt.expectAttempts)
			}

			select {
			case event := <-recorder.Events:
				if tt.expectEventPart == "" || !strings.Contains(event, tt.expectEventPart) {
					t.Errorf("Unexpected event %q; want %q", event, tt.expectEventPart)
				}
			default:
				if tt.expectEventPart != "" {
					t.Errorf("Expected event %q, got none", tt.expectEventPart)
				}
			}
		})
	}
}

// TestForceSyncDelay verifies the exponential spacing and its cap.
func TestForceSyncDelay(t *testing.T) {
	tests := []struct {
		attempts int
		want     time.Duration
	}{
		{1, time.Second},
		{2, 2 * time.Second},
		{3, 4 * time.Second},
		{30, maxForceSyncDelay},
	}
	for _, tt := range tests {
		if got := forceSyncDelay(time.Second, tt.attempts); got != tt.want {
			t.Errorf("forceSyncDelay(1s, %d) = %v; want %v", tt.attempts, got, tt.want)
		}
	}
}
