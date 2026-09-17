package controllers

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

// TestLiqoTrapPredicate verifies the event filtering logic for the Trap reconciler.
func TestLiqoTrapPredicate(t *testing.T) {
	pred := LiqoTrapPredicate()

	// The Early Exit Pattern requires the predicate to allow Create and Update events through.
	// Actual state evaluation and filtering are safely deferred to the Reconcile loop itself.
	if !pred.Create(event.CreateEvent{}) {
		t.Errorf("Expected Create events to be passed to Reconcile")
	}
	if !pred.Update(event.UpdateEvent{}) {
		t.Errorf("Expected Update events to be passed to Reconcile")
	}

	// Delete events are ignored because if a stuck pod is deleted, the trap is resolved,
	// requiring no further remediation from this specific controller.
	if pred.Delete(event.DeleteEvent{}) {
		t.Errorf("Expected Delete events to be ignored by Trap predicate")
	}
}

// TestLiqoCleanupPredicate verifies the event filtering logic for the Cleanup reconciler.
// It ensures the controller is only woken up for meaningful pod lifecycle events to prevent hot-loops.
func TestLiqoCleanupPredicate(t *testing.T) {
	pred := LiqoCleanupPredicate()

	// 1. Base Event Types: Creations and Deletions always trigger a reconciliation
	// to evaluate if offloading policies need to be created or cleaned up.
	if !pred.Create(event.CreateEvent{}) {
		t.Errorf("Expected Cleanup predicate to process Create events")
	}
	if !pred.Delete(event.DeleteEvent{}) {
		t.Errorf("Expected Cleanup predicate to process Delete events")
	}

	// Mock a base pending pod with no node assigned yet to test transition triggers
	oldPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-pod"},
		Spec:       corev1.PodSpec{NodeName: ""},
		Status:     corev1.PodStatus{Phase: corev1.PodPending},
	}

	// 2. State Change: Node Assignment
	// Reconcile must trigger when a pod is scheduled to a node (local or remote cluster).
	newPodScheduled := oldPod.DeepCopy()
	newPodScheduled.Spec.NodeName = "cluster-remote"
	if !pred.Update(event.UpdateEvent{ObjectOld: oldPod, ObjectNew: newPodScheduled}) {
		t.Errorf("Expected Update event to trigger on NodeName change")
	}

	// 3. State Change: Pod Phase Transition
	// Reconcile must trigger when a pod moves between phases (e.g., Pending -> Running, or -> Succeeded).
	newPodRunning := oldPod.DeepCopy()
	newPodRunning.Status.Phase = corev1.PodRunning
	if !pred.Update(event.UpdateEvent{ObjectOld: oldPod, ObjectNew: newPodRunning}) {
		t.Errorf("Expected Update event to trigger on Phase change")
	}

	// 4. State Change: Graceful Shutdown Initiation
	// Reconcile must trigger when a pod starts terminating to correctly delay policy cleanup until completion.
	newPodTerminating := oldPod.DeepCopy()
	now := metav1.NewTime(time.Now())
	newPodTerminating.DeletionTimestamp = &now
	if !pred.Update(event.UpdateEvent{ObjectOld: oldPod, ObjectNew: newPodTerminating}) {
		t.Errorf("Expected Update event to trigger on DeletionTimestamp change")
	}

	// 5. Irrelevant Changes (Noise Reduction)
	// Reconcile MUST NOT trigger for irrelevant updates like label modifications or minor status updates.
	// This optimization is critical to prevent controller CPU thrashing.
	newPodLabelled := oldPod.DeepCopy()
	newPodLabelled.Labels = map[string]string{"foo": "bar"}
	if pred.Update(event.UpdateEvent{ObjectOld: oldPod, ObjectNew: newPodLabelled}) {
		t.Errorf("Expected Update event to be ignored if Phase/NodeName/DeletionTimestamp did not change")
	}
}
