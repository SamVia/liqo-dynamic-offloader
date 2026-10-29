//go:build e2e

package e2e

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

const podTimeout = 3 * time.Minute

// filteredWindow is how long a filtered namespace must stay without a policy.
// It spans several trap backoff periods, enough for the operator to act if
// its filters were broken.
const filteredWindow = 5*trapBackoff + 10*time.Second

var _ = Describe("Liqo dynamic offloader", func() {

	It("offloads a trapped pod and removes the policy once the namespace is idle", func() {
		ns := createNamespace("e2e-allowed", nil)
		createTrapDeployment(ns)

		By("waiting for the Trap to create the NamespaceOffloading")
		Eventually(policyExists(ns)).WithTimeout(podTimeout).Should(BeTrue())
		policy, err := getPolicy(ns)
		Expect(err).NotTo(HaveOccurred())
		Expect(clusterSelectorValues(policy)).To(ContainElement(cfg.remoteCluster))

		By("waiting for the pod to run on the virtual node")
		Eventually(trapPodMatches(ns, runningOn(cfg.remoteCluster))).WithTimeout(podTimeout).Should(BeTrue())

		By("checking that Cleanup keeps the policy while the pod is active")
		Consistently(policyExists(ns)).WithTimeout(cleanupDelay + 5*time.Second).Should(BeTrue())

		By("deleting the workload and waiting for Cleanup to remove the policy")
		Expect(deleteTrapDeployment(ns)).To(Succeed())
		Eventually(policyExists(ns)).WithTimeout(cleanupDelay + 90*time.Second).Should(BeFalse())
	})

	It("ignores namespaces excluded by name pattern", func() {
		// The suffix matches the *-system exclusion pattern.
		ns := createNamespaceNamed(uniqueName("e2e-excluded")+"-system", nil)
		createTrapDeployment(ns)

		Eventually(trapPodMatches(ns, isPodTrapped)).WithTimeout(podTimeout).Should(BeTrue())
		Consistently(policyExists(ns)).WithTimeout(filteredWindow).Should(BeFalse())
	})

	It("ignores namespaces with the Trap blacklist label", func() {
		ns := createNamespace("e2e-labeled", map[string]string{trapIgnoreLabel: "true"})
		createTrapDeployment(ns)

		Eventually(trapPodMatches(ns, isPodTrapped)).WithTimeout(podTimeout).Should(BeTrue())
		Consistently(policyExists(ns)).WithTimeout(filteredWindow).Should(BeFalse())
	})

	It("keeps the policy of namespaces with the Cleanup blacklist label", func() {
		ns := createNamespace("e2e-keep", map[string]string{cleanupIgnoreLabel: "true"})
		createTrapDeployment(ns)

		Eventually(policyExists(ns)).WithTimeout(podTimeout).Should(BeTrue())
		Eventually(trapPodMatches(ns, runningOn(cfg.remoteCluster))).WithTimeout(podTimeout).Should(BeTrue())

		Expect(deleteTrapDeployment(ns)).To(Succeed())
		Consistently(policyExists(ns)).WithTimeout(2*cleanupDelay + 10*time.Second).Should(BeTrue())
	})
})
