/*
Copyright 2025 Google LLC

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

// Package oracle implements the oracle subcommand.
package oracle

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"time"

	dpb "google.golang.org/protobuf/types/known/durationpb"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"github.com/GoogleCloudPlatform/workloadagent/internal/daemon/configuration"
	"github.com/GoogleCloudPlatform/workloadagentplatform/sharedlibraries/log"

	cpb "github.com/GoogleCloudPlatform/workloadagent/protos/configuration"
)

// Config holds the configuration for the Oracle subcommand.
type Config struct {
	OracleConfiguration *cpb.OracleConfiguration
	CloudProperties     *cpb.CloudProperties
	Path                string
	ConfigModified      bool
}

// LoadWAConfig creates a new Configuration.
func LoadWAConfig(cloudProps *cpb.CloudProperties) (*cpb.Configuration, error) {
	config, err := configuration.Load(configPath(), os.ReadFile, cloudProps)
	if err != nil {
		return nil, fmt.Errorf("failed to load configuration: %w", err)
	}
	return config, nil
}

// NewCommand creates a new 'oracle' command.
func NewCommand(cloudProps *cpb.CloudProperties) *cobra.Command {
	ocfg := &Config{
		CloudProperties: cloudProps,
		Path:            configPath(),
	}
	var waConfig *cpb.Configuration
	var enabled bool

	oracleCmd := &cobra.Command{
		Use:   "oracle",
		Short: "Configure Oracle settings",
		Long: `Configure Oracle settings for the Google Cloud Agent for Compute Workloads.

This command allows you to enable and configure various features
for monitoring Oracle databases, including discovery and metrics collection.`,
		Run: func(cmd *cobra.Command, args []string) {
			if cmd.Flags().Changed("enabled") {
				ocfg.OracleConfiguration.Enabled = &enabled
				ocfg.ConfigModified = true
			}
			// TODO: We'll add/remove this logic once we have a clearer design.
			if enabled {
				fmt.Println("Oracle Configuration is Enabled.  Performing Oracle-specific tasks...")
			} else {
				fmt.Println("Oracle Configuration is Disabled.")
			}
		},
		// TODO: Remove PersistentPreRunE and PersistentPostRunE.
		// PersistentPreRunE is called before each cli command is run.
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			var err error
			waConfig, err = LoadWAConfig(cloudProps)
			if err != nil {
				return err
			}
			// Set default values for Oracle configuration if file is empty.
			if waConfig == nil {
				waConfig = &cpb.Configuration{
					CloudProperties: cloudProps,
					OracleConfiguration: &cpb.OracleConfiguration{
						Enabled: &enabled,
						OracleDiscovery: &cpb.OracleDiscovery{
							Enabled:         proto.Bool(true),
							UpdateFrequency: dpb.New(time.Duration(configuration.DefaultOracleDiscoveryFrequency)),
						},
						OracleMetrics: &cpb.OracleMetrics{
							Enabled:             proto.Bool(false),
							CollectionFrequency: dpb.New(time.Duration(configuration.DefaultOracleMetricsFrequency)),
							QueryTimeout:        dpb.New(time.Duration(configuration.DefaultOracleMetricsQueryTimeout)),
							MaxExecutionThreads: configuration.DefaultOracleMetricsMaxThreads,
							Queries:             []*cpb.Query{},
						},
					},
				}
			}

			ocfg.OracleConfiguration = waConfig.GetOracleConfiguration()
			return nil
		},
		// PersistentPostRunE is called after each cli command is run.
		PersistentPostRunE: func(cmd *cobra.Command, args []string) error {
			if !ocfg.ConfigModified {
				log.CtxLogger(cmd.Context()).Info("No Oracle configuration changes to save.")
				return nil
			}
			// TODO: Display Modified Configuration on Console.
			ctx := cmd.Context()
			return ocfg.writeFile(ctx, waConfig)
		},
	}

	oracleCmd.Flags().BoolVar(&enabled, "enabled", false, "Enable Oracle configuration")

	oracleCmd.AddCommand(DiscoveryCommand(ocfg))
	oracleCmd.AddCommand(MetricsCommand(ocfg))

	return oracleCmd
}

// writeFile writes the modified configuration to the configuration file.
func (c *Config) writeFile(ctx context.Context, wac *cpb.Configuration) error {
	// Create a clone of the input configuration
	clonedWac := proto.Clone(wac).(*cpb.Configuration)

	// Modify the cloned configuration
	clonedWac.OracleConfiguration = c.OracleConfiguration

	file, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(clonedWac)
	if err != nil {
		return fmt.Errorf("unable to marshal configuration.json: %w", err)
	}

	var buf bytes.Buffer
	json.Indent(&buf, file, "", "  ")
	err = os.WriteFile(c.Path, buf.Bytes(), 0644)
	if err != nil {
		return fmt.Errorf("unable to write configuration.json: %w", err)
	}
	log.CtxLogger(ctx).Info("Successfully Updated configuration.json")
	return nil
}

// configPath determines the configuration path based on the OS.
func configPath() string {
	if runtime.GOOS == "windows" {
		return configuration.WindowsConfigPath
	}
	return configuration.LinuxConfigPath
}
