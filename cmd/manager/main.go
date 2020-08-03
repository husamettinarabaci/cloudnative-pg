/*
This file is part of Cloud Native PostgreSQL.

Copyright (C) 2019-2020 2ndQuadrant Italia SRL. Exclusively licensed to 2ndQuadrant Limited.
*/

package main

import (
	"flag"
	"os"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	_ "k8s.io/client-go/plugin/pkg/client/auth/gcp"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	postgresqlv1alpha1 "gitlab.2ndquadrant.com/k8s/cloud-native-postgresql/api/v1alpha1"
	"gitlab.2ndquadrant.com/k8s/cloud-native-postgresql/cmd/manager/app"
	"gitlab.2ndquadrant.com/k8s/cloud-native-postgresql/controllers"
	"gitlab.2ndquadrant.com/k8s/cloud-native-postgresql/pkg/versions"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	_ = clientgoscheme.AddToScheme(scheme)

	_ = postgresqlv1alpha1.AddToScheme(scheme)
	// +kubebuilder:scaffold:scheme
}

// This is the main procedure of the operator, and is used as the
// controller-manager of the operator and as the controller of a certain
// PostgreSQL instance.
//
// This code really belongs to app/controller_manager.go but we can't put
// it here to respect the project layout created by kubebuilder.
//
// TODO this code wants to be replaced by using Cobra. Please evaluate if
// there are cons using Cobra with kubebuilder
func main() {
	// If we are about to handle a subcommand, let's do that
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "instance":
			app.InstanceManagerCommand(os.Args[2:])
			return

		case "bootstrap":
			app.BootstrapIntoCommand(os.Args[0], os.Args[2:])
			return

		case "wal-archive":
			app.WalArchiveCommand(os.Args[2:])
			return

		case "backup":
			app.BackupCommand(os.Args[2:])
			return
		}
	}

	// No subcommand invoked, let's start the operator
	var metricsAddr string
	var enableLeaderElection bool
	flag.StringVar(&metricsAddr, "metrics-addr", ":8080", "The address the metric endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "enable-leader-election", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))

	setupLog.Info("Starting Cloud Native PostgreSQL Operator", "version", versions.Version)

	watchNamespace := os.Getenv("WATCH_NAMESPACE")
	setupLog.Info("Listening for changes", "watchNamespace", watchNamespace)

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:             scheme,
		MetricsBindAddress: metricsAddr,
		Port:               9443,
		LeaderElection:     enableLeaderElection,
		LeaderElectionID:   "db9c8771.k8s.2ndq.io",
		Namespace:          watchNamespace,
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	if err = (&controllers.ClusterReconciler{
		Client: mgr.GetClient(),
		Log:    ctrl.Log.WithName("controllers").WithName("Cluster"),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Cluster")
		os.Exit(1)
	}
	if err = (&controllers.BackupReconciler{
		Client: mgr.GetClient(),
		Log:    ctrl.Log.WithName("controllers").WithName("Backup"),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Backup")
		os.Exit(1)
	}
	if err = (&controllers.ScheduledBackupReconciler{
		Client: mgr.GetClient(),
		Log:    ctrl.Log.WithName("controllers").WithName("ScheduledBackup"),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ScheduledBackup")
		os.Exit(1)
	}
	// +kubebuilder:scaffold:builder

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
