/*
This file is part of Cloud Native PostgreSQL.

Copyright (C) 2019-2021 EnterpriseDB Corporation.
*/

// Package run implements the "instance run" subcommand of the operator
package run

import (
	"context"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
	ctrl "sigs.k8s.io/controller-runtime/pkg/client"

	apiv1 "github.com/EnterpriseDB/cloud-native-postgresql/api/v1"
	"github.com/EnterpriseDB/cloud-native-postgresql/internal/management/controller"
	"github.com/EnterpriseDB/cloud-native-postgresql/pkg/management/execlog"
	"github.com/EnterpriseDB/cloud-native-postgresql/pkg/management/log"
	"github.com/EnterpriseDB/cloud-native-postgresql/pkg/management/postgres"
	"github.com/EnterpriseDB/cloud-native-postgresql/pkg/management/postgres/logpipe"
	"github.com/EnterpriseDB/cloud-native-postgresql/pkg/management/postgres/metricsserver"
	"github.com/EnterpriseDB/cloud-native-postgresql/pkg/management/postgres/webserver"
	"github.com/EnterpriseDB/cloud-native-postgresql/pkg/versions"
)

// NewCmd creates the "instance run" subcommand
func NewCmd() *cobra.Command {
	var pwFile string
	var pgData string
	var podName string
	var clusterName string
	var namespace string

	cmd := &cobra.Command{
		Use: "run [flags]",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			var instance postgres.Instance

			instance.PgData = pgData
			instance.Namespace = namespace
			instance.PodName = podName
			instance.ClusterName = clusterName

			return runSubCommand(ctx, &instance)
		},
	}

	cmd.Flags().StringVar(&pgData, "pg-data", os.Getenv("PGDATA"), "The PGDATA to be started up")
	cmd.Flags().StringVar(&podName, "pod-name", os.Getenv("POD_NAME"), "The name of this pod, to "+
		"be checked against the cluster state")
	cmd.Flags().StringVar(&clusterName, "cluster-name", os.Getenv("CLUSTER_NAME"), "The name of the "+
		"current cluster in k8s, used to coordinate switchover and failover")
	cmd.Flags().StringVar(&namespace, "namespace", os.Getenv("NAMESPACE"), "The namespace of "+
		"the cluster and of the Pod in k8s")
	cmd.Flags().StringVar(&pwFile, "pw-file", "",
		"The file containing the PostgreSQL superuser password to be used to connect to PostgreSQL")

	return cmd
}

func runSubCommand(ctx context.Context, instance *postgres.Instance) error {
	var err error

	log.Log.Info("Starting Cloud Native PostgreSQL Instance Manager",
		"version", versions.Version,
		"build", versions.Info)

	reconciler, err := controller.NewInstanceReconciler(instance)
	if err != nil {
		log.Log.Error(err, "Error while starting reconciler")
		return err
	}

	_, err = instance.RefreshConfigurationFiles(ctx, reconciler.GetClient())
	if err != nil {
		log.Log.Error(err, "Error while writing the bootstrap configuration")
		return err
	}

	var cluster apiv1.Cluster
	err = reconciler.GetClient().Get(ctx,
		ctrl.ObjectKey{Namespace: instance.Namespace, Name: instance.ClusterName},
		&cluster)
	if err != nil {
		log.Log.Error(err, "Error while getting cluster")
		return err
	}

	_, err = reconciler.RefreshServerCertificateFiles(ctx, &cluster)
	if err != nil {
		log.Log.Error(err, "Error while writing the TLS server certificates")
		return err
	}

	_, err = reconciler.RefreshReplicationUserCertificate(ctx, &cluster)
	if err != nil {
		log.Log.Error(err, "Error while writing the TLS server certificates")
		return err
	}

	_, err = reconciler.RefreshClientCA(ctx, &cluster)
	if err != nil {
		log.Log.Error(err, "Error while writing the TLS CA Client certificates")
		return err
	}

	_, err = reconciler.RefreshServerCA(ctx, &cluster)
	if err != nil {
		log.Log.Error(err, "Error while writing the TLS CA Server certificates")
		return err
	}

	err = reconciler.VerifyPgDataCoherence(ctx)
	if err != nil {
		log.Log.Error(err, "Error while checking Kubernetes cluster status")
		return err
	}

	primary, err := instance.IsPrimary()
	if err != nil {
		log.Log.Error(err, "Error while getting the primary status")
		os.Exit(1)
	}

	if !primary {
		err = reconciler.RefreshReplicaConfiguration(ctx)
		if err != nil {
			log.Log.Error(err, "Error while creating the replica configuration")
			os.Exit(1)
		}
	}

	if err = startWebServer(instance); err != nil {
		log.Log.Error(err, "Error while starting the web server")
		return err
	}

	startReconciler(ctx, reconciler)

	// Print the content of PostgreSQL control data, for debugging and tracing
	const pgControlDataName = "pg_controldata"
	pgControlDataCmd := exec.Command(pgControlDataName)
	pgControlDataCmd.Env = os.Environ()
	pgControlDataCmd.Env = append(pgControlDataCmd.Env, "PGDATA="+instance.PgData)
	err = execlog.RunBuffering(pgControlDataCmd, pgControlDataName)
	if err != nil {
		log.Log.Error(err, "Error printing the control information of this PostgreSQL instance")
		return err
	}

	if err = logpipe.Start(); err != nil {
		log.Log.Error(err, "Error while starting the logging collector routine")
		return err
	}

	postgresCommand, err := instance.Run()
	if err != nil {
		log.Log.Error(err, "Unable to start PostgreSQL up")
		return err
	}

	registerSignalHandler(reconciler, postgresCommand)

	if err = postgresCommand.Wait(); err != nil {
		log.Log.Error(err, "PostgreSQL exited with errors")
		return err
	}

	return nil
}

// startWebServer start the web server for handling probes given
// a certain PostgreSQL instance
func startWebServer(instance *postgres.Instance) error {
	webserver.Setup(instance)
	if err := metricsserver.Setup(instance); err != nil {
		return err
	}

	go func() {
		err := webserver.ListenAndServe()
		if err != nil {
			log.Log.Error(err, "Error while starting the status web server")
		}
	}()

	go func() {
		err := metricsserver.ListenAndServe()
		if err != nil {
			log.Log.Error(err, "Error while starting the metrics server")
		}
	}()

	return nil
}

// startReconciler start the reconciliation loop
func startReconciler(ctx context.Context, reconciler *controller.InstanceReconciler) {
	go reconciler.Run(ctx)
}

// registerSignalHandler handles signals from k8s, notifying postgres as
// needed
func registerSignalHandler(reconciler *controller.InstanceReconciler, postgresCommand *exec.Cmd) {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		sig := <-signals
		log.Log.Info("Received termination signal", "signal", sig)

		log.Log.Info("Shutting down web server")
		err := webserver.Shutdown()
		if err != nil {
			log.Log.Error(err, "Error while shutting down the web server")
		} else {
			log.Log.Info("Web server shut down")
		}

		err = metricsserver.Shutdown()
		if err != nil {
			log.Log.Error(err, "Error while shutting down the metrics server")
		} else {
			log.Log.Info("Metrics server shut down")
		}

		log.Log.Info("Shutting down controller")
		reconciler.Stop()

		if postgresCommand != nil {
			log.Log.Info("Shutting down PostgreSQL instance")
			err := postgresCommand.Process.Signal(syscall.SIGINT)
			if err != nil {
				log.Log.Error(err, "Unable to send SIGINT to PostgreSQL instance")
			}
		}
	}()
}
