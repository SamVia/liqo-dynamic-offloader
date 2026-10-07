package controllers

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

// TestLiqoTrapPredicate verifies that only trapped pod events enqueue
// remediation work and that delete events are ignored.
func TestLiqoTrapPredicate(t *testing.T) {
	pred := LiqoTrapPredicate()

	// Healthy pods should be ignored.
	healthyPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "healthy-pod"},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}

	// Trapped pods should trigger reconciliation.
	trappedPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "trapped-pod"},
		Status:     corev1.PodStatus{Reason: "OffloadingBackOff"},
	}

	// Create events only trigger for trapped pods.
	if pred.Create(event.CreateEvent{Object: healthyPod}) {
		t.Errorf("Expected healthy pod creation to be ignored")
	}
	if !pred.Create(event.CreateEvent{Object: trappedPod}) {
		t.Errorf("Expected trapped pod creation to trigger Reconcile")
	}

	// Update events evaluate the new object for the trapped condition.
	if pred.Update(event.UpdateEvent{ObjectOld: healthyPod, ObjectNew: healthyPod}) {
		t.Errorf("Expected healthy pod update to be ignored")
	}
	if !pred.Update(event.UpdateEvent{ObjectOld: healthyPod, ObjectNew: trappedPod}) {
		t.Errorf("Expected trapped pod update to trigger Reconcile")
	}

	// Delete events require no remediation.
	if pred.Delete(event.DeleteEvent{Object: trappedPod}) {
		t.Errorf("Expected Delete events to be ignored by Trap predicate")
	}
}

// TestLiqoCleanupPredicate verifies that cleanup reacts to pod lifecycle
// changes affecting remote workload accounting, but ignores metadata noise.
func TestLiqoCleanupPredicate(t *testing.T) {
	pred := LiqoCleanupPredicate()

	// Create and Delete events always trigger cleanup evaluation.
	if !pred.Create(event.CreateEvent{}) {
		t.Errorf("Expected Cleanup predicate to process Create events")
	}
	if !pred.Delete(event.DeleteEvent{}) {
		t.Errorf("Expected Cleanup predicate to process Delete events")
	}

	oldPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-pod"},
		Spec:       corev1.PodSpec{NodeName: ""},
		Status:     corev1.PodStatus{Phase: corev1.PodPending},
	}

	// Node assignment changes can affect remote workload accounting.
	newPodScheduled := oldPod.DeepCopy()
	newPodScheduled.Spec.NodeName = "cluster-remote"
	if !pred.Update(event.UpdateEvent{ObjectOld: oldPod, ObjectNew: newPodScheduled}) {
		t.Errorf("Expected Update event to trigger on NodeName change")
	}

	// Phase changes can make a workload active or terminal.
	newPodRunning := oldPod.DeepCopy()
	newPodRunning.Status.Phase = corev1.PodRunning
	if !pred.Update(event.UpdateEvent{ObjectOld: oldPod, ObjectNew: newPodRunning}) {
		t.Errorf("Expected Update event to trigger on Phase change")
	}

	// Deletion transitions must keep terminating remote pods in the count.
	newPodTerminating := oldPod.DeepCopy()
	now := metav1.NewTime(time.Now())
	newPodTerminating.DeletionTimestamp = &now
	if !pred.Update(event.UpdateEvent{ObjectOld: oldPod, ObjectNew: newPodTerminating}) {
		t.Errorf("Expected Update event to trigger on DeletionTimestamp change")
	}

	// Metadata-only changes should be ignored.
	newPodLabelled := oldPod.DeepCopy()
	newPodLabelled.Labels = map[string]string{"foo": "bar"}
	if pred.Update(event.UpdateEvent{ObjectOld: oldPod, ObjectNew: newPodLabelled}) {
		t.Errorf("Expected Update event to be ignored if Phase/NodeName/DeletionTimestamp did not change")
	}
}
