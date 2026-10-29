package main

import (
	"flag"
	"os"
	"strings"
	"time"

	"liqo-dynamic-offloader/controllers"

	_ "k8s.io/client-go/plugin/pkg/client/auth"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

func main() {
	var targetClusterIDs string
	var allowedNamespaces string
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
	var metricsAddr string
	var leaderElectionNamespace string

	flag.StringVar(&targetClusterIDs, "target-cluster-ids", "", "Comma-separated list of Target Cluster IDs. Empty delegates to Liqo.")
	flag.StringVar(&allowedNamespaces, "allowed-namespaces", "", "Comma-separated list of namespace glob patterns to ALLOW (e.g. tenant-*). Empty allows all non-excluded namespaces.")
	flag.StringVar(&excludedNamespaces, "excluded-namespaces", "kube-*,*-system", "Comma-separated list of namespaces to exclude (supports glob expressions like *-system).")

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
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to. Use '0' to disable.")
	flag.StringVar(&leaderElectionNamespace, "leader-election-namespace", "default", "Namespace where the leader election lease will be created. Set this explicitly in deployments.")

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
		LeaderElectionNamespace: leaderElectionNamespace,
		HealthProbeBindAddress:  ":8081",
		Metrics: metricsserver.Options{
			BindAddress: metricsAddr,
		},
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	targetClusters := parseCSV(targetClusterIDs)
	allowedNsList := parseCSV(allowedNamespaces)
	excludedNsList := parseCSV(excludedNamespaces)

	// Register the Trap reconciler only when enabled.
	if enableTrap {
		if err = (&controllers.LiqoTrapReconciler{
			Client:             mgr.GetClient(),
			TargetClusters:     targetClusters,
			AllowedNamespaces:  allowedNsList,
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
			AllowedNamespaces:  allowedNsList,
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
		"AllowedNamespaces", allowedNsList,
		"ExcludedNamespaces", excludedNsList,
		"TrapWhitelist", trapWhitelist,
		"TrapBlacklist", trapBlacklist,
		"CleanupWhitelist", cleanupWhitelist,
		"CleanupBlacklist", cleanupBlacklist,
		"TrapBackoff", trapBackoff,
		"CleanupDelay", cleanupDelay,
		"MetricsAddr", metricsAddr,
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
// by the controllers as a key-presence match. Keys and values are trimmed, and
// entries with an empty key are ignored because they can never match a label.
func parseLabels(val string) map[string]string {
	res := make(map[string]string)
	for _, s := range strings.Split(val, ",") {
		key, value, _ := strings.Cut(s, "=")
		if key = strings.TrimSpace(key); key == "" {
			continue
		}
		res[key] = strings.TrimSpace(value)
	}
	return res
}
