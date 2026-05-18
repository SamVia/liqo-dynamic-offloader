package main

import (
	"os"

	"liqo-demo/operator/controllers"

	_ "k8s.io/client-go/plugin/pkg/client/auth"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

func main() {
	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))
	setupLog := ctrl.Log.WithName("setup")

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	// namespace offloading controller to enable the offloading of namespaces to remote clusters
	if err = (&controllers.LiqoTrapReconciler{
		Client: mgr.GetClient(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "LiqoTrap")
		os.Exit(1)
	}

	// namespace cleanup controller to enable the cleanup of namespaces offloading
	if err = (&controllers.LiqoCleanupReconciler{
		Client: mgr.GetClient(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "LiqoCleanup")
		os.Exit(1)
	}

	setupLog.Info("Starting controller with both LiqoTrap and LiqoCleanup")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running controller manager")
		os.Exit(1)
	}
}
