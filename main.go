package main

import (
	"os"
	"strings"
	"time"

	"liqo-dynamic-offloader/controllers"

	_ "k8s.io/client-go/plugin/pkg/client/auth"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch;delete

func main() {
	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))
	setupLog := ctrl.Log.WithName("setup")

	// Initialize the controller manager with Leader Election enabled to guarantee High Availability.
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		LeaderElection:          true,
		LeaderElectionID:        "liqo-auto-healing-lock",
		LeaderElectionNamespace: "default",
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	// Read dynamic configuration parameters and target cluster IDs from environment variables.
	targetClusters := make([]string, 0)
	for _, clusterID := range strings.Split(os.Getenv("TARGET_CLUSTER_ID"), ",") {
		if clusterID = strings.TrimSpace(clusterID); clusterID != "" {
			targetClusters = append(targetClusters, clusterID)
		}
	}

	excludedNamespacesEnv := os.Getenv("EXCLUDED_NAMESPACES")
	excludedNamespaces := make(map[string]bool)

	if excludedNamespacesEnv == "" {
		// Default protection map for critical system namespaces that must never be modified or offloaded.
		excludedNamespaces["kube-system"] = true
		excludedNamespaces["liqo-system"] = true
		excludedNamespaces["local-path-storage"] = true
		excludedNamespaces["crownlabs-system"] = true
	} else {
		// Parse custom comma-separated namespaces provided via environment variables.
		namespaces := strings.Split(excludedNamespacesEnv, ",")
		for _, ns := range namespaces {
			trimmedNs := strings.TrimSpace(ns)
			if trimmedNs != "" {
				excludedNamespaces[trimmedNs] = true
			}
		}
	}

	// Read operational timeouts and backoff durations from the environment with secure fallbacks.
	trapBackoffStr := os.Getenv("TRAP_BACKOFF")
	trapBackoff, err := time.ParseDuration(trapBackoffStr)
	if err != nil || trapBackoff == 0 {
		trapBackoff = 2 * time.Second
	}

	cleanupGraceStr := os.Getenv("CLEANUP_GRACE_PERIOD")
	cleanupGrace, err := time.ParseDuration(cleanupGraceStr)
	if err != nil || cleanupGrace == 0 {
		cleanupGrace = 10 * time.Second
	}

	// Register the LiqoTrap reconciler to rescue pods stuck in OffloadingBackOff states.
	if err = (&controllers.LiqoTrapReconciler{
		Client:             mgr.GetClient(),
		TargetClusters:     targetClusters,
		ExcludedNamespaces: excludedNamespaces,
		BackoffDuration:    trapBackoff,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "LiqoTrap")
		os.Exit(1)
	}

	// Register the LiqoCleanup reconciler to automatically revoke unused namespace offloading policies.
	if err = (&controllers.LiqoCleanupReconciler{
		Client:             mgr.GetClient(),
		TargetClusters:     targetClusters,
		ExcludedNamespaces: excludedNamespaces,
		Recorder:           mgr.GetEventRecorderFor("liqo-cleanup"),
		GracePeriod:        cleanupGrace,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "LiqoCleanup")
		os.Exit(1)
	}

	setupLog.Info("Starting controller with both LiqoTrap and LiqoCleanup",
		"TargetClusters", targetClusters,
		"TrapBackoff", trapBackoff,
		"CleanupGracePeriod", cleanupGrace,
		"LeaderElection", true)

	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running controller manager")
		os.Exit(1)
	}
}
