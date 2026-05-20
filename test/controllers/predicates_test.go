package controllers

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

func TestLiqoTrapPredicate(t *testing.T) {
	pred := LiqoTrapPredicate()

	validEvent := &corev1.Event{
		Reason:         "ReflectionDisabled",
		InvolvedObject: corev1.ObjectReference{Kind: "Pod"},
	}
	invalidReasonEvent := &corev1.Event{
		Reason:         "FailedScheduling",
		InvolvedObject: corev1.ObjectReference{Kind: "Pod"},
	}
	invalidKindEvent := &corev1.Event{
		Reason:         "ReflectionDisabled",
		InvolvedObject: corev1.ObjectReference{Kind: "Deployment"},
	}
	notAnEvent := &corev1.Pod{}

	if !pred.Create(event.CreateEvent{Object: validEvent}) {
		t.Errorf("Expected valid trap event to be processed")
	}
	if pred.Create(event.CreateEvent{Object: invalidReasonEvent}) {
		t.Errorf("Expected event with wrong reason to be ignored")
	}
	if pred.Create(event.CreateEvent{Object: invalidKindEvent}) {
		t.Errorf("Expected event with wrong kind to be ignored")
	}
	if pred.Create(event.CreateEvent{Object: notAnEvent}) {
		t.Errorf("Expected non-event object to be ignored")
	}

	if pred.Update(event.UpdateEvent{}) {
		t.Errorf("Expected Trap predicate to ignore Update events")
	}
	if pred.Delete(event.DeleteEvent{}) {
		t.Errorf("Expected Trap predicate to ignore Delete events")
	}
}

func TestLiqoCleanupPredicate(t *testing.T) {
	pred := LiqoCleanupPredicate()

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

	newPodScheduled := oldPod.DeepCopy()
	newPodScheduled.Spec.NodeName = "cluster-remote"
	if !pred.Update(event.UpdateEvent{ObjectOld: oldPod, ObjectNew: newPodScheduled}) {
		t.Errorf("Expected Update event to trigger on NodeName change")
	}

	newPodRunning := oldPod.DeepCopy()
	newPodRunning.Status.Phase = corev1.PodRunning
	if !pred.Update(event.UpdateEvent{ObjectOld: oldPod, ObjectNew: newPodRunning}) {
		t.Errorf("Expected Update event to trigger on Phase change")
	}

	newPodLabelled := oldPod.DeepCopy()
	newPodLabelled.Labels = map[string]string{"foo": "bar"}
	if pred.Update(event.UpdateEvent{ObjectOld: oldPod, ObjectNew: newPodLabelled}) {
		t.Errorf("Expected Update event to be ignored if Phase/NodeName did not change")
	}

	if pred.Update(event.UpdateEvent{ObjectOld: &corev1.Service{}, ObjectNew: &corev1.Service{}}) {
		t.Errorf("Expected Update event to be ignored if objects are not Pods")
	}
}
