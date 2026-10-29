//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"math/rand"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const trapDeploymentName = "e2e-trap"

var offloadingGVK = schema.GroupVersionKind{
	Group:   "offloading.liqo.io",
	Version: "v1beta1",
	Kind:    "NamespaceOffloading",
}

// uniqueName appends a random suffix so reruns are independent of leftovers.
func uniqueName(prefix string) string {
	return fmt.Sprintf("%s-%05d", prefix, rand.Intn(100000))
}

func createNamespace(prefix string, labels map[string]string) string {
	GinkgoHelper()
	return createNamespaceNamed(uniqueName(prefix), labels)
}

// createNamespaceNamed creates a namespace and deletes it after the spec,
// dumping its state first if the spec failed.
func createNamespaceNamed(name string, labels map[string]string) string {
	GinkgoHelper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
	Expect(k8sClient.Create(context.Background(), ns)).To(Succeed())
	DeferCleanup(func() {
		if CurrentSpecReport().Failed() {
			dumpNamespace(name)
		}
		_ = k8sClient.Delete(context.Background(), ns)
	})
	return name
}

// createTrapDeployment creates a pod pinned to the Liqo virtual node. Without
// a NamespaceOffloading policy Liqo rejects it with OffloadingBackOff.
func createTrapDeployment(ns string) {
	GinkgoHelper()
	replicas := int32(1)
	labels := map[string]string{"app": trapDeploymentName}
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: trapDeploymentName, Namespace: ns},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					Containers:   []corev1.Container{{Name: "pause", Image: "registry.k8s.io/pause:3.9"}},
					NodeSelector: map[string]string{"kubernetes.io/hostname": cfg.remoteCluster},
					Tolerations:  []corev1.Toleration{{Key: "virtual-node.liqo.io/not-allowed", Operator: corev1.TolerationOpExists}},
				},
			},
		},
	}
	Expect(k8sClient.Create(context.Background(), deploy)).To(Succeed())
}

func deleteTrapDeployment(ns string) error {
	return k8sClient.Delete(context.Background(),
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: trapDeploymentName, Namespace: ns}})
}

func getPolicy(ns string) (*unstructured.Unstructured, error) {
	policy := &unstructured.Unstructured{}
	policy.SetGroupVersionKind(offloadingGVK)
	err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "offloading", Namespace: ns}, policy)
	return policy, err
}

// policyExists returns a poll function for Eventually and Consistently. A
// NotFound error means "no policy", any other error fails the assertion.
func policyExists(ns string) func() (bool, error) {
	return func() (bool, error) {
		_, err := getPolicy(ns)
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return err == nil, err
	}
}

// trapPodMatches returns a poll function reporting whether any trap pod in the
// namespace satisfies cond.
func trapPodMatches(ns string, cond func(*corev1.Pod) bool) func() (bool, error) {
	return func() (bool, error) {
		var pods corev1.PodList
		if err := k8sClient.List(context.Background(), &pods, client.InNamespace(ns),
			client.MatchingLabels{"app": trapDeploymentName}); err != nil {
			return false, err
		}
		for i := range pods.Items {
			if cond(&pods.Items[i]) {
				return true, nil
			}
		}
		return false, nil
	}
}

func runningOn(node string) func(*corev1.Pod) bool {
	return func(pod *corev1.Pod) bool {
		return pod.Status.Phase == corev1.PodRunning && pod.Spec.NodeName == node
	}
}

// isPodTrapped mirrors the operator's own detection of OffloadingBackOff.
func isPodTrapped(pod *corev1.Pod) bool {
	if pod.Status.Reason == "OffloadingBackOff" {
		return true
	}
	for _, cs := range append(pod.Status.ContainerStatuses, pod.Status.InitContainerStatuses...) {
		if cs.State.Waiting != nil && cs.State.Waiting.Reason == "OffloadingBackOff" {
			return true
		}
	}
	return false
}

func isNodeReady(node *corev1.Node) bool {
	for _, cond := range node.Status.Conditions {
		if cond.Type == corev1.NodeReady {
			return cond.Status == corev1.ConditionTrue
		}
	}
	return false
}

func clusterSelectorValues(policy *unstructured.Unstructured) []string {
	terms, _, _ := unstructured.NestedSlice(policy.Object, "spec", "clusterSelector", "nodeSelectorTerms")
	var values []string
	for _, term := range terms {
		exprs, _, _ := unstructured.NestedSlice(term.(map[string]interface{}), "matchExpressions")
		for _, expr := range exprs {
			v, _, _ := unstructured.NestedStringSlice(expr.(map[string]interface{}), "values")
			values = append(values, v...)
		}
	}
	return values
}

func operatorPodName() (string, error) {
	var pods corev1.PodList
	if err := k8sClient.List(context.Background(), &pods, client.InNamespace(operatorNamespace),
		client.MatchingLabels{"app.kubernetes.io/name": "liqo-dynamic-offloader"}); err != nil {
		return "", err
	}
	for _, pod := range pods.Items {
		if pod.Status.Phase == corev1.PodRunning && pod.DeletionTimestamp.IsZero() {
			return pod.Name, nil
		}
	}
	return "", fmt.Errorf("no running operator pod in namespace %s", operatorNamespace)
}

func operatorLogs() (string, error) {
	pod, err := operatorPodName()
	if err != nil {
		return "", err
	}
	raw, err := clientset.CoreV1().Pods(operatorNamespace).GetLogs(pod, &corev1.PodLogOptions{}).DoRaw(context.Background())
	return string(raw), err
}

// dumpNamespace logs the state of a failed spec's namespace to ease debugging.
func dumpNamespace(ns string) {
	var pods corev1.PodList
	if err := k8sClient.List(context.Background(), &pods, client.InNamespace(ns)); err == nil {
		for _, pod := range pods.Items {
			fmt.Fprintf(GinkgoWriter, "[%s] pod %s phase=%s reason=%q node=%q annotations=%v\n",
				ns, pod.Name, pod.Status.Phase, pod.Status.Reason, pod.Spec.NodeName, pod.Annotations)
		}
	}
	if policy, err := getPolicy(ns); err == nil {
		fmt.Fprintf(GinkgoWriter, "[%s] policy annotations=%v spec=%v\n", ns, policy.GetAnnotations(), policy.Object["spec"])
	}
	if logs, err := operatorLogs(); err == nil {
		lines := strings.Split(strings.TrimSpace(logs), "\n")
		if len(lines) > 50 {
			lines = lines[len(lines)-50:]
		}
		fmt.Fprintf(GinkgoWriter, "Operator logs (last %d lines):\n%s\n", len(lines), strings.Join(lines, "\n"))
	}
}
