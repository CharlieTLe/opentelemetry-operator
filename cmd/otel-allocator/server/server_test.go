// Copyright The OpenTelemetry Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package server

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/common/config"
	"github.com/prometheus/common/model"
	promconfig "github.com/prometheus/prometheus/config"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/model/relabel"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v2"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/open-telemetry/opentelemetry-operator/cmd/otel-allocator/allocation"
	allocatorconfig "github.com/open-telemetry/opentelemetry-operator/cmd/otel-allocator/config"
	"github.com/open-telemetry/opentelemetry-operator/cmd/otel-allocator/target"
)

var (
	logger       = logf.Log.WithName("server-unit-tests")
	baseLabelSet = labels.Labels{
		{Name: "test_label", Value: "test-value"},
	}
	testJobLabelSetTwo = labels.Labels{
		{Name: "test_label", Value: "test-value2"},
	}
	baseTargetItem          = target.NewItem("test-job", "test-url", baseLabelSet, "test-collector")
	secondTargetItem        = target.NewItem("test-job", "test-url", baseLabelSet, "test-collector")
	testJobTargetItemTwo    = target.NewItem("test-job", "test-url2", testJobLabelSetTwo, "test-collector2")
	testJobTwoTargetItemTwo = target.NewItem("test-job2", "test-url3", testJobLabelSetTwo, "test-collector2")
)

func TestServer_LivenessProbeHandler(t *testing.T) {
	leastWeighted, _ := allocation.New("least-weighted", logger)
	listenAddr := ":8080"
	s := NewServer(logger, leastWeighted, listenAddr)
	request := httptest.NewRequest("GET", "/livez", nil)
	w := httptest.NewRecorder()

	s.server.Handler.ServeHTTP(w, request)
	result := w.Result()

	assert.Equal(t, http.StatusOK, result.StatusCode)
}

func TestServer_TargetsHandler(t *testing.T) {
	leastWeighted, _ := allocation.New("least-weighted", logger)
	type args struct {
		collector string
		job       string
		cMap      map[string]*target.Item
		allocator allocation.Allocator
	}
	type want struct {
		items     []*targetJSON
		errString string
	}
	tests := []struct {
		name string
		args args
		want want
	}{
		{
			name: "Empty target map",
			args: args{
				collector: "test-collector",
				job:       "test-job",
				cMap:      map[string]*target.Item{},
				allocator: leastWeighted,
			},
			want: want{
				items: []*targetJSON{},
			},
		},
		{
			name: "Single entry target map",
			args: args{
				collector: "test-collector",
				job:       "test-job",
				cMap: map[string]*target.Item{
					baseTargetItem.Hash(): baseTargetItem,
				},
				allocator: leastWeighted,
			},
			want: want{
				items: []*targetJSON{
					{
						TargetURL: []string{"test-url"},
						Labels: labels.Labels{
							{Name: "test_label", Value: "test-value"},
						},
					},
				},
			},
		},
		{
			name: "Multiple entry target map",
			args: args{
				collector: "test-collector",
				job:       "test-job",
				cMap: map[string]*target.Item{
					baseTargetItem.Hash():   baseTargetItem,
					secondTargetItem.Hash(): secondTargetItem,
				},
				allocator: leastWeighted,
			},
			want: want{
				items: []*targetJSON{
					{
						TargetURL: []string{"test-url"},
						Labels: labels.Labels{
							{Name: "test_label", Value: "test-value"},
						},
					},
				},
			},
		},
		{
			name: "Multiple entry target map of same job with label merge",
			args: args{
				collector: "test-collector",
				job:       "test-job",
				cMap: map[string]*target.Item{
					baseTargetItem.Hash():       baseTargetItem,
					testJobTargetItemTwo.Hash(): testJobTargetItemTwo,
				},
				allocator: leastWeighted,
			},
			want: want{
				items: []*targetJSON{
					{
						TargetURL: []string{"test-url"},
						Labels: labels.Labels{
							{Name: "test_label", Value: "test-value"},
						},
					},
					{
						TargetURL: []string{"test-url2"},
						Labels: labels.Labels{
							{Name: "test_label", Value: "test-value2"},
						},
					},
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			listenAddr := ":8080"
			s := NewServer(logger, tt.args.allocator, listenAddr)
			tt.args.allocator.SetCollectors(map[string]*allocation.Collector{"test-collector": {Name: "test-collector"}})
			tt.args.allocator.SetTargets(tt.args.cMap)
			request := httptest.NewRequest("GET", fmt.Sprintf("/jobs/%s/targets?collector_id=%s", tt.args.job, tt.args.collector), nil)
			w := httptest.NewRecorder()

			s.server.Handler.ServeHTTP(w, request)
			result := w.Result()

			assert.Equal(t, http.StatusOK, result.StatusCode)
			body := result.Body
			bodyBytes, err := io.ReadAll(body)
			assert.NoError(t, err)
			if len(tt.want.errString) != 0 {
				assert.EqualError(t, err, tt.want.errString)
				return
			}
			var itemResponse []*targetJSON
			err = json.Unmarshal(bodyBytes, &itemResponse)
			assert.NoError(t, err)
			assert.ElementsMatch(t, tt.want.items, itemResponse)
		})
	}
}

func TestServer_ScrapeConfigsHandler(t *testing.T) {
	svrConfig := allocatorconfig.HTTPSServerConfig{}
	tlsConfig, _ := svrConfig.NewTLSConfig()
	tests := []struct {
		description   string
		scrapeConfigs map[string]*promconfig.ScrapeConfig
		expectedCode  int
		expectedBody  []byte
		serverOptions []Option
	}{
		{
			description:   "nil scrape config",
			scrapeConfigs: nil,
			expectedCode:  http.StatusOK,
			expectedBody:  []byte("{}"),
		},
		{
			description:   "empty scrape config",
			scrapeConfigs: map[string]*promconfig.ScrapeConfig{},
			expectedCode:  http.StatusOK,
			expectedBody:  []byte("{}"),
		},
		{
			description: "single entry",
			scrapeConfigs: map[string]*promconfig.ScrapeConfig{
				"serviceMonitor/testapp/testapp/0": {
					JobName:         "serviceMonitor/testapp/testapp/0",
					HonorTimestamps: true,
					ScrapeInterval:  model.Duration(30 * time.Second),
					ScrapeTimeout:   model.Duration(30 * time.Second),
					MetricsPath:     "/metrics",
					Scheme:          "http",
					HTTPClientConfig: config.HTTPClientConfig{
						FollowRedirects: true,
					},
					RelabelConfigs: []*relabel.Config{
						{
							SourceLabels: model.LabelNames{model.LabelName("job")},
							Separator:    ";",
							Regex:        relabel.MustNewRegexp("(.*)"),
							TargetLabel:  "__tmp_prometheus_job_name",
							Replacement:  "$$1",
							Action:       relabel.Replace,
						},
					},
				},
			},
			expectedCode: http.StatusOK,
		},
		{
			description: "multiple entries",
			scrapeConfigs: map[string]*promconfig.ScrapeConfig{
				"serviceMonitor/testapp/testapp/0": {
					JobName:         "serviceMonitor/testapp/testapp/0",
					HonorTimestamps: true,
					ScrapeInterval:  model.Duration(30 * time.Second),
					ScrapeTimeout:   model.Duration(30 * time.Second),
					MetricsPath:     "/metrics",
					Scheme:          "http",
					HTTPClientConfig: config.HTTPClientConfig{
						FollowRedirects: true,
					},
					RelabelConfigs: []*relabel.Config{
						{
							SourceLabels: model.LabelNames{model.LabelName("job")},
							Separator:    ";",
							Regex:        relabel.MustNewRegexp("(.*)"),
							TargetLabel:  "__tmp_prometheus_job_name",
							Replacement:  "$$1",
							Action:       relabel.Replace,
						},
						{
							SourceLabels: model.LabelNames{
								model.LabelName("__meta_kubernetes_service_label_app_kubernetes_io_name"),
								model.LabelName("__meta_kubernetes_service_labelpresent_app_kubernetes_io_name"),
							},
							Separator:   ";",
							Regex:       relabel.MustNewRegexp("(testapp);true"),
							Replacement: "$$1",
							Action:      relabel.Keep,
						},
						{
							SourceLabels: model.LabelNames{model.LabelName("__meta_kubernetes_endpoint_port_name")},
							Separator:    ";",
							Regex:        relabel.MustNewRegexp("http"),
							Replacement:  "$$1",
							Action:       relabel.Keep,
						},
						{
							SourceLabels: model.LabelNames{model.LabelName("__meta_kubernetes_namespace")},
							Separator:    ";",
							Regex:        relabel.MustNewRegexp("(.*)"),
							TargetLabel:  "namespace",
							Replacement:  "$$1",
							Action:       relabel.Replace,
						},
						{
							SourceLabels: model.LabelNames{model.LabelName("__meta_kubernetes_service_name")},
							Separator:    ";",
							Regex:        relabel.MustNewRegexp("(.*)"),
							TargetLabel:  "service",
							Replacement:  "$$1",
							Action:       relabel.Replace,
						},
						{
							SourceLabels: model.LabelNames{model.LabelName("__meta_kubernetes_pod_name")},
							Separator:    ";",
							Regex:        relabel.MustNewRegexp("(.*)"),
							TargetLabel:  "pod",
							Replacement:  "$$1",
							Action:       relabel.Replace,
						},
						{
							SourceLabels: model.LabelNames{model.LabelName("__meta_kubernetes_pod_container_name")},
							Separator:    ";",
							Regex:        relabel.MustNewRegexp("(.*)"),
							TargetLabel:  "container",
							Replacement:  "$$1",
							Action:       relabel.Replace,
						},
					},
				},
				"serviceMonitor/testapp/testapp1/0": {
					JobName:         "serviceMonitor/testapp/testapp1/0",
					HonorTimestamps: true,
					ScrapeInterval:  model.Duration(5 * time.Minute),
					ScrapeTimeout:   model.Duration(10 * time.Second),
					MetricsPath:     "/v2/metrics",
					Scheme:          "http",
					HTTPClientConfig: config.HTTPClientConfig{
						FollowRedirects: true,
					},
					RelabelConfigs: []*relabel.Config{
						{
							SourceLabels: model.LabelNames{model.LabelName("job")},
							Separator:    ";",
							Regex:        relabel.MustNewRegexp("(.*)"),
							TargetLabel:  "__tmp_prometheus_job_name",
							Replacement:  "$$1",
							Action:       relabel.Replace,
						},
						{
							SourceLabels: model.LabelNames{
								model.LabelName("__meta_kubernetes_service_label_app_kubernetes_io_name"),
								model.LabelName("__meta_kubernetes_service_labelpresent_app_kubernetes_io_name"),
							},
							Separator:   ";",
							Regex:       relabel.MustNewRegexp("(testapp);true"),
							Replacement: "$$1",
							Action:      relabel.Keep,
						},
						{
							SourceLabels: model.LabelNames{model.LabelName("__meta_kubernetes_endpoint_port_name")},
							Separator:    ";",
							Regex:        relabel.MustNewRegexp("http"),
							Replacement:  "$$1",
							Action:       relabel.Keep,
						},
						{
							SourceLabels: model.LabelNames{model.LabelName("__meta_kubernetes_namespace")},
							Separator:    ";",
							Regex:        relabel.MustNewRegexp("(.*)"),
							TargetLabel:  "namespace",
							Replacement:  "$$1",
							Action:       relabel.Replace,
						},
						{
							SourceLabels: model.LabelNames{model.LabelName("__meta_kubernetes_service_name")},
							Separator:    ";",
							Regex:        relabel.MustNewRegexp("(.*)"),
							TargetLabel:  "service",
							Replacement:  "$$1",
							Action:       relabel.Replace,
						},
						{
							SourceLabels: model.LabelNames{model.LabelName("__meta_kubernetes_pod_name")},
							Separator:    ";",
							Regex:        relabel.MustNewRegexp("(.*)"),
							TargetLabel:  "pod",
							Replacement:  "$$1",
							Action:       relabel.Replace,
						},
						{
							SourceLabels: model.LabelNames{model.LabelName("__meta_kubernetes_pod_container_name")},
							Separator:    ";",
							Regex:        relabel.MustNewRegexp("(.*)"),
							TargetLabel:  "container",
							Replacement:  "$$1",
							Action:       relabel.Replace,
						},
					},
				},
				"serviceMonitor/testapp/testapp2/0": {
					JobName:         "serviceMonitor/testapp/testapp2/0",
					HonorTimestamps: true,
					ScrapeInterval:  model.Duration(30 * time.Minute),
					ScrapeTimeout:   model.Duration(2 * time.Minute),
					MetricsPath:     "/metrics",
					Scheme:          "http",
					HTTPClientConfig: config.HTTPClientConfig{
						FollowRedirects: true,
					},
					RelabelConfigs: []*relabel.Config{
						{
							SourceLabels: model.LabelNames{model.LabelName("job")},
							Separator:    ";",
							Regex:        relabel.MustNewRegexp("(.*)"),
							TargetLabel:  "__tmp_prometheus_job_name",
							Replacement:  "$$1",
							Action:       relabel.Replace,
						},
						{
							SourceLabels: model.LabelNames{
								model.LabelName("__meta_kubernetes_service_label_app_kubernetes_io_name"),
								model.LabelName("__meta_kubernetes_service_labelpresent_app_kubernetes_io_name"),
							},
							Separator:   ";",
							Regex:       relabel.MustNewRegexp("(testapp);true"),
							Replacement: "$$1",
							Action:      relabel.Keep,
						},
						{
							SourceLabels: model.LabelNames{model.LabelName("__meta_kubernetes_endpoint_port_name")},
							Separator:    ";",
							Regex:        relabel.MustNewRegexp("http"),
							Replacement:  "$$1",
							Action:       relabel.Keep,
						},
						{
							SourceLabels: model.LabelNames{model.LabelName("__meta_kubernetes_namespace")},
							Separator:    ";",
							Regex:        relabel.MustNewRegexp("(.*)"),
							TargetLabel:  "namespace",
							Replacement:  "$$1",
							Action:       relabel.Replace,
						},
						{
							SourceLabels: model.LabelNames{model.LabelName("__meta_kubernetes_service_name")},
							Separator:    ";",
							Regex:        relabel.MustNewRegexp("(.*)"),
							TargetLabel:  "service",
							Replacement:  "$$1",
							Action:       relabel.Replace,
						},
						{
							SourceLabels: model.LabelNames{model.LabelName("__meta_kubernetes_pod_name")},
							Separator:    ";",
							Regex:        relabel.MustNewRegexp("(.*)"),
							TargetLabel:  "pod",
							Replacement:  "$$1",
							Action:       relabel.Replace,
						},
						{
							SourceLabels: model.LabelNames{model.LabelName("__meta_kubernetes_pod_container_name")},
							Separator:    ";",
							Regex:        relabel.MustNewRegexp("(.*)"),
							TargetLabel:  "container",
							Replacement:  "$$1",
							Action:       relabel.Replace,
						},
					},
				},
			},
			expectedCode: http.StatusOK,
		},
		{
			description: "https secret handling",
			scrapeConfigs: map[string]*promconfig.ScrapeConfig{
				"serviceMonitor/testapp/testapp3/0": {
					JobName:         "serviceMonitor/testapp/testapp3/0",
					HonorTimestamps: true,
					ScrapeInterval:  model.Duration(30 * time.Second),
					ScrapeTimeout:   model.Duration(30 * time.Second),
					MetricsPath:     "/metrics",
					Scheme:          "http",
					HTTPClientConfig: config.HTTPClientConfig{
						FollowRedirects: true,
						BasicAuth: &config.BasicAuth{
							Username: "test",
							Password: "P@$$w0rd1!?",
						},
					},
				},
			},
			expectedCode: http.StatusOK,
			serverOptions: []Option{
				WithTLSConfig(tlsConfig, ""),
			},
		},
		{
			description: "http secret handling",
			scrapeConfigs: map[string]*promconfig.ScrapeConfig{
				"serviceMonitor/testapp/testapp3/0": {
					JobName:         "serviceMonitor/testapp/testapp3/0",
					HonorTimestamps: true,
					ScrapeInterval:  model.Duration(30 * time.Second),
					ScrapeTimeout:   model.Duration(30 * time.Second),
					MetricsPath:     "/metrics",
					Scheme:          "http",
					HTTPClientConfig: config.HTTPClientConfig{
						FollowRedirects: true,
						BasicAuth: &config.BasicAuth{
							Username: "test",
							Password: "P@$$w0rd1!?",
						},
					},
				},
			},
			expectedCode: http.StatusOK,
		},
	}
	for _, tc := range tests {
		t.Run(tc.description, func(t *testing.T) {
			listenAddr := ":8080"
			s := NewServer(logger, nil, listenAddr, tc.serverOptions...)
			assert.NoError(t, s.UpdateScrapeConfigResponse(tc.scrapeConfigs))

			request := httptest.NewRequest("GET", "/scrape_configs", nil)
			w := httptest.NewRecorder()

			if s.httpsServer != nil {
				request.TLS = &tls.ConnectionState{}
				s.httpsServer.Handler.ServeHTTP(w, request)
			} else {
				s.server.Handler.ServeHTTP(w, request)
			}
			result := w.Result()

			assert.Equal(t, tc.expectedCode, result.StatusCode)
			bodyBytes, err := io.ReadAll(result.Body)
			require.NoError(t, err)
			if tc.expectedBody != nil {
				assert.Equal(t, tc.expectedBody, bodyBytes)
				return
			}
			scrapeConfigs := map[string]*promconfig.ScrapeConfig{}
			err = yaml.Unmarshal(bodyBytes, scrapeConfigs)
			require.NoError(t, err)

			for _, c := range scrapeConfigs {
				if s.httpsServer == nil && c.HTTPClientConfig.BasicAuth != nil {
					assert.Equal(t, c.HTTPClientConfig.BasicAuth.Password, config.Secret("<secret>"))
				}
			}

			for _, c := range tc.scrapeConfigs {
				if s.httpsServer == nil && c.HTTPClientConfig.BasicAuth != nil {
					c.HTTPClientConfig.BasicAuth.Password = "<secret>"
				}
			}

			assert.Equal(t, tc.scrapeConfigs, scrapeConfigs)
		})
	}
}

func TestServer_JobHandler(t *testing.T) {
	tests := []struct {
		description  string
		targetItems  map[string]*target.Item
		expectedCode int
		expectedJobs map[string]linkJSON
	}{
		{
			description:  "nil jobs",
			targetItems:  nil,
			expectedCode: http.StatusOK,
			expectedJobs: make(map[string]linkJSON),
		},
		{
			description:  "empty jobs",
			targetItems:  map[string]*target.Item{},
			expectedCode: http.StatusOK,
			expectedJobs: make(map[string]linkJSON),
		},
		{
			description: "one job",
			targetItems: map[string]*target.Item{
				"targetitem": target.NewItem("job1", "", labels.Labels{}, ""),
			},
			expectedCode: http.StatusOK,
			expectedJobs: map[string]linkJSON{
				"job1": newLink("job1"),
			},
		},
		{
			description: "multiple jobs",
			targetItems: map[string]*target.Item{
				"a": target.NewItem("job1", "", labels.Labels{}, ""),
				"b": target.NewItem("job2", "", labels.Labels{}, ""),
				"c": target.NewItem("job3", "", labels.Labels{}, ""),
				"d": target.NewItem("job3", "", labels.Labels{}, ""),
				"e": target.NewItem("job3", "", labels.Labels{}, "")},
			expectedCode: http.StatusOK,
			expectedJobs: map[string]linkJSON{
				"job1": newLink("job1"),
				"job2": newLink("job2"),
				"job3": newLink("job3"),
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.description, func(t *testing.T) {
			listenAddr := ":8080"
			a := &mockAllocator{targetItems: tc.targetItems}
			s := NewServer(logger, a, listenAddr)
			request := httptest.NewRequest("GET", "/jobs", nil)
			w := httptest.NewRecorder()

			s.server.Handler.ServeHTTP(w, request)
			result := w.Result()

			assert.Equal(t, tc.expectedCode, result.StatusCode)
			bodyBytes, err := io.ReadAll(result.Body)
			require.NoError(t, err)
			jobs := map[string]linkJSON{}
			err = json.Unmarshal(bodyBytes, &jobs)
			require.NoError(t, err)
			assert.Equal(t, tc.expectedJobs, jobs)
		})
	}
}
func TestServer_JobsHandler_HTML(t *testing.T) {
	tests := []struct {
		description  string
		targetItems  map[string]*target.Item
		expectedCode int
		expectedJobs string
	}{
		{
			description:  "nil jobs",
			targetItems:  nil,
			expectedCode: http.StatusOK,
			expectedJobs: `<html>
<body>
<h1>Jobs</h1>
<table>
<thead><td>Job</td><td>Target Count</td></thead>
</table>
</body>
</html>`,
		},
		{
			description:  "empty jobs",
			targetItems:  map[string]*target.Item{},
			expectedCode: http.StatusOK,
			expectedJobs: `<html>
<body>
<h1>Jobs</h1>
<table>
<thead><td>Job</td><td>Target Count</td></thead>
</table>
</body>
</html>`,
		},
		{
			description: "one job",
			targetItems: map[string]*target.Item{
				"targetitem": target.NewItem("job1", "", labels.Labels{}, ""),
			},
			expectedCode: http.StatusOK,
			expectedJobs: `<html>
<body>
<h1>Jobs</h1>
<table>
<thead><td>Job</td><td>Target Count</td></thead>
<tr><td><a href="/job?job_id=job1">job1</a></td><td>1</td></tr>
</table>
</body>
</html>`,
		},
		{
			description: "multiple jobs",
			targetItems: map[string]*target.Item{
				"a": target.NewItem("job1", "1.1.1.1:8080", labels.Labels{}, ""),
				"b": target.NewItem("job2", "1.1.1.2:8080", labels.Labels{}, ""),
				"c": target.NewItem("job3", "1.1.1.3:8080", labels.Labels{}, ""),
				"d": target.NewItem("job3", "1.1.1.4:8080", labels.Labels{}, ""),
				"e": target.NewItem("job3", "1.1.1.5:8080", labels.Labels{}, "")},
			expectedCode: http.StatusOK,
			expectedJobs: `<html>
<body>
<h1>Jobs</h1>
<table>
<thead><td>Job</td><td>Target Count</td></thead>
<tr><td><a href="/job?job_id=job1">job1</a></td><td>1</td></tr>
<tr><td><a href="/job?job_id=job2">job2</a></td><td>1</td></tr>
<tr><td><a href="/job?job_id=job3">job3</a></td><td>3</td></tr>
</table>
</body>
</html>`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.description, func(t *testing.T) {
			listenAddr := ":8080"
			a := &mockAllocator{targetItems: tc.targetItems}
			s := NewServer(logger, a, listenAddr)
			a.SetCollectors(map[string]*allocation.Collector{
				"test-collector":  {Name: "test-collector"},
				"test-collector2": {Name: "test-collector2"},
			})
			request := httptest.NewRequest("GET", "/jobs", nil)
			request.Header.Set("Accept", "text/html")
			w := httptest.NewRecorder()

			s.server.Handler.ServeHTTP(w, request)
			result := w.Result()

			assert.Equal(t, tc.expectedCode, result.StatusCode)
			bodyBytes, err := io.ReadAll(result.Body)
			require.NoError(t, err)
			assert.Equal(t, tc.expectedJobs, string(bodyBytes))
		})
	}
}

func TestServer_JobHandler_HTML(t *testing.T) {
	consistentHashing, _ := allocation.New("consistent-hashing", logger)
	type args struct {
		job       string
		cMap      map[string]*target.Item
		allocator allocation.Allocator
	}
	type want struct {
		items     string
		errString string
	}
	tests := []struct {
		name string
		args args
		want want
	}{
		{
			name: "Empty target map",
			args: args{
				job:       "test-job",
				cMap:      map[string]*target.Item{},
				allocator: consistentHashing,
			},
			want: want{
				items: `<html>
<body>
<h1>Job: test-job</h1>
<table>
<thead><td>Collector</td><td>Target Count</td></thead>
<tr><td><a href="/collector?collector_id=test-collector">test-collector</a></td><td>0</td></tr>
<tr><td><a href="/collector?collector_id=test-collector2">test-collector2</a></td><td>0</td></tr>
</table>
<table>
<thead><td>Collector</td><td>Target</td></thead>
</table>
</body>
</html>`},
		},
		{
			name: "Single entry target map",
			args: args{
				job: "test-job",
				cMap: map[string]*target.Item{
					baseTargetItem.Hash(): baseTargetItem,
				},
				allocator: consistentHashing,
			},
			want: want{
				items: `<html>
<body>
<h1>Job: test-job</h1>
<table>
<thead><td>Collector</td><td>Target Count</td></thead>
<tr><td><a href="/collector?collector_id=test-collector">test-collector</a></td><td>0</td></tr>
<tr><td><a href="/collector?collector_id=test-collector2">test-collector2</a></td><td>1</td></tr>
</table>
<table>
<thead><td>Collector</td><td>Target</td></thead>
<tr><td><a href="/collector?collector_id=test-collector2">test-collector2</a></td><td><a href="/target?target_hash=test-jobtest-url6020141254647672168">test-url</a></td></tr>
</table>
</body>
</html>`,
			},
		},
		{
			name: "Multiple entry target map",
			args: args{
				job: "test-job",
				cMap: map[string]*target.Item{
					baseTargetItem.Hash():          baseTargetItem,
					testJobTwoTargetItemTwo.Hash(): testJobTwoTargetItemTwo,
				},
				allocator: consistentHashing,
			},
			want: want{
				items: `<html>
<body>
<h1>Job: test-job</h1>
<table>
<thead><td>Collector</td><td>Target Count</td></thead>
<tr><td><a href="/collector?collector_id=test-collector">test-collector</a></td><td>0</td></tr>
<tr><td><a href="/collector?collector_id=test-collector2">test-collector2</a></td><td>1</td></tr>
</table>
<table>
<thead><td>Collector</td><td>Target</td></thead>
<tr><td><a href="/collector?collector_id=test-collector2">test-collector2</a></td><td><a href="/target?target_hash=test-jobtest-url6020141254647672168">test-url</a></td></tr>
</table>
</body>
</html>`,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			listenAddr := ":8080"
			s := NewServer(logger, tt.args.allocator, listenAddr)
			tt.args.allocator.SetCollectors(map[string]*allocation.Collector{
				"test-collector":  {Name: "test-collector"},
				"test-collector2": {Name: "test-collector2"},
			})
			tt.args.allocator.SetTargets(tt.args.cMap)
			request := httptest.NewRequest("GET", fmt.Sprintf("/job?job_id=%s", tt.args.job), nil)
			request.Header.Set("Accept", "text/html")
			w := httptest.NewRecorder()

			s.server.Handler.ServeHTTP(w, request)
			result := w.Result()

			assert.Equal(t, http.StatusOK, result.StatusCode)
			body := result.Body
			bodyBytes, err := io.ReadAll(body)
			assert.NoError(t, err)
			if len(tt.want.errString) != 0 {
				assert.EqualError(t, err, tt.want.errString)
				return
			}
			assert.Equal(t, tt.want.items, string(bodyBytes))
		})
	}
}

func TestServer_IndexHandler(t *testing.T) {
	allocator, _ := allocation.New("consistent-hashing", logger)
	tests := []struct {
		description  string
		allocator    allocation.Allocator
		targetItems  map[string]*target.Item
		expectedHTML string
	}{
		{
			description: "Empty target map",
			targetItems: map[string]*target.Item{},
			allocator:   allocator,
			expectedHTML: strings.Trim(`
<html>
<body>
<h1>OpenTelemetry Target Allocator</h1>
<table>
<thead><td>Category</td><td>Count</td></thead>
<tr><td><a href="/jobs">Jobs</a></td><td>0</td></tr>
<tr><td><a href="/targets">Targets</a></td><td>0</td></tr>
</table>
<table>
<thead><td>Collector</td><td>Job Count</td><td>Target Count</td></thead>
<tr><td><a href="/collector?collector_id=test-collector1">test-collector1</a></td><td>0</td><td>0</td></tr>
<tr><td><a href="/collector?collector_id=test-collector2">test-collector2</a></td><td>0</td><td>0</td></tr>
</table>
</body>
</html>
`, "\n"),
		},
		{
			description: "Single entry target map",
			targetItems: map[string]*target.Item{
				baseTargetItem.Hash(): baseTargetItem,
			},
			allocator: allocator,
			expectedHTML: strings.Trim(`
<html>
<body>
<h1>OpenTelemetry Target Allocator</h1>
<table>
<thead><td>Category</td><td>Count</td></thead>
<tr><td><a href="/jobs">Jobs</a></td><td>1</td></tr>
<tr><td><a href="/targets">Targets</a></td><td>1</td></tr>
</table>
<table>
<thead><td>Collector</td><td>Job Count</td><td>Target Count</td></thead>
<tr><td><a href="/collector?collector_id=test-collector1">test-collector1</a></td><td>1</td><td>1</td></tr>
<tr><td><a href="/collector?collector_id=test-collector2">test-collector2</a></td><td>0</td><td>0</td></tr>
</table>
</body>
</html>
`, "\n"),
		},
		{
			description: "Multiple entry target map",
			targetItems: map[string]*target.Item{
				baseTargetItem.Hash():          baseTargetItem,
				testJobTargetItemTwo.Hash():    testJobTargetItemTwo,
				testJobTwoTargetItemTwo.Hash(): testJobTwoTargetItemTwo,
			},
			allocator: allocator,
			expectedHTML: strings.Trim(`
<html>
<body>
<h1>OpenTelemetry Target Allocator</h1>
<table>
<thead><td>Category</td><td>Count</td></thead>
<tr><td><a href="/jobs">Jobs</a></td><td>2</td></tr>
<tr><td><a href="/targets">Targets</a></td><td>3</td></tr>
</table>
<table>
<thead><td>Collector</td><td>Job Count</td><td>Target Count</td></thead>
<tr><td><a href="/collector?collector_id=test-collector1">test-collector1</a></td><td>2</td><td>2</td></tr>
<tr><td><a href="/collector?collector_id=test-collector2">test-collector2</a></td><td>1</td><td>1</td></tr>
</table>
</body>
</html>
`, "\n"),
		},
	}
	for _, tc := range tests {
		t.Run(tc.description, func(t *testing.T) {
			listenAddr := ":8080"
			s := NewServer(logger, tc.allocator, listenAddr)
			tc.allocator.SetCollectors(map[string]*allocation.Collector{
				"test-collector1": {Name: "test-collector1"},
				"test-collector2": {Name: "test-collector2"},
			})
			tc.allocator.SetTargets(tc.targetItems)
			request := httptest.NewRequest("GET", "/", nil)
			request.Header.Set("Accept", "text/html")
			w := httptest.NewRecorder()

			s.server.Handler.ServeHTTP(w, request)
			result := w.Result()

			assert.Equal(t, http.StatusOK, result.StatusCode)
			body := result.Body
			bodyBytes, err := io.ReadAll(body)
			assert.NoError(t, err)
			assert.Equal(t, tc.expectedHTML, string(bodyBytes))
		})
	}
}
func TestServer_TargetsHTMLHandler(t *testing.T) {
	allocator, _ := allocation.New("consistent-hashing", logger)
	tests := []struct {
		description  string
		allocator    allocation.Allocator
		targetItems  map[string]*target.Item
		expectedHTML string
	}{
		{
			description: "Empty target map",
			targetItems: map[string]*target.Item{},
			allocator:   allocator,
			expectedHTML: `<html>
<body>
<h1>Targets</h1>
<table>
<thead><td>Job</td><td>Target</td><td>Collector</td><td>Endpoint Slice</td></thead>
</table>
</body>
</html>`,
		},
		{
			description: "Single entry target map",
			targetItems: map[string]*target.Item{
				baseTargetItem.Hash(): baseTargetItem,
			},
			allocator: allocator,
			expectedHTML: `<html>
<body>
<h1>Targets</h1>
<table>
<thead><td>Job</td><td>Target</td><td>Collector</td><td>Endpoint Slice</td></thead>
<tr><td><a href="/job?job_id=test-job">test-job</a></td><td><a href="/target?target_hash=test-jobtest-url6020141254647672168">test-url</a></td><td><a href="/collector?collector_id=test-collector1">test-collector1</a></td><td></td></tr>
</table>
</body>
</html>`,
		},
		{
			description: "Multiple entry target map",
			targetItems: map[string]*target.Item{
				baseTargetItem.Hash():          baseTargetItem,
				testJobTargetItemTwo.Hash():    testJobTargetItemTwo,
				testJobTwoTargetItemTwo.Hash(): testJobTwoTargetItemTwo,
			},
			allocator: allocator,
			expectedHTML: `<html>
<body>
<h1>Targets</h1>
<table>
<thead><td>Job</td><td>Target</td><td>Collector</td><td>Endpoint Slice</td></thead>
<tr><td><a href="/job?job_id=test-job2">test-job2</a></td><td><a href="/target?target_hash=test-job2test-url31177771863092489626">test-url3</a></td><td><a href="/collector?collector_id=test-collector1">test-collector1</a></td><td></td></tr>
<tr><td><a href="/job?job_id=test-job">test-job</a></td><td><a href="/target?target_hash=test-jobtest-url21177771863092489626">test-url2</a></td><td><a href="/collector?collector_id=test-collector2">test-collector2</a></td><td></td></tr>
<tr><td><a href="/job?job_id=test-job">test-job</a></td><td><a href="/target?target_hash=test-jobtest-url6020141254647672168">test-url</a></td><td><a href="/collector?collector_id=test-collector1">test-collector1</a></td><td></td></tr>
</table>
</body>
</html>`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.description, func(t *testing.T) {
			listenAddr := ":8080"
			s := NewServer(logger, tc.allocator, listenAddr)
			tc.allocator.SetCollectors(map[string]*allocation.Collector{
				"test-collector1": {Name: "test-collector1"},
				"test-collector2": {Name: "test-collector2"},
			})
			tc.allocator.SetTargets(tc.targetItems)
			request := httptest.NewRequest("GET", "/targets", nil)
			request.Header.Set("Accept", "text/html")
			w := httptest.NewRecorder()

			s.server.Handler.ServeHTTP(w, request)
			result := w.Result()

			assert.Equal(t, http.StatusOK, result.StatusCode)
			body := result.Body
			bodyBytes, err := io.ReadAll(body)
			assert.NoError(t, err)
			assert.Equal(t, tc.expectedHTML, string(bodyBytes))
		})
	}
}

func TestServer_CollectorHandler(t *testing.T) {
	allocator, _ := allocation.New("consistent-hashing", logger)
	tests := []struct {
		description  string
		collectorId  string
		allocator    allocation.Allocator
		targetItems  map[string]*target.Item
		expectedCode int
		expectedHTML string
	}{
		{
			description:  "Empty target map",
			collectorId:  "test-collector",
			targetItems:  map[string]*target.Item{},
			allocator:    allocator,
			expectedCode: http.StatusOK,
			expectedHTML: `<html>
<body>
<h1>Collector: test-collector</h1>
<table>
<thead><td>Job</td><td>Target</td><td>Endpoint Slice</td></thead>
</table>
</body>
</html>`,
		},
		{
			description: "Single entry target map",
			collectorId: "test-collector2",
			targetItems: map[string]*target.Item{
				baseTargetItem.Hash(): baseTargetItem,
			},
			allocator:    allocator,
			expectedCode: http.StatusOK,
			expectedHTML: `<html>
<body>
<h1>Collector: test-collector2</h1>
<table>
<thead><td>Job</td><td>Target</td><td>Endpoint Slice</td></thead>
<tr><td><a href="/job?job_id=test-job">test-job</a></td><td><a href="/target?target_hash=test-jobtest-url6020141254647672168">test-url</a></td><td></td></tr>
</table>
</body>
</html>`,
		},
		{
			description: "Multiple entry target map",
			collectorId: "test-collector2",
			targetItems: map[string]*target.Item{
				baseTargetItem.Hash():          baseTargetItem,
				testJobTwoTargetItemTwo.Hash(): testJobTwoTargetItemTwo,
			},
			allocator:    allocator,
			expectedCode: http.StatusOK,
			expectedHTML: `<html>
<body>
<h1>Collector: test-collector2</h1>
<table>
<thead><td>Job</td><td>Target</td><td>Endpoint Slice</td></thead>
<tr><td><a href="/job?job_id=test-job">test-job</a></td><td><a href="/target?target_hash=test-jobtest-url6020141254647672168">test-url</a></td><td></td></tr>
</table>
</body>
</html>`,
		},
		{
			description: "Multiple entry target map, collector id is empty",
			collectorId: "",
			targetItems: map[string]*target.Item{
				baseTargetItem.Hash():          baseTargetItem,
				testJobTwoTargetItemTwo.Hash(): testJobTwoTargetItemTwo,
			},
			allocator:    allocator,
			expectedCode: http.StatusBadRequest,
			expectedHTML: `<html>
<body>
<h1>Bad Request</h1>
<p>Expected collector_id in the query string</p>
<p>Example: /collector?collector_id=my-collector-42</p>
</body>
</html>`,
		},
		{
			description: "Multiple entry target map, unknown collector id",
			collectorId: "unknown-collector-1",
			targetItems: map[string]*target.Item{
				baseTargetItem.Hash():          baseTargetItem,
				testJobTwoTargetItemTwo.Hash(): testJobTwoTargetItemTwo,
			},
			allocator:    allocator,
			expectedCode: http.StatusNotFound,
			expectedHTML: `<html>
<body>
<h1>Unknown Collector: unknown-collector-1</h1>
</body>
</html>`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.description, func(t *testing.T) {
			listenAddr := ":8080"
			s := NewServer(logger, tc.allocator, listenAddr)
			tc.allocator.SetCollectors(map[string]*allocation.Collector{
				"test-collector":  {Name: "test-collector"},
				"test-collector2": {Name: "test-collector2"},
			})
			tc.allocator.SetTargets(tc.targetItems)
			request := httptest.NewRequest("GET", "/collector", nil)
			request.Header.Set("Accept", "text/html")
			request.URL.RawQuery = "collector_id=" + tc.collectorId
			w := httptest.NewRecorder()

			s.server.Handler.ServeHTTP(w, request)
			result := w.Result()

			assert.Equal(t, tc.expectedCode, result.StatusCode)
			body := result.Body
			bodyBytes, err := io.ReadAll(body)
			assert.NoError(t, err)
			assert.Equal(t, tc.expectedHTML, string(bodyBytes))
		})
	}
}

func TestServer_TargetHTMLHandler(t *testing.T) {
	allocator, _ := allocation.New("consistent-hashing", logger)
	tests := []struct {
		description  string
		targetHash   string
		allocator    allocation.Allocator
		targetItems  map[string]*target.Item
		expectedCode int
		expectedHTML string
	}{
		{
			description:  "Missing target hash",
			targetHash:   "",
			targetItems:  map[string]*target.Item{},
			allocator:    allocator,
			expectedCode: http.StatusBadRequest,
			expectedHTML: `<html>
<body>
<h1>Bad Request</h1>
<p>Expected target_hash in the query string</p>
<p>Example: /target?target_hash=my-target-42</p>
</body>
</html>`,
		},
		{
			description: "Single entry target map",
			targetHash:  baseTargetItem.Hash(),
			targetItems: map[string]*target.Item{
				baseTargetItem.Hash(): baseTargetItem,
			},
			allocator:    allocator,
			expectedCode: http.StatusOK,
			expectedHTML: `<html>
<body>
<h1>Target: test-url</h1>
<table>
<tr><td>Collector</td><td>test-collector2</td></tr>
<tr><td>Job</td><td>test-job</td></tr>
</table>
<h2>Target Labels</h2>
<table>
<thead><td>Label</td><td>Value</td></thead>
<tr><td>test_label</td><td>test-value</td></tr>
</table>
</body>
</html>`,
		},
		{
			description: "Multiple entry target map",
			targetHash:  testJobTwoTargetItemTwo.Hash(),
			targetItems: map[string]*target.Item{
				baseTargetItem.Hash():          baseTargetItem,
				testJobTwoTargetItemTwo.Hash(): testJobTwoTargetItemTwo,
			},
			allocator:    allocator,
			expectedCode: http.StatusOK,
			expectedHTML: `<html>
<body>
<h1>Target: test-url3</h1>
<table>
<tr><td>Collector</td><td>test-collector</td></tr>
<tr><td>Job</td><td>test-job2</td></tr>
</table>
<h2>Target Labels</h2>
<table>
<thead><td>Label</td><td>Value</td></thead>
<tr><td>test_label</td><td>test-value2</td></tr>
</table>
</body>
</html>`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.description, func(t *testing.T) {
			listenAddr := ":8080"
			s := NewServer(logger, tc.allocator, listenAddr)
			tc.allocator.SetCollectors(map[string]*allocation.Collector{
				"test-collector":  {Name: "test-collector"},
				"test-collector2": {Name: "test-collector2"},
			})
			tc.allocator.SetTargets(tc.targetItems)
			request := httptest.NewRequest("GET", "/target", nil)
			request.Header.Set("Accept", "text/html")
			request.URL.RawQuery = "target_hash=" + tc.targetHash
			w := httptest.NewRecorder()

			s.server.Handler.ServeHTTP(w, request)
			result := w.Result()

			assert.Equal(t, tc.expectedCode, result.StatusCode)
			body := result.Body
			bodyBytes, err := io.ReadAll(body)
			assert.NoError(t, err)
			assert.Equal(t, tc.expectedHTML, string(bodyBytes))
		})
	}
}

func TestServer_Readiness(t *testing.T) {
	tests := []struct {
		description   string
		scrapeConfigs map[string]*promconfig.ScrapeConfig
		expectedCode  int
		expectedBody  []byte
	}{
		{
			description:   "nil scrape config",
			scrapeConfigs: nil,
			expectedCode:  http.StatusServiceUnavailable,
		},
		{
			description:   "empty scrape config",
			scrapeConfigs: map[string]*promconfig.ScrapeConfig{},
			expectedCode:  http.StatusOK,
		},
		{
			description: "single entry",
			scrapeConfigs: map[string]*promconfig.ScrapeConfig{
				"serviceMonitor/testapp/testapp/0": {
					JobName:         "serviceMonitor/testapp/testapp/0",
					HonorTimestamps: true,
					ScrapeInterval:  model.Duration(30 * time.Second),
					ScrapeTimeout:   model.Duration(30 * time.Second),
					MetricsPath:     "/metrics",
					Scheme:          "http",
					HTTPClientConfig: config.HTTPClientConfig{
						FollowRedirects: true,
					},
					RelabelConfigs: []*relabel.Config{
						{
							SourceLabels: model.LabelNames{model.LabelName("job")},
							Separator:    ";",
							Regex:        relabel.MustNewRegexp("(.*)"),
							TargetLabel:  "__tmp_prometheus_job_name",
							Replacement:  "$$1",
							Action:       relabel.Replace,
						},
					},
				},
			},
			expectedCode: http.StatusOK,
		},
	}
	for _, tc := range tests {
		t.Run(tc.description, func(t *testing.T) {
			listenAddr := ":8080"
			s := NewServer(logger, nil, listenAddr)
			if tc.scrapeConfigs != nil {
				assert.NoError(t, s.UpdateScrapeConfigResponse(tc.scrapeConfigs))
			}

			request := httptest.NewRequest("GET", "/readyz", nil)
			w := httptest.NewRecorder()

			s.server.Handler.ServeHTTP(w, request)
			result := w.Result()

			assert.Equal(t, tc.expectedCode, result.StatusCode)
		})
	}
}

func TestServer_ScrapeConfigRespose(t *testing.T) {
	tests := []struct {
		description  string
		filePath     string
		expectedCode int
	}{
		{
			description:  "Jobs with all actions",
			filePath:     "./testdata/prom-config-all-actions.yaml",
			expectedCode: http.StatusOK,
		},
		{
			description:  "Jobs with config combinations",
			filePath:     "./testdata/prom-config-test.yaml",
			expectedCode: http.StatusOK,
		},
		{
			description:  "Jobs with no config",
			filePath:     "./testdata/prom-no-config.yaml",
			expectedCode: http.StatusOK,
		},
	}
	for _, tc := range tests {
		t.Run(tc.description, func(t *testing.T) {
			listenAddr := ":8080"
			s := NewServer(logger, nil, listenAddr)

			allocCfg := allocatorconfig.CreateDefaultConfig()
			err := allocatorconfig.LoadFromFile(tc.filePath, &allocCfg)
			require.NoError(t, err)

			jobToScrapeConfig := make(map[string]*promconfig.ScrapeConfig)

			for _, scrapeConfig := range allocCfg.PromConfig.ScrapeConfigs {
				jobToScrapeConfig[scrapeConfig.JobName] = scrapeConfig
			}

			assert.NoError(t, s.UpdateScrapeConfigResponse(jobToScrapeConfig))

			request := httptest.NewRequest("GET", "/scrape_configs", nil)
			w := httptest.NewRecorder()

			s.server.Handler.ServeHTTP(w, request)
			result := w.Result()

			assert.Equal(t, tc.expectedCode, result.StatusCode)
			bodyBytes, err := io.ReadAll(result.Body)
			require.NoError(t, err)

			// Checking to make sure yaml unmarshaling doesn't result in errors for responses containing any supported prometheus relabel action
			scrapeConfigs := map[string]*promconfig.ScrapeConfig{}
			err = yaml.Unmarshal(bodyBytes, scrapeConfigs)
			require.NoError(t, err)
		})
	}
}

func newLink(jobName string) linkJSON {
	return linkJSON{Link: fmt.Sprintf("/jobs/%s/targets", url.QueryEscape(jobName))}
}
