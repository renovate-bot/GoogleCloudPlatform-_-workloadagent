/*
Copyright 2024 Google LLC

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package daemon implements daemon mode execution subcommand in Workload Agent.
package daemon

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/GoogleCloudPlatform/sapagent/shared/log"
	"github.com/GoogleCloudPlatform/sapagent/shared/recovery"
	"github.com/GoogleCloudPlatform/workloadagent/internal/daemon/configuration"
	"github.com/GoogleCloudPlatform/workloadagent/internal/daemon/mysql"
	"github.com/GoogleCloudPlatform/workloadagent/internal/daemon/oracle"
	"github.com/GoogleCloudPlatform/workloadagent/internal/usagemetrics"

	cpb "github.com/GoogleCloudPlatform/workloadagent/protos/configuration"
)

// Daemon has args for startdaemon subcommand.
type Daemon struct {
	configFilePath string
	lp             log.Parameters
	config         *cpb.Configuration
	cloudProps     *cpb.CloudProperties
	services       []Service
}

type (
	// Service defines the common interface for workload services.
	// Start methods are used to start the workload monitoring services.
	Service interface {
		Start(ctx context.Context, a any)
		String() string
		ErrorCode() int
		ExpectedMinDuration() time.Duration
	}
)

// NewDaemon creates a new startdaemon command.
func NewDaemon(lp log.Parameters, cloudProps *cpb.CloudProperties) *cobra.Command {
	d := &Daemon{
		lp:         lp,
		cloudProps: cloudProps,
	}
	cmd := &cobra.Command{
		Use:   "startdaemon",
		Short: "Start daemon mode of the agent",
		Long:  "startdaemon [--config <path-to-config-file>]",
		RunE: func(cmd *cobra.Command, args []string) error {
			return d.Execute(cmd.Context())
		},
	}
	cmd.Flags().StringVar(&d.configFilePath, "config", "", "configuration path for startdaemon mode")
	cmd.Flags().StringVar(&d.configFilePath, "c", "", "configuration path for startdaemon mode")
	return cmd
}

// Execute runs the daemon command.
func (d *Daemon) Execute(ctx context.Context) error {
	// Configure daemon logging with default values until the config file is loaded.
	d.lp.CloudLogName = `google-cloud-workload-agent`
	d.lp.LogFileName = `/var/log/google-cloud-workload-agent.log`
	if d.lp.OSType == "windows" {
		d.lp.LogFileName = `C:\Program Files\Google\google-cloud-workload-agent\logs\google-cloud-workload-agent.log`
		logDir := `C:\Program Files\Google\google-cloud-workload-agent\logs`
		os.MkdirAll(logDir, 0755)
		os.Chmod(logDir, 0777)
	}
	log.SetupLogging(d.lp)
	// Run the daemon handler that will start any services
	ctx, cancel := context.WithCancel(ctx)
	d.startdaemonHandler(ctx, cancel)
	return nil
}

func (d *Daemon) startdaemonHandler(ctx context.Context, cancel context.CancelFunc) error {
	var err error
	d.config, err = configuration.Load(d.configFilePath, os.ReadFile, d.cloudProps)
	if err != nil {
		return fmt.Errorf("loading %s configuration file: %w", d.configFilePath, err)
	}

	d.lp.LogToCloud = d.config.GetLogToCloud()
	d.lp.Level = configuration.LogLevelToZapcore(d.config.GetLogLevel())
	d.lp.CloudLoggingClient = log.CloudLoggingClient(ctx, d.config.GetCloudProperties().GetProjectId())
	if d.lp.CloudLoggingClient != nil {
		defer d.lp.CloudLoggingClient.Close()
	}

	log.SetupLogging(d.lp)

	log.Logger.Infow("Starting daemon mode", "agent_name", configuration.AgentName, "agent_version", configuration.AgentVersion)
	log.Logger.Infow("Cloud Properties",
		"projectid", d.cloudProps.GetProjectId(),
		"projectnumber", d.cloudProps.GetNumericProjectId(),
		"instanceid", d.cloudProps.GetInstanceId(),
		"zone", d.cloudProps.GetZone(),
		"instancename", d.cloudProps.GetInstanceName(),
		"machinetype", d.cloudProps.GetMachineType(),
		"image", d.cloudProps.GetImage(),
	)

	configureUsageMetricsForDaemon(d.cloudProps)
	usagemetrics.Configured()
	usagemetrics.Started()

	shutdownch := make(chan os.Signal, 1)
	signal.Notify(shutdownch, syscall.SIGINT, syscall.SIGTERM, os.Interrupt)

	// Add any additional services here.
	d.services = []Service{
		&oracle.Service{Config: d.config, CloudProps: d.cloudProps},
		&mysql.Service{Config: d.config, CloudProps: d.cloudProps},
	}
	for _, service := range d.services {
		log.Logger.Infof("Starting %s", service.String())
		recoverableStart := &recovery.RecoverableRoutine{
			Routine:             service.Start,
			ErrorCode:           service.ErrorCode(),
			ExpectedMinDuration: service.ExpectedMinDuration(),
			UsageLogger:         *usagemetrics.UsageLogger,
		}
		recoverableStart.StartRoutine(ctx)
	}

	// Log a RUNNING usage metric once a day.
	go usagemetrics.LogRunningDaily()
	d.waitForShutdown(shutdownch, cancel)
	return nil
}

// configureUsageMetricsForDaemon sets up UsageMetrics for Daemon.
func configureUsageMetricsForDaemon(cp *cpb.CloudProperties) {
	usagemetrics.SetAgentProperties(&cpb.AgentProperties{
		Name:            configuration.AgentName,
		Version:         configuration.AgentVersion,
		LogUsageMetrics: true,
	})
	usagemetrics.SetCloudProperties(cp)
}

// waitForShutdown observes a channel for a shutdown signal, then proceeds to shut down the Agent.
func (d *Daemon) waitForShutdown(ch <-chan os.Signal, cancel context.CancelFunc) {
	// wait for the shutdown signal
	<-ch
	log.Logger.Info("Shutdown signal observed, the agent will begin shutting down")
	cancel()
	usagemetrics.Stopped()
	time.Sleep(3 * time.Second)
	log.Logger.Info("Shutting down...")
}