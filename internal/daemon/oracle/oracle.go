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

// Package oracle implements the Oracle workload agent service.
package oracle

import (
	"context"
	"runtime"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/workloadagent/internal/oraclediscovery"
	"github.com/GoogleCloudPlatform/workloadagent/internal/oraclemetrics"
	"github.com/GoogleCloudPlatform/workloadagent/internal/servicecommunication"
	"github.com/GoogleCloudPlatform/workloadagent/internal/usagemetrics"
	"github.com/GoogleCloudPlatform/workloadagentplatform/sharedlibraries/log"
	"github.com/GoogleCloudPlatform/workloadagentplatform/sharedlibraries/recovery"

	cpb "github.com/GoogleCloudPlatform/workloadagent/protos/configuration"
)

// Service implements the interfaces for Oracle workload agent service.
type Service struct {
	Config                  *cpb.Configuration
	CloudProps              *cpb.CloudProperties
	metricCollectionRoutine *recovery.RecoverableRoutine
	discoveryRoutine        *recovery.RecoverableRoutine
	currentSIDs             []string
	CommonCh                <-chan *servicecommunication.Message
	isProcessPresent        bool
	processes               []servicecommunication.ProcessWrapper
	processesMutex          sync.Mutex
}

type runDiscoveryArgs struct {
	s *Service
}

type runMetricCollectionArgs struct {
	s *Service
}

var oraProcessPrefixes = []string{"ora_pmon_", "db_pmon_"}

// Start initiates the Oracle workload agent service
func (s *Service) Start(ctx context.Context, a any) {
	go (func() {
		for {
			s.checkServiceCommunication(ctx)
		}
	})()
	// Check if the enabled field is unset. If it is, then the service is still enabled if the workload is present.
	if s.Config.GetOracleConfiguration().Enabled == nil {
		log.CtxLogger(ctx).Info("Oracle service enabled field is not set, will check for workload presence to determine if service should be enabled.")
		// If the workload is present, proceed with starting the service even if it is not enabled.
		for !s.isProcessPresent {
			time.Sleep(5 * time.Second)
		}
		log.CtxLogger(ctx).Info("Oracle workload is present. Starting service.")
	} else if !s.Config.GetOracleConfiguration().GetEnabled() {
		log.CtxLogger(ctx).Info("Oracle service is disabled")
		return
	}

	if runtime.GOOS != "linux" {
		log.CtxLogger(ctx).Error("Oracle service is only supported on Linux")
		return
	}

	if s.Config.GetOracleConfiguration().GetOracleDiscovery().GetEnabled() {
		dCtx := log.SetCtx(ctx, "context", "OracleDiscovery")
		s.discoveryRoutine = &recovery.RecoverableRoutine{
			Routine:             runDiscovery,
			RoutineArg:          runDiscoveryArgs{s},
			ErrorCode:           usagemetrics.OracleDiscoverDatabaseFailure,
			UsageLogger:         *usagemetrics.UsageLogger,
			ExpectedMinDuration: 20 * time.Second,
		}
		s.discoveryRoutine.StartRoutine(dCtx)
	}

	if s.Config.GetOracleConfiguration().GetOracleMetrics().GetEnabled() {
		mcCtx := log.SetCtx(ctx, "context", "OracleMetricCollection")
		s.metricCollectionRoutine = &recovery.RecoverableRoutine{
			Routine:             runMetricCollection,
			RoutineArg:          runMetricCollectionArgs{s},
			ErrorCode:           usagemetrics.OracleMetricCollectionFailure,
			UsageLogger:         *usagemetrics.UsageLogger,
			ExpectedMinDuration: 20 * time.Second,
		}
		s.metricCollectionRoutine.StartRoutine(mcCtx)
	}
	select {
	case <-ctx.Done():
		log.CtxLogger(ctx).Info("Oracle workload agent service cancellation requested")
		return
	}
}

func runDiscovery(ctx context.Context, a any) {
	log.CtxLogger(ctx).Info("Running Oracle Discovery")
	var args runDiscoveryArgs
	var ok bool
	if args, ok = a.(runDiscoveryArgs); !ok {
		log.CtxLogger(ctx).Error("args is not of type runDiscoveryArgs")
		return
	}
	s := args.s

	ticker := time.NewTicker(args.s.Config.GetOracleConfiguration().GetOracleDiscovery().GetUpdateFrequency().AsDuration())
	defer ticker.Stop()

	ds := oraclediscovery.New()

	for {
		// Discovery data is not used yet.
		s.processesMutex.Lock()
		processes := s.processes
		s.processesMutex.Unlock()
		// Don't start discovery until processes are populated.
		for processes == nil {
			time.Sleep(5 * time.Second)
			s.processesMutex.Lock()
			processes = s.processes
			s.processesMutex.Unlock()
			// Respect context cancellation.
			select {
			case <-ctx.Done():
				log.CtxLogger(ctx).Info("Oracle Discovery cancellation requested")
				return
			default:
				continue
			}
		}
		_, err := ds.Discover(ctx, s.CloudProps, processes)
		if err != nil {
			log.CtxLogger(ctx).Errorw("Failed to discover databases", "error", err)
			return
		}

		select {
		case <-ctx.Done():
			log.CtxLogger(ctx).Info("Oracle Discovery cancellation requested")
			return
		case <-ticker.C:
			continue
		}
	}
}

func runMetricCollection(ctx context.Context, a any) {
	log.CtxLogger(ctx).Info("Running Oracle metric collection")
	var args runMetricCollectionArgs
	var ok bool
	if args, ok = a.(runMetricCollectionArgs); !ok {
		log.CtxLogger(ctx).Errorw("Failed to parse metric collection args", "args", a)
		return
	}

	ticker := time.NewTicker(args.s.Config.GetOracleConfiguration().GetOracleMetrics().GetCollectionFrequency().AsDuration())
	defer ticker.Stop()

	metricCollector, err := oraclemetrics.New(ctx, args.s.Config)
	if err != nil {
		log.CtxLogger(ctx).Errorw("Failed to initialize metric collector", "error", err)
		return
	}

	for {
		select {
		case <-ctx.Done():
			log.CtxLogger(ctx).Info("Metric Collection cancellation requested")
			return
		case <-ticker.C:
			metricCollector.SendHealthMetricsToCloudMonitoring(ctx)
			metricCollector.SendDefaultMetricsToCloudMonitoring(ctx)
		}
	}
}

// checkServiceCommunication listens to the common channel for messages and processes them.
func (s *Service) checkServiceCommunication(ctx context.Context) {
	// Effectively give ctx.Done() priority over the channel.
	if ctx.Err() != nil {
		return
	}

	select {
	case <-ctx.Done():
		return
	case msg := <-s.CommonCh:
		log.CtxLogger(ctx).Debugw("Oracle workload agent service received a message on the common channel", "message", msg)
		switch msg.Origin {
		case servicecommunication.Discovery:
			log.CtxLogger(ctx).Debugw("Oracle workload agent service received a discovery message")
			s.processesMutex.Lock()
			s.processes = msg.DiscoveryResult.Processes
			s.processesMutex.Unlock()
			for _, p := range msg.DiscoveryResult.Processes {
				name, err := p.Name()
				if err == nil && servicecommunication.HasAnyPrefix(name, oraProcessPrefixes) {
					s.isProcessPresent = true
					break
				}
			}
		case servicecommunication.DWActivation:
			log.CtxLogger(ctx).Debugw("Oracle workload agent service received a DW activation message")
		default:
			log.CtxLogger(ctx).Debugw("Oracle workload agent service received a message with an unexpected origin", "origin", msg.Origin)
		}
	}
}

// String returns the name of the oracle service.
func (s *Service) String() string {
	return "Oracle Service"
}

// ErrorCode returns the error code for the oracle service.
func (s *Service) ErrorCode() int {
	return usagemetrics.OracleServiceError
}

// ExpectedMinDuration returns the expected minimum duration for the oracle service.
// Used by the recovery handler to determine if the service ran long enough to be considered
// successful.
func (s *Service) ExpectedMinDuration() time.Duration {
	return 20 * time.Second
}
