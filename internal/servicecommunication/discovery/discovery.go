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

// Package discovery performs common discovery operations for all services.
package discovery

import (
	"context"
	"errors"
	"reflect"
	"time"

	"github.com/shirou/gopsutil/v3/process"
	"github.com/GoogleCloudPlatform/workloadagent/internal/usagemetrics"
	"github.com/GoogleCloudPlatform/workloadagentplatform/integration/common/shared/commandlineexecutor"
	"github.com/GoogleCloudPlatform/workloadagentplatform/integration/common/shared/log"

	"github.com/GoogleCloudPlatform/workloadagent/internal/servicecommunication"
	cpb "github.com/GoogleCloudPlatform/workloadagent/protos/configuration"
)

// executeCommand abstracts the commandlineexecutor.ExecuteCommand for testability.
type executeCommand func(ctx context.Context, params commandlineexecutor.Params) commandlineexecutor.Result

// readFile abstracts the file reading operation for testability.
type readFile func(string) ([]byte, error)

// hostname abstracts the os.Hostname for testability.
type hostname func() (string, error)

// processLister is a wrapper around []*process.Process.
type processLister interface {
	listAllProcesses() ([]servicecommunication.ProcessWrapper, error)
}

// DefaultProcessLister implements the ProcessLister interface for listing processes.
type DefaultProcessLister struct{}

// listAllProcesses returns a list of processes.
func (DefaultProcessLister) listAllProcesses() ([]servicecommunication.ProcessWrapper, error) {
	ps, err := process.Processes()
	if err != nil {
		return nil, err
	}
	processes := make([]servicecommunication.ProcessWrapper, len(ps))
	for i, p := range ps {
		processes[i] = &gopsProcess{process: p}
	}
	return processes, nil
}

// Service is used to perform common discovery operations.
type Service struct {
	ProcessLister processLister
	ReadFile      readFile
	Hostname      hostname
	Config        *cpb.Configuration
}

// gopsProcess implements the processWrapper for abstracting process.Process.
type gopsProcess struct {
	process *process.Process
}

// Username returns a username of the process.
func (p gopsProcess) Username() (string, error) {
	return p.process.Username()
}

// Pid returns the PID of the process.
func (p gopsProcess) Pid() int32 {
	return p.process.Pid
}

// Name returns the name of the process.
func (p gopsProcess) Name() (string, error) {
	return p.process.Name()
}

// CmdlineSlice returns the command line arguments of the process.
func (p gopsProcess) CmdlineSlice() ([]string, error) {
	return p.process.CmdlineSlice()
}

// Environ returns the environment variables of the process.
// The format of each env var string is "key=value".
func (p gopsProcess) Environ() ([]string, error) {
	return p.process.Environ()
}

func (d Service) commonDiscoveryLoop(ctx context.Context) (servicecommunication.DiscoveryResult, error) {
	processes, err := d.ProcessLister.listAllProcesses()
	if err != nil {
		return servicecommunication.DiscoveryResult{}, err
	}
	if len(processes) < 1 {
		return servicecommunication.DiscoveryResult{}, errors.New("no processes found")
	}
	return servicecommunication.DiscoveryResult{Processes: processes}, nil
}

// CommonDiscovery returns a CommonDiscoveryResult and any errors encountered during the discovery process.
func (d Service) CommonDiscovery(ctx context.Context, a any) {
	log.CtxLogger(ctx).Info("CommonDiscovery started")
	var chs []chan<- *servicecommunication.Message
	var ok bool
	if chs, ok = a.([]chan<- *servicecommunication.Message); !ok {
		log.CtxLogger(ctx).Warnw("args is not of type []chan servicecommunication.Message", "args", a, "type", reflect.TypeOf(a), "kind", reflect.TypeOf(a).Kind())
		return
	}
	frequency := 3 * time.Hour
	if d.Config.GetCommonDiscovery().GetCollectionFrequency() != nil {
		frequency = d.Config.GetCommonDiscovery().GetCollectionFrequency().AsDuration()
	}
	ticker := time.NewTicker(frequency)
	defer ticker.Stop()
	for {
		discoveryResult, err := d.commonDiscoveryLoop(ctx)
		if err != nil {
			log.CtxLogger(ctx).Errorw("Failed to perform common discovery", "error", err)
			return
		}
		log.CtxLogger(ctx).Infof("CommonDiscovery found %d processes.", len(discoveryResult.Processes))
		fullChs := 0
		for _, ch := range chs {
			select {
			case ch <- &servicecommunication.Message{Origin: servicecommunication.Discovery, DiscoveryResult: discoveryResult}:
			default:
				fullChs++
			}
		}
		if fullChs > 0 {
			log.CtxLogger(ctx).Infof("CommonDiscovery found %d full channels that it was unable to write to.", fullChs)
		}
		select {
		case <-ctx.Done():
			log.CtxLogger(ctx).Info("CommonDiscovery cancellation requested")
			return
		case <-ticker.C:
			log.CtxLogger(ctx).Debug("CommonDiscovery ticker fired")
			continue
		}
	}
}

// ErrorCode returns the error code for CommonDiscovery.
func (d Service) ErrorCode() int {
	return usagemetrics.CommonDiscoveryFailure
}

// ExpectedMinDuration returns the expected minimum duration for CommonDiscovery.
// Used by the recovery handler to determine if the service ran long enough to be considered
// successful.
func (d Service) ExpectedMinDuration() time.Duration {
	return 0
}