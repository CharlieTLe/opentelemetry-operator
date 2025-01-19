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
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"net/http/pprof"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	yaml2 "github.com/ghodss/yaml"
	"github.com/gin-gonic/gin"
	"github.com/go-logr/logr"
	jsoniter "github.com/json-iterator/go"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	promcommconfig "github.com/prometheus/common/config"
	promconfig "github.com/prometheus/prometheus/config"
	"github.com/prometheus/prometheus/model/labels"
	"gopkg.in/yaml.v2"

	"github.com/open-telemetry/opentelemetry-operator/cmd/otel-allocator/allocation"
	"github.com/open-telemetry/opentelemetry-operator/cmd/otel-allocator/target"
)

var (
	httpDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name: "opentelemetry_allocator_http_duration_seconds",
		Help: "Duration of received HTTP requests.",
	}, []string{"path"})
)

var (
	jsonConfig = jsoniter.Config{
		EscapeHTML:                    false,
		MarshalFloatWith6Digits:       true,
		ObjectFieldMustBeSimpleString: true,
	}.Froze()
)

type collectorJSON struct {
	Link string        `json:"_link"`
	Jobs []*targetJSON `json:"targets"`
}

type linkJSON struct {
	Link string `json:"_link"`
}

type targetJSON struct {
	TargetURL []string      `json:"targets"`
	Labels    labels.Labels `json:"labels"`
}

type Server struct {
	logger         logr.Logger
	allocator      allocation.Allocator
	server         *http.Server
	httpsServer    *http.Server
	jsonMarshaller jsoniter.API

	// Use RWMutex to protect scrapeConfigResponse, since it
	// will be predominantly read and only written when config
	// is applied.
	mtx                                  sync.RWMutex
	scrapeConfigResponse                 []byte
	ScrapeConfigMarshalledSecretResponse []byte
}

type Option func(*Server)

// Option to create an additional https server with mTLS configuration.
// Used for getting the scrape config with real secret values.
func WithTLSConfig(tlsConfig *tls.Config, httpsListenAddr string) Option {
	return func(s *Server) {
		httpsRouter := gin.New()
		s.setRouter(httpsRouter)

		s.httpsServer = &http.Server{Addr: httpsListenAddr, Handler: httpsRouter, ReadHeaderTimeout: 90 * time.Second, TLSConfig: tlsConfig}
	}
}

func (s *Server) setRouter(router *gin.Engine) {
	router.Use(gin.Recovery())
	router.UseRawPath = true
	router.UnescapePathValues = false
	router.Use(s.PrometheusMiddleware)

	router.GET("/", s.IndexHandler)
	router.GET("/collector", s.CollectorHTMLHandler)
	router.GET("/job", s.JobHTMLHandler)
	router.GET("/target", s.TargetHTMLHandler)
	router.GET("/targets", s.TargetsHTMLHandler)
	router.GET("/scrape_configs", s.ScrapeConfigsHandler)
	router.GET("/jobs", s.JobsHandler)
	router.GET("/jobs/:job_id/targets", s.TargetsHandler)
	router.GET("/metrics", gin.WrapH(promhttp.Handler()))
	router.GET("/livez", s.LivenessProbeHandler)
	router.GET("/readyz", s.ReadinessProbeHandler)
	registerPprof(router.Group("/debug/pprof/"))
}

func NewServer(log logr.Logger, allocator allocation.Allocator, listenAddr string, options ...Option) *Server {
	s := &Server{
		logger:         log,
		allocator:      allocator,
		jsonMarshaller: jsonConfig,
	}

	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	s.setRouter(router)

	s.server = &http.Server{Addr: listenAddr, Handler: router, ReadHeaderTimeout: 90 * time.Second}

	for _, opt := range options {
		opt(s)
	}

	return s
}

func (s *Server) Start() error {
	s.logger.Info("Starting server...")
	return s.server.ListenAndServe()
}

func (s *Server) Shutdown(ctx context.Context) error {
	s.logger.Info("Shutting down server...")
	return s.server.Shutdown(ctx)
}

func (s *Server) StartHTTPS() error {
	s.logger.Info("Starting HTTPS server...")
	return s.httpsServer.ListenAndServeTLS("", "")
}

func (s *Server) ShutdownHTTPS(ctx context.Context) error {
	s.logger.Info("Shutting down HTTPS server...")
	return s.httpsServer.Shutdown(ctx)
}

// RemoveRegexFromRelabelAction is needed specifically for keepequal/dropequal actions because even though the user doesn't specify the
// regex field for these actions the unmarshalling implementations of prometheus adds back the default regex fields
// which in turn causes the receiver to error out since the unmarshaling of the json response doesn't expect anything in the regex fields
// for these actions. Adding this as a fix until the original issue with prometheus unmarshaling is fixed -
// https://github.com/prometheus/prometheus/issues/12534
func RemoveRegexFromRelabelAction(jsonConfig []byte) ([]byte, error) {
	var jobToScrapeConfig map[string]interface{}
	err := json.Unmarshal(jsonConfig, &jobToScrapeConfig)
	if err != nil {
		return nil, err
	}
	for _, scrapeConfig := range jobToScrapeConfig {
		scrapeConfig := scrapeConfig.(map[string]interface{})
		if scrapeConfig["relabel_configs"] != nil {
			relabelConfigs := scrapeConfig["relabel_configs"].([]interface{})
			for _, relabelConfig := range relabelConfigs {
				relabelConfig := relabelConfig.(map[string]interface{})
				// Dropping regex key from the map since unmarshalling this on the client(metrics_receiver.go) results in error
				// because of the bug here - https://github.com/prometheus/prometheus/issues/12534
				if relabelConfig["action"] == "keepequal" || relabelConfig["action"] == "dropequal" {
					delete(relabelConfig, "regex")
				}
			}
		}
		if scrapeConfig["metric_relabel_configs"] != nil {
			metricRelabelConfigs := scrapeConfig["metric_relabel_configs"].([]interface{})
			for _, metricRelabelConfig := range metricRelabelConfigs {
				metricRelabelConfig := metricRelabelConfig.(map[string]interface{})
				// Dropping regex key from the map since unmarshalling this on the client(metrics_receiver.go) results in error
				// because of the bug here - https://github.com/prometheus/prometheus/issues/12534
				if metricRelabelConfig["action"] == "keepequal" || metricRelabelConfig["action"] == "dropequal" {
					delete(metricRelabelConfig, "regex")
				}
			}
		}
	}

	jsonConfigNew, err := json.Marshal(jobToScrapeConfig)
	if err != nil {
		return nil, err
	}
	return jsonConfigNew, nil
}

func (s *Server) MarshalScrapeConfig(configs map[string]*promconfig.ScrapeConfig, marshalSecretValue bool) error {
	promcommconfig.MarshalSecretValue = marshalSecretValue

	configBytes, err := yaml.Marshal(configs)
	if err != nil {
		return err
	}

	var jsonConfig []byte
	jsonConfig, err = yaml2.YAMLToJSON(configBytes)
	if err != nil {
		return err
	}

	jsonConfigNew, err := RemoveRegexFromRelabelAction(jsonConfig)
	if err != nil {
		return err
	}

	s.mtx.Lock()
	if marshalSecretValue {
		s.ScrapeConfigMarshalledSecretResponse = jsonConfigNew
	} else {
		s.scrapeConfigResponse = jsonConfigNew
	}
	s.mtx.Unlock()

	return nil
}

// UpdateScrapeConfigResponse updates the scrape config response. The target allocator first marshals these
// configurations such that the underlying prometheus marshaling is used. After that, the YAML is converted
// in to a JSON format for consumers to use.
func (s *Server) UpdateScrapeConfigResponse(configs map[string]*promconfig.ScrapeConfig) error {
	err := s.MarshalScrapeConfig(configs, false)
	if err != nil {
		return err
	}
	err = s.MarshalScrapeConfig(configs, true)
	if err != nil {
		return err
	}
	return nil
}

// ScrapeConfigsHandler returns the available scrape configuration discovered by the target allocator.
func (s *Server) ScrapeConfigsHandler(c *gin.Context) {
	s.mtx.RLock()
	result := s.scrapeConfigResponse
	if c.Request.TLS != nil {
		result = s.ScrapeConfigMarshalledSecretResponse
	}
	s.mtx.RUnlock()

	// We don't use the jsonHandler method because we don't want our bytes to be re-encoded
	c.Writer.Header().Set("Content-Type", "application/json")
	_, err := c.Writer.Write(result)
	if err != nil {
		s.errorHandler(c.Writer, err)
	}
}

func (s *Server) ReadinessProbeHandler(c *gin.Context) {
	s.mtx.RLock()
	result := s.scrapeConfigResponse
	s.mtx.RUnlock()

	if result != nil {
		c.Status(http.StatusOK)
	} else {
		c.Status(http.StatusServiceUnavailable)
	}
}

func (s *Server) JobsHandler(c *gin.Context) {
	displayData := make(map[string]linkJSON)
	for _, v := range s.allocator.TargetItems() {
		displayData[v.JobName] = linkJSON{Link: fmt.Sprintf("/jobs/%s/targets", url.QueryEscape(v.JobName))}
	}
	if strings.Contains(c.Request.Header.Get("Accept"), "text/html") {
		s.JobsHTMLHandler(c)
		return
	}
	s.jsonHandler(c.Writer, displayData)
}

func (s *Server) LivenessProbeHandler(c *gin.Context) {
	c.Status(http.StatusOK)
}

func (s *Server) PrometheusMiddleware(c *gin.Context) {
	path := c.FullPath()
	timer := prometheus.NewTimer(httpDuration.WithLabelValues(path))
	c.Next()
	timer.ObserveDuration()
}

func header(data ...string) string {
	return "<thead><td>" + strings.Join(data, "</td><td>") + "</td></thead>\n"
}

func row(data ...string) string {
	return "<tr><td>" + strings.Join(data, "</td><td>") + "</td></tr>\n"
}

// IndexHandler displays the main page of the allocator. It shows the number of jobs and targets.
// It also displays a table with the collectors and the number of jobs and targets for each collector.
// The collector names are links to the respective pages. The table is sorted by collector name.
func (s *Server) IndexHandler(c *gin.Context) {
	c.Writer.Header().Set("Content-Type", "text/html")
	var b bytes.Buffer
	b.WriteString(`<html>
<body>
<h1>OpenTelemetry Target Allocator</h1>
`)

	fmt.Fprint(&b, "<table>\n")
	fmt.Fprint(&b, header("Category", "Count"))
	fmt.Fprint(&b, row(jobsAnchorLink(), strconv.Itoa(s.getJobCount())))
	fmt.Fprint(&b, row(targetsAnchorLink(), strconv.Itoa(len(s.allocator.TargetItems()))))
	fmt.Fprint(&b, "</table>\n")

	fmt.Fprint(&b, "<table>\n")
	fmt.Fprint(&b, header("Collector", "Job Count", "Target Count"))

	// Sort the collectors by name to ensure consistent order
	collectorNames := []string{}
	for k := range s.allocator.Collectors() {
		collectorNames = append(collectorNames, k)
	}
	sort.Strings(collectorNames)

	for _, colName := range collectorNames {
		jobCount := strconv.Itoa(s.getJobCountForCollector(colName))
		targetCount := strconv.Itoa(s.getTargetCountForCollector(colName))
		fmt.Fprint(&b, row(collectorAnchorLink(colName), jobCount, targetCount))
	}
	b.WriteString(`</table>
</body>
</html>`)

	_, err := c.Writer.Write(b.Bytes())
	if err != nil {
		s.logger.Error(err, "failed to write response")
		c.Status(http.StatusInternalServerError)
	}

	c.Status(http.StatusOK)
}

func targetsAnchorLink() string {
	return `<a href="/targets">Targets</a>`
}

// TargetsHTMLHandler displays the targets in a table format. Each target is a row in the table.
// The table has four columns: Job, Target, Collector, and Endpoint Slice.
// The Job, Target, and Collector columns are links to the respective pages.
func (s *Server) TargetsHTMLHandler(c *gin.Context) {
	c.Writer.Header().Set("X-Content-Type-Options", "nosniff")
	c.Writer.Header().Set("Content-Type", "text/html; charset=utf-8")

	var b bytes.Buffer
	b.WriteString(`<html>
<body>
<h1>Targets</h1>
<table>
`)
	fmt.Fprint(&b, header("Job", "Target", "Collector", "Endpoint Slice"))
	for _, v := range s.sortedTargetItems() {
		fmt.Fprint(&b, row(jobAnchorLink(v.JobName), targetAnchorLink(v), collectorAnchorLink(v.CollectorName), v.GetEndpointSliceName()))
	}

	b.WriteString(`</table>
</body>
</html>`)

	_, err := c.Writer.Write(b.Bytes())
	if err != nil {
		s.logger.Error(err, "failed to write response")
		c.Status(http.StatusInternalServerError)
	}

	c.Status(http.StatusOK)
}

func targetAnchorLink(t *target.Item) string {
	return fmt.Sprintf("<a href=\"/target?target_hash=%s\">%s</a>", t.Hash(), t.TargetURL)
}

// TargetHTMLHandler displays information about a target in a table format.
// There are two tables: one for high-level target information and another for the target's labels.
func (s *Server) TargetHTMLHandler(c *gin.Context) {
	c.Writer.Header().Set("X-Content-Type-Options", "nosniff")
	c.Writer.Header().Set("Content-Type", "text/html; charset=utf-8")

	targetHash := c.Request.URL.Query().Get("target_hash")
	if targetHash == "" {
		c.Status(http.StatusBadRequest)
		_, err := c.Writer.WriteString(`<html>
<body>
<h1>Bad Request</h1>
<p>Expected target_hash in the query string</p>
<p>Example: /target?target_hash=my-target-42</p>
</body>
</html>`)
		if err != nil {
			s.logger.Error(err, "failed to write response")
		}
		return
	}

	target, found := s.allocator.TargetItems()[targetHash]
	if !found {
		c.Status(http.StatusNotFound)
		t, err := template.New("unknown_target").Parse(`<html>
<body>
<h1>Unknown Target: {{.}}</h1>
</body>
</html>`)
		if err != nil {
			s.logger.Error(err, "failed to parse template")
		}
		err = t.Execute(c.Writer, targetHash)
		if err != nil {
			s.logger.Error(err, "failed to write response")
		}
		return
	}

	var b bytes.Buffer
	b.WriteString(`<html>
<body>
<h1>Target: ` + target.TargetURL + `</h1>
<table>
`)

	fmt.Fprint(&b, row("Collector", target.CollectorName))
	fmt.Fprint(&b, row("Job", target.JobName))
	if namespace := target.Labels.Get("__meta_kubernetes_namespace"); namespace != "" {
		fmt.Fprint(&b, row("Namespace", namespace))
	}
	if service := target.Labels.Get("__meta_kubernetes_service_name"); service != "" {
		fmt.Fprint(&b, row("Service Name", service))
	}
	if port := target.Labels.Get("__meta_kubernetes_service_port"); port != "" {
		fmt.Fprint(&b, row("Service Port", port))
	}
	if podName := target.Labels.Get("__meta_kubernetes_pod_name"); podName != "" {
		fmt.Fprint(&b, row("Pod Name", podName))
	}
	if container := target.Labels.Get("__meta_kubernetes_pod_container_name"); container != "" {
		fmt.Fprint(&b, row("Container Name", container))
	}
	if containerPortName := target.Labels.Get("__meta_kubernetes_pod_container_port_name"); containerPortName != "" {
		fmt.Fprint(&b, row("Container Port Name", containerPortName))
	}
	if node := target.GetNodeName(); node != "" {
		fmt.Fprint(&b, row("Node Name", node))
	}
	if endpointSliceName := target.GetEndpointSliceName(); endpointSliceName != "" {
		fmt.Fprint(&b, row("Endpoint Slice Name", endpointSliceName))
	}

	b.WriteString(`</table>
<h2>Target Labels</h2>
<table>
`)
	fmt.Fprint(&b, header("Label", "Value"))
	for _, l := range target.Labels {
		fmt.Fprint(&b, row(l.Name, l.Value))
	}
	b.WriteString(`</table>
</body>
</html>`)
	_, err := c.Writer.Write(b.Bytes())
	if err != nil {
		s.logger.Error(err, "failed to write response")
		c.Status(http.StatusInternalServerError)
	}

	c.Status(http.StatusOK)
}

func jobsAnchorLink() string {
	return `<a href="/jobs">Jobs</a>`
}

// JobsHTMLHandler displays the jobs in a table format. Each job is a row in the table.
// The table has two columns: Job and Target Count. The Job column is a link to the job's targets.
func (s *Server) JobsHTMLHandler(c *gin.Context) {
	c.Writer.Header().Set("X-Content-Type-Options", "nosniff")
	c.Writer.Header().Set("Content-Type", "text/html; charset=utf-8")

	var b bytes.Buffer
	b.WriteString(`<html>
<body>
<h1>Jobs</h1>
<table>
`)
	fmt.Fprint(&b, header("Job", "Target Count"))

	jobs := make(map[string]int)
	for _, v := range s.allocator.TargetItems() {
		jobs[v.JobName]++
	}

	// Sort the jobs by name to ensure consistent order
	jobNames := make([]string, 0, len(jobs))
	for k := range jobs {
		jobNames = append(jobNames, k)
	}
	sort.Strings(jobNames)

	for _, j := range jobNames {
		fmt.Fprint(&b, row(jobAnchorLink(j), strconv.Itoa(jobs[j])))
	}

	b.WriteString(`</table>
</body>
</html>`)

	_, err := c.Writer.Write(b.Bytes())
	if err != nil {
		s.logger.Error(err, "failed to write response")
		c.Status(http.StatusInternalServerError)
	}

	c.Status(http.StatusOK)
}

func jobAnchorLink(jobId string) string {
	return fmt.Sprintf("<a href=\"/job?job_id=%s\">%s</a>", jobId, jobId)
}
func (s *Server) JobHTMLHandler(c *gin.Context) {
	c.Writer.Header().Set("X-Content-Type-Options", "nosniff")
	c.Writer.Header().Set("Content-Type", "text/html; charset=utf-8")

	jobIdValues := c.Request.URL.Query()["job_id"]
	if len(jobIdValues) != 1 {
		c.Status(http.StatusBadRequest)
		return
	}
	jobId := jobIdValues[0]

	var b bytes.Buffer
	t, err := template.New("job").Parse(`<html>
<body>
<h1>Job: {{.}}</h1>
<table>
`)
	if err != nil {
		s.logger.Error(err, "failed to parse template")
		return
	}
	err = t.Execute(&b, jobId)
	if err != nil {
		s.logger.Error(err, "failed to execute template")
		return
	}
	fmt.Fprint(&b, header("Collector", "Target Count"))

	// Filter targets by job
	targets := map[string]*target.Item{}
	for k, v := range s.allocator.TargetItems() {
		if v.JobName == jobId {
			targets[k] = v
		}
	}

	colNames := []string{}
	for _, col := range s.allocator.Collectors() {
		colNames = append(colNames, col.Name)
	}
	sort.Strings(colNames)

	for _, colName := range colNames {
		count := 0
		for _, target := range targets {
			if target.CollectorName == colName {
				count++
			}
		}
		fmt.Fprint(&b, row(collectorAnchorLink(colName), strconv.Itoa(count)))
	}
	b.WriteString(`</table>
<table>
`)
	fmt.Fprint(&b, header("Collector", "Target"))
	for _, v := range colNames {
		for _, t := range targets {
			if t.CollectorName == v {
				fmt.Fprint(&b, row(collectorAnchorLink(v), targetAnchorLink(t)))
			}
		}
	}
	b.WriteString(`</table>
</body>
</html>`)
	_, err = c.Writer.Write(b.Bytes())
	if err != nil {
		s.logger.Error(err, "failed to write response")
		c.Status(http.StatusInternalServerError)
	}

	c.Status(http.StatusOK)
}

func collectorAnchorLink(collectorId string) string {
	return fmt.Sprintf("<a href=\"/collector?collector_id=%s\">%s</a>", collectorId, collectorId)
}

func (s *Server) CollectorHTMLHandler(c *gin.Context) {
	c.Writer.Header().Set("X-Content-Type-Options", "nosniff")
	c.Writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	collectorIdValues := c.Request.URL.Query()["collector_id"]
	collectorId := ""
	if len(collectorIdValues) == 1 {
		collectorId = collectorIdValues[0]
	}

	if collectorId == "" {
		c.Status(http.StatusBadRequest)
		_, err := c.Writer.WriteString(`<html>
<body>
<h1>Bad Request</h1>
<p>Expected collector_id in the query string</p>
<p>Example: /collector?collector_id=my-collector-42</p>
</body>
</html>`)
		if err != nil {
			s.logger.Error(err, "failed to write response")
		}
		return
	}

	found := false
	for _, v := range s.allocator.Collectors() {
		if v.Name == collectorId {
			found = true
			break
		}
	}
	if !found {
		c.Status(http.StatusNotFound)
		t, err := template.New("unknown_collector").Parse(`<html>
<body>
<h1>Unknown Collector: {{.}}</h1>
</body>
</html>`)
		if err != nil {
			s.logger.Error(err, "failed to parse template")
		}
		err = t.Execute(c.Writer, collectorId)
		if err != nil {
			s.logger.Error(err, "failed to write response")
		}
		return
	}

	var b bytes.Buffer
	t, err := template.New("collector").Parse(`<html>
<body>
<h1>Collector: {{.}}</h1>
<table>
`)
	if err != nil {
		s.logger.Error(err, "failed to parse template")
		return
	}
	err = t.Execute(&b, collectorId)
	if err != nil {
		s.logger.Error(err, "failed to execute template")
		return
	}

	fmt.Fprint(&b, header("Job", "Target", "Endpoint Slice"))
	for _, v := range s.sortedTargetItems() {
		if v.CollectorName == collectorId {
			fmt.Fprint(&b, row(jobAnchorLink(v.JobName), targetAnchorLink(v), v.GetEndpointSliceName()))
		}
	}
	b.WriteString(`</table>
</body>
</html>`)
	_, err = c.Writer.Write(b.Bytes())
	if err != nil {
		s.logger.Error(err, "failed to write response")
		c.Status(http.StatusInternalServerError)
	}

	c.Status(http.StatusOK)
}

func (s *Server) TargetsHandler(c *gin.Context) {
	q := c.Request.URL.Query()["collector_id"]

	jobIdParam := c.Params.ByName("job_id")
	jobId, err := url.QueryUnescape(jobIdParam)
	if err != nil {
		s.errorHandler(c.Writer, err)
		return
	}

	if len(q) == 0 {
		displayData := GetAllTargetsByJob(s.allocator, jobId)
		s.jsonHandler(c.Writer, displayData)
	} else {
		targets := GetAllTargetsByCollectorAndJob(s.allocator, q[0], jobId)
		// Displays empty list if nothing matches
		if len(targets) == 0 {
			s.jsonHandler(c.Writer, []interface{}{})
			return
		}
		s.jsonHandler(c.Writer, targets)
	}
}

func (s *Server) errorHandler(w http.ResponseWriter, err error) {
	w.WriteHeader(http.StatusInternalServerError)
	s.jsonHandler(w, err)
}

func (s *Server) jsonHandler(w http.ResponseWriter, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	err := s.jsonMarshaller.NewEncoder(w).Encode(data)
	if err != nil {
		s.logger.Error(err, "failed to encode data for http response")
	}
}

// sortedTargetItems returns a sorted list of target items by its hash.
func (s *Server) sortedTargetItems() []*target.Item {
	targetItems := make([]*target.Item, 0, len(s.allocator.TargetItems()))
	for _, v := range s.allocator.TargetItems() {
		targetItems = append(targetItems, v)
	}
	sort.Slice(targetItems, func(i, j int) bool {
		return targetItems[i].Hash() < targetItems[j].Hash()
	})
	return targetItems
}

func (s *Server) getJobCount() int {
	jobs := make(map[string]struct{})
	for _, v := range s.allocator.TargetItems() {
		jobs[v.JobName] = struct{}{}
	}
	return len(jobs)
}

func (s *Server) getJobCountForCollector(collector string) int {
	jobs := make(map[string]struct{})
	for _, v := range s.allocator.TargetItems() {
		if v.CollectorName == collector {
			jobs[v.JobName] = struct{}{}
		}
	}
	return len(jobs)
}

func (s *Server) getTargetCountForCollector(collector string) int {
	count := 0
	for _, v := range s.allocator.TargetItems() {
		if v.CollectorName == collector {
			count++
		}
	}
	return count
}

// GetAllTargetsByJob is a relatively expensive call that is usually only used for debugging purposes.
func GetAllTargetsByJob(allocator allocation.Allocator, job string) map[string]collectorJSON {
	displayData := make(map[string]collectorJSON)
	for _, col := range allocator.Collectors() {
		targets := GetAllTargetsByCollectorAndJob(allocator, col.Name, job)
		displayData[col.Name] = collectorJSON{
			Link: fmt.Sprintf("/jobs/%s/targets?collector_id=%s", url.QueryEscape(job), col.Name),
			Jobs: targets,
		}
	}
	return displayData
}

// GetAllTargetsByCollector returns all the targets for a given collector and job.
func GetAllTargetsByCollectorAndJob(allocator allocation.Allocator, collectorName string, jobName string) []*targetJSON {
	items := allocator.GetTargetsForCollectorAndJob(collectorName, jobName)
	targets := make([]*targetJSON, len(items))
	for i, item := range items {
		targets[i] = targetJsonFromTargetItem(item)
	}
	return targets
}

// registerPprof registers the pprof handlers and either serves the requested
// specific profile or falls back to index handler.
func registerPprof(g *gin.RouterGroup) {
	g.GET("/*profile", func(c *gin.Context) {
		path := c.Param("profile")
		switch strings.TrimPrefix(path, "/") {
		case "cmdline":
			gin.WrapF(pprof.Cmdline)(c)
		case "profile":
			gin.WrapF(pprof.Profile)(c)
		case "symbol":
			gin.WrapF(pprof.Symbol)(c)
		case "trace":
			gin.WrapF(pprof.Trace)(c)
		default:
			gin.WrapF(pprof.Index)(c)
		}
	})
}

func targetJsonFromTargetItem(item *target.Item) *targetJSON {
	return &targetJSON{
		TargetURL: []string{item.TargetURL},
		Labels:    item.Labels,
	}
}
