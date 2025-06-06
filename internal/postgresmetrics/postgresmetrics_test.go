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

package postgresmetrics

import (
	"context"
	"errors"
	"strconv"
	"testing"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/api/googleapi"
	"google.golang.org/protobuf/testing/protocmp"
	"github.com/GoogleCloudPlatform/workloadagent/internal/workloadmanager"
	configpb "github.com/GoogleCloudPlatform/workloadagent/protos/configuration"
	gcefake "github.com/GoogleCloudPlatform/workloadagentplatform/sharedlibraries/gce/fake"
	"github.com/GoogleCloudPlatform/workloadagentplatform/sharedlibraries/gce/wlm"
	"github.com/GoogleCloudPlatform/workloadagentplatform/sharedlibraries/log"
)

type testDB struct {
	workMemRows rowsInterface
	workMemErr  error
	pingErr     error
}

var emptyDB = &testDB{}

func (t *testDB) QueryContext(ctx context.Context, query string, args ...any) (rowsInterface, error) {
	if query == "SHOW work_mem" {
		return t.workMemRows, t.workMemErr
	}
	return nil, nil
}

func (t *testDB) Ping() error {
	return t.pingErr
}

type workMemRows struct {
	count     int
	size      int
	data      string
	shouldErr bool
}

func (f *workMemRows) Scan(dest ...any) error {
	log.CtxLogger(context.Background()).Infow("fakeRows.Scan", "dest", dest, "dest[0]", dest[0], "data", f.data)
	if f.shouldErr {
		return errors.New("test-error")
	}
	*dest[0].(*string) = f.data
	return nil
}

func (f *workMemRows) Next() bool {
	f.count++
	return f.count <= f.size
}

func (f *workMemRows) Close() error {
	return nil
}

func TestInitDB(t *testing.T) {
	tests := []struct {
		name       string
		m          PostgresMetrics
		gceService gceInterface
		wantErr    bool
	}{
		{
			name: "HappyPath",
			m: PostgresMetrics{
				Config: &configpb.Configuration{
					PostgresConfiguration: &configpb.PostgresConfiguration{
						ConnectionParameters: &configpb.ConnectionParameters{
							Username: "test-user",
							Secret:   &configpb.SecretRef{ProjectId: "fake-project-id", SecretName: "fake-secret-name"},
						},
					},
				},
				connect: func(ctx context.Context, dataSource string) (dbInterface, error) { return emptyDB, nil },
			},
			gceService: &gcefake.TestGCE{
				GetSecretResp: []string{"fake-password"},
				GetSecretErr:  []error{nil},
			},
			wantErr: false,
		},
		{
			name: "HappyPathMissingSecret",
			m: PostgresMetrics{
				Config: &configpb.Configuration{
					PostgresConfiguration: &configpb.PostgresConfiguration{
						ConnectionParameters: &configpb.ConnectionParameters{
							Username: "test-user",
							Secret:   &configpb.SecretRef{ProjectId: "fake-project-id"},
						},
					},
				},
				connect: func(ctx context.Context, dataSource string) (dbInterface, error) { return emptyDB, nil },
			},
			gceService: &gcefake.TestGCE{
				GetSecretResp: []string{"fake-password"},
				GetSecretErr:  []error{errors.New("fake-error")},
			},
			wantErr: false,
		},
		{
			name: "ConfigPassword",
			m: PostgresMetrics{
				Config: &configpb.Configuration{
					PostgresConfiguration: &configpb.PostgresConfiguration{
						ConnectionParameters: &configpb.ConnectionParameters{
							Username: "test-user",
							Password: "fake-password",
						},
					},
				},
				connect: func(ctx context.Context, dataSource string) (dbInterface, error) { return emptyDB, nil },
			},
			gceService: &gcefake.TestGCE{
				GetSecretResp: []string{""},
				GetSecretErr:  []error{errors.New("fake-error")},
			},
			wantErr: false,
		},
		{
			name: "PasswordError",
			m: PostgresMetrics{
				Config: &configpb.Configuration{
					PostgresConfiguration: &configpb.PostgresConfiguration{
						ConnectionParameters: &configpb.ConnectionParameters{
							Username: "test-user",
							Secret:   &configpb.SecretRef{ProjectId: "fake-project-id", SecretName: "fake-secret-name"},
						},
					},
				},
				connect: func(ctx context.Context, dataSource string) (dbInterface, error) { return emptyDB, nil },
			},
			gceService: &gcefake.TestGCE{
				GetSecretResp: []string{""},
				GetSecretErr:  []error{errors.New("fake-error")},
			},
			wantErr: true,
		},
		{
			name: "ConnectError",
			m: PostgresMetrics{
				Config: &configpb.Configuration{
					PostgresConfiguration: &configpb.PostgresConfiguration{
						ConnectionParameters: &configpb.ConnectionParameters{
							Username: "test-user",
							Secret:   &configpb.SecretRef{ProjectId: "fake-project-id", SecretName: "fake-secret-name"},
						},
					},
				},
				connect: func(ctx context.Context, dataSource string) (dbInterface, error) {
					return nil, errors.New("fake-error")
				},
			},
			gceService: &gcefake.TestGCE{
				GetSecretResp: []string{"fake-password"},
				GetSecretErr:  []error{nil},
			},
			wantErr: true,
		},
	}

	ctx := context.Background()

	for _, tc := range tests {
		err := tc.m.InitDB(ctx, tc.gceService)
		gotErr := err != nil
		if gotErr != tc.wantErr {
			t.Errorf("InitDB(%v) = %v, wantErr %v", tc.name, err, tc.wantErr)
		}
	}
}

func TestInitDBError(t *testing.T) {
	m := PostgresMetrics{
		Config: &configpb.Configuration{
			PostgresConfiguration: &configpb.PostgresConfiguration{
				ConnectionParameters: &configpb.ConnectionParameters{
					Username: "test-user",
					Password: "fake-password",
				},
			},
		},
		connect: func(ctx context.Context, dataSource string) (dbInterface, error) {
			return &testDB{pingErr: errors.New("ping-error")}, nil
		},
	}
	gceService := &gcefake.TestGCE{}
	err := m.InitDB(context.Background(), gceService)
	if err == nil {
		t.Errorf("InitDB() = nil, want error")
	}
}

func TestCollectMetricsOnce(t *testing.T) {
	tests := []struct {
		name        string
		m           PostgresMetrics
		wantMetrics *workloadmanager.WorkloadMetrics
		wantErr     bool
	}{
		{
			name: "HappyPath",
			m: PostgresMetrics{
				db: &testDB{
					workMemRows: &workMemRows{count: 0, size: 1, data: "80MB", shouldErr: false},
					workMemErr:  nil,
				},
				WLMClient: &gcefake.TestWLM{
					WriteInsightErrs: []error{nil},
					WriteInsightResponses: []*wlm.WriteInsightResponse{
						&wlm.WriteInsightResponse{ServerResponse: googleapi.ServerResponse{HTTPStatusCode: 201}},
					},
				},
			},
			wantMetrics: &workloadmanager.WorkloadMetrics{
				WorkloadType: workloadmanager.POSTGRES,
				Metrics: map[string]string{
					workMemKey: strconv.Itoa(80 * 1024 * 1024),
				},
			},
			wantErr: false,
		},
		{
			name: "HappyPathKB",
			m: PostgresMetrics{
				db: &testDB{
					workMemRows: &workMemRows{count: 0, size: 1, data: "64kB", shouldErr: false},
					workMemErr:  nil,
				},
				WLMClient: &gcefake.TestWLM{
					WriteInsightErrs: []error{nil},
					WriteInsightResponses: []*wlm.WriteInsightResponse{
						&wlm.WriteInsightResponse{ServerResponse: googleapi.ServerResponse{HTTPStatusCode: 201}},
					},
				},
			},
			wantMetrics: &workloadmanager.WorkloadMetrics{
				WorkloadType: workloadmanager.POSTGRES,
				Metrics: map[string]string{
					workMemKey: strconv.Itoa(64 * 1024),
				},
			},
			wantErr: false,
		},
		{
			name: "HappyPathGB",
			m: PostgresMetrics{
				db: &testDB{
					workMemRows: &workMemRows{count: 0, size: 1, data: "4GB", shouldErr: false},
					workMemErr:  nil,
				},
				WLMClient: &gcefake.TestWLM{
					WriteInsightErrs: []error{nil},
					WriteInsightResponses: []*wlm.WriteInsightResponse{
						&wlm.WriteInsightResponse{ServerResponse: googleapi.ServerResponse{HTTPStatusCode: 201}},
					},
				},
			},
			wantMetrics: &workloadmanager.WorkloadMetrics{
				WorkloadType: workloadmanager.POSTGRES,
				Metrics: map[string]string{
					workMemKey: strconv.Itoa(4 * 1024 * 1024 * 1024),
				},
			},
			wantErr: false,
		},
		{
			name: "GetWorkMemError",
			m: PostgresMetrics{
				db: &testDB{
					workMemErr: errors.New("test-error"),
				},
				WLMClient: &gcefake.TestWLM{
					WriteInsightErrs: []error{nil},
					WriteInsightResponses: []*wlm.WriteInsightResponse{
						&wlm.WriteInsightResponse{ServerResponse: googleapi.ServerResponse{HTTPStatusCode: 201}},
					},
				},
			},
			wantMetrics: nil,
			wantErr:     true,
		},
		{
			name: "WLMClientError",
			m: PostgresMetrics{
				db: &testDB{
					workMemRows: &workMemRows{count: 0, size: 1, data: "64MB", shouldErr: false},
					workMemErr:  nil,
				},
				WLMClient: &gcefake.TestWLM{
					WriteInsightErrs: []error{errors.New("test-error")},
					WriteInsightResponses: []*wlm.WriteInsightResponse{
						&wlm.WriteInsightResponse{ServerResponse: googleapi.ServerResponse{HTTPStatusCode: 400}},
					},
				},
			},
			wantMetrics: nil,
			wantErr:     true,
		},
		{
			name: "NilWriteInsightResponse",
			m: PostgresMetrics{
				db: &testDB{
					workMemRows: &workMemRows{count: 0, size: 1, data: "64MB", shouldErr: false},
					workMemErr:  nil,
				},
				WLMClient: &gcefake.TestWLM{
					WriteInsightErrs:      []error{nil},
					WriteInsightResponses: []*wlm.WriteInsightResponse{nil},
				},
			},
			wantMetrics: &workloadmanager.WorkloadMetrics{
				WorkloadType: workloadmanager.POSTGRES,
				Metrics: map[string]string{
					workMemKey: strconv.Itoa(64 * 1024 * 1024),
				},
			},
			wantErr: false,
		},
	}

	ctx := context.Background()

	for _, tc := range tests {
		gotMetrics, err := tc.m.CollectMetricsOnce(ctx)
		if tc.wantErr {
			if err == nil {
				t.Errorf("CollectMetricsOnce(%v) returned no error, want error", tc.name)
			}
			continue
		}
		if diff := cmp.Diff(tc.wantMetrics, gotMetrics, protocmp.Transform()); diff != "" {
			t.Errorf("CollectMetricsOnce(%v) returned diff (-want +got):\n%s", tc.name, diff)
		}
	}
}
