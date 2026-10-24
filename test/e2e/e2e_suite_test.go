//go:build e2e

// Package e2e verifies the operator end to end on two kind clusters peered
// with Liqo. The suite builds the whole environment itself: it creates the
// clusters, installs and peers Liqo, builds the operator image from the
// working tree and deploys it with the Helm chart.
//
// Environment variables:
//
//	E2E_SKIP_CLUSTER_SETUP=true  reuse existing clusters with Liqo already peered
//	E2E_KEEP_CLUSTERS=true       do not delete the clusters created by the suite
//	E2E_LOCAL_CLUSTER            local kind cluster name (default e2e-local)
//	E2E_REMOTE_CLUSTER           remote kind cluster name (default e2e-remote)
//	E2E_KIND_IMAGE               kind node image (default kindest/node:v1.35.0)
package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
)

// Operator configuration deployed by the suite. The specs derive their
// expectations from these values, so the two can never drift apart.
const (
	imageRepository    = "liqo-dynamic-offloader"
	imageTag           = "e2e"
	helmRelease        = "liqo-dynamic-offloader"
	operatorNamespace  = "dynamic-offloader-system"
	excludedNamespaces = "kube-*,*-system,local-path-storage,liqo*"
	trapIgnoreLabel    = "dynamic-offloader.liqo.io/ignore-trap"
	cleanupIgnoreLabel = "dynamic-offloader.liqo.io/ignore-cleanup"
	trapBackoff        = 2 * time.Second
	cleanupDelay       = 10 * time.Second
)

type suiteConfig struct {
	localCluster     string
	remoteCluster    string
	kindImage        string
	skipClusterSetup bool
	keepClusters     bool
}

var (
	cfg             suiteConfig
	projectDir      string
	k8sClient       client.Client
	clientset       *kubernetes.Clientset
	createdClusters bool
)

func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Liqo Dynamic Offloader E2E Suite")
}

var _ = BeforeSuite(func() {
	SetDefaultEventuallyPollingInterval(2 * time.Second)
	SetDefaultConsistentlyPollingInterval(2 * time.Second)

	cfg = suiteConfig{
		localCluster:     envOr("E2E_LOCAL_CLUSTER", "e2e-local"),
		remoteCluster:    envOr("E2E_REMOTE_CLUSTER", "e2e-remote"),
		kindImage:        envOr("E2E_KIND_IMAGE", "kindest/node:v1.35.0"),
		skipClusterSetup: os.Getenv("E2E_SKIP_CLUSTER_SETUP") == "true",
		keepClusters:     os.Getenv("E2E_KEEP_CLUSTERS") == "true",
	}

	var err error
	projectDir, err = filepath.Abs(filepath.Join("..", ".."))
	Expect(err).NotTo(HaveOccurred())

	for _, tool := range []string{"docker", "kind", "kubectl", "helm", "liqoctl"} {
		_, err := exec.LookPath(tool)
		Expect(err).NotTo(HaveOccurred(), "%s not found in PATH", tool)
	}

	if !cfg.skipClusterSetup {
		// Set before creating anything so AfterSuite also removes a
		// half-created environment.
		createdClusters = true

		By("creating the kind clusters")
		for _, name := range []string{cfg.localCluster, cfg.remoteCluster} {
			mustRun("kind", "create", "cluster", "--name", name, "--image", cfg.kindImage, "--wait", "180s")
		}

		By("installing Liqo on both clusters")
		for _, name := range []string{cfg.localCluster, cfg.remoteCluster} {
			mustRun("liqoctl", "install", "kind", "--cluster-id", name, "--context", kubeContext(name))
		}

		By("peering the local cluster with the remote one")
		mustRun("liqoctl", "peer",
			"--context", kubeContext(cfg.localCluster),
			"--remote-context", kubeContext(cfg.remoteCluster),
			"--gw-server-service-type", "NodePort")
	}

	restCfg, err := config.GetConfigWithContext(kubeContext(cfg.localCluster))
	Expect(err).NotTo(HaveOccurred())
	k8sClient, err = client.New(restCfg, client.Options{})
	Expect(err).NotTo(HaveOccurred())
	clientset, err = kubernetes.NewForConfig(restCfg)
	Expect(err).NotTo(HaveOccurred())

	By("waiting for the Liqo virtual node to become Ready")
	Eventually(func(g Gomega) {
		var node corev1.Node
		g.Expect(k8sClient.Get(context.Background(), client.ObjectKey{Name: cfg.remoteCluster}, &node)).To(Succeed())
		g.Expect(isNodeReady(&node)).To(BeTrue(), "virtual node %s is not Ready", cfg.remoteCluster)
	}).WithTimeout(5 * time.Minute).WithPolling(5 * time.Second).Should(Succeed())

	By("building the operator image from the working tree")
	image := imageRepository + ":" + imageTag
	mustRun("docker", "build", "-t", image, ".")
	mustRun("kind", "load", "docker-image", image, "--name", cfg.localCluster)

	By("deploying the operator with the Helm chart")
	mustRun("helm", "upgrade", "--install", helmRelease, "charts/liqo-dynamic-offloader",
		"--kube-context", kubeContext(cfg.localCluster),
		"--namespace", operatorNamespace, "--create-namespace",
		"--set-string", "image.repository="+imageRepository,
		"--set-string", "image.tag="+imageTag,
		"--set", "image.pullPolicy=Never",
		"--set-string", "config.targetClusterIDs="+cfg.remoteCluster,
		// Helm splits --set values on commas unless they are escaped.
		"--set-string", "config.excludedNamespaces="+strings.ReplaceAll(excludedNamespaces, ",", `\,`),
		"--set-string", "config.trapBlacklistLabels="+trapIgnoreLabel+"=true",
		"--set-string", "config.cleanupBlacklistLabels="+cleanupIgnoreLabel+"=true",
		"--set", "config.trapBackoff="+trapBackoff.String(),
		"--set", "config.cleanupDelay="+cleanupDelay.String())
	// The image tag is reused across runs, so force new pods to pick up the
	// freshly loaded image.
	mustRun("kubectl", "--context", kubeContext(cfg.localCluster), "-n", operatorNamespace,
		"rollout", "restart", "deployment/"+helmRelease)
	mustRun("kubectl", "--context", kubeContext(cfg.localCluster), "-n", operatorNamespace,
		"rollout", "status", "deployment/"+helmRelease, "--timeout=180s")

	By("waiting for the operator to become leader")
	// Without this, specs asserting that nothing happens could pass only
	// because the new pod is still waiting for the previous lease to expire.
	Eventually(operatorLogs).WithTimeout(2 * time.Minute).
		Should(ContainSubstring("successfully acquired lease"))
})

var _ = AfterSuite(func() {
	if !createdClusters || cfg.keepClusters {
		return
	}
	By("deleting the kind clusters")
	for _, name := range []string{cfg.localCluster, cfg.remoteCluster} {
		if out, err := run("kind", "delete", "cluster", "--name", name); err != nil {
			fmt.Fprintf(GinkgoWriter, "failed to delete cluster %s: %v\n%s\n", name, err, out)
		}
	}
})

func kubeContext(cluster string) string {
	return "kind-" + cluster
}

// run executes a command from the project root and returns its combined output.
func run(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = projectDir
	fmt.Fprintf(GinkgoWriter, "running: %s %s\n", name, strings.Join(args, " "))
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return string(out), nil
}

func mustRun(name string, args ...string) {
	GinkgoHelper()
	out, err := run(name, args...)
	Expect(err).NotTo(HaveOccurred(), "command output:\n%s", out)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
