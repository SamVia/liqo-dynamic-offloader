package main

import (
	"flag"
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
	var targetClusterIDs string
	var excludedNamespaces string
	var trapWhitelist string
	var trapBlacklist string
	var cleanupWhitelist string
	var cleanupBlacklist string
	var trapBackoff time.Duration
	var cleanupDelay time.Duration
	var dryRun bool
	var enableTrap bool
	var enableCleanup bool

	flag.StringVar(&targetClusterIDs, "target-cluster-ids", "", "Comma-separated list of Target Cluster IDs. Empty delegates to Liqo.")
	flag.StringVar(&excludedNamespaces, "excluded-namespaces", "kube-system,liqo-system", "Comma-separated list of namespaces to exclude (supports glob expressions like *-system).")

	// Trap and Cleanup use independent labels so remediation and policy
	// cleanup can be enabled for different namespace populations.
	flag.StringVar(&trapWhitelist, "trap-whitelist-labels", "", "Comma-separated key=value labels REQUIRED on namespace to ALLOW Trap creation.")
	flag.StringVar(&trapBlacklist, "trap-blacklist-labels", "", "Comma-separated key=value labels on namespace to PREVENT Trap creation.")
	flag.StringVar(&cleanupWhitelist, "cleanup-whitelist-labels", "", "Comma-separated key=value labels REQUIRED on namespace to ALLOW Cleanup.")
	flag.StringVar(&cleanupBlacklist, "cleanup-blacklist-labels", "", "Comma-separated key=value labels on namespace to PREVENT Cleanup.")

	flag.DurationVar(&trapBackoff, "trap-backoff", 2*time.Second, "Wait duration before verifying stuck pod.")
	flag.DurationVar(&cleanupDelay, "cleanup-delay", 10*time.Second, "Delay before cleaning up an empty offloading policy. Set to 0 to disable.")
	flag.BoolVar(&dryRun, "dry-run", false, "Enable dry-run mode to log actions without modifying cluster state.")
	flag.BoolVar(&enableTrap, "enable-trap", true, "Enable the LiqoTrap controller (auto-offloading of stuck pods).")
	flag.BoolVar(&enableCleanup, "enable-cleanup", true, "Enable the LiqoCleanup controller (auto-removal of idle offloading policies).")

	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	// Bind the configured logger before creating controllers so startup and
	// reconciliation failures use the same structured logging pipeline.
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	setupLog := ctrl.Log.WithName("setup")

	// Enable Leader Election so only one replica mutates cluster state while
	// standby replicas remain available for failover.
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		LeaderElection:          true,
		LeaderElectionID:        "liqo-auto-healing-lock",
		LeaderElectionNamespace: "default",
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	targetClusters := parseCSV(targetClusterIDs)
	excludedNsList := parseCSV(excludedNamespaces)

	// Register the Trap reconciler only when enabled.
	if enableTrap {
		if err = (&controllers.LiqoTrapReconciler{
			Client:             mgr.GetClient(),
			TargetClusters:     targetClusters,
			ExcludedNamespaces: excludedNsList,
			WhitelistLabels:    parseLabels(trapWhitelist),
			BlacklistLabels:    parseLabels(trapBlacklist),
			BackoffDuration:    trapBackoff,
			DryRun:             dryRun,
		}).SetupWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create controller", "controller", "LiqoTrap")
			os.Exit(1)
		}
	} else {
		setupLog.Info("LiqoTrap controller is disabled (--enable-trap=false)")
	}

	// Register the Cleanup reconciler only when enabled.
	if enableCleanup {
		if err = (&controllers.LiqoCleanupReconciler{
			Client:             mgr.GetClient(),
			TargetClusters:     targetClusters,
			ExcludedNamespaces: excludedNsList,
			WhitelistLabels:    parseLabels(cleanupWhitelist),
			BlacklistLabels:    parseLabels(cleanupBlacklist),
			Recorder:           mgr.GetEventRecorderFor("liqo-cleanup"),
			CleanupDelay:       cleanupDelay,
			DryRun:             dryRun,
		}).SetupWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create controller", "controller", "LiqoCleanup")
			os.Exit(1)
		}
	} else {
		setupLog.Info("LiqoCleanup controller is disabled (--enable-cleanup=false)")
	}

	// Log the effective startup configuration for operational diagnostics.
	// Sensitive credentials are not included; only controller policy values are
	// reported.
	setupLog.Info("Starting controller",
		"EnableTrap", enableTrap,
		"EnableCleanup", enableCleanup,
		"TargetClusters", targetClusters,
		"ExcludedNamespaces", excludedNsList,
		"TrapWhitelist", trapWhitelist,
		"TrapBlacklist", trapBlacklist,
		"CleanupWhitelist", cleanupWhitelist,
		"CleanupBlacklist", cleanupBlacklist,
		"TrapBackoff", trapBackoff,
		"CleanupDelay", cleanupDelay,
		"DryRun", dryRun)

	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running controller manager")
		os.Exit(1)
	}
}

// parseCSV parses a comma-separated flag into trimmed, non-empty values.
// Empty entries are ignored so trailing commas do not create invalid targets
// or namespace patterns.
func parseCSV(val string) []string {
	var res []string
	for _, s := range strings.Split(val, ",") {
		if trimmed := strings.TrimSpace(s); trimmed != "" {
			res = append(res, trimmed)
		}
	}
	return res
}

// parseLabels parses comma-separated key[=value] entries into a label policy
// map. A key without a value is preserved as an empty value and is interpreted
// by the controllers as a key-presence match.
func parseLabels(val string) map[string]string {
	res := make(map[string]string)
	for _, s := range strings.Split(val, ",") {
		parts := strings.SplitN(strings.TrimSpace(s), "=", 2)
		if len(parts) == 2 {
			res[parts[0]] = parts[1]
		} else if len(parts) == 1 && parts[0] != "" {
			res[parts[0]] = ""
		}
	}
	return res
}
