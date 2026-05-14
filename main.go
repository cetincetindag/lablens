package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	defaultPort          = "8080"
	defaultPrometheusURL = "http://kube-prometheus-stack-prometheus.monitoring.svc.cluster.local:9090"
	defaultRequestTTL    = 8 * time.Second
)

//go:embed web
var webFS embed.FS

type PrometheusClient struct {
	baseURL string
	client  *http.Client
}

type promResponse struct {
	Status string `json:"status"`
	Error  string `json:"error"`
	Data   struct {
		ResultType string          `json:"resultType"`
		Result     json.RawMessage `json:"result"`
	} `json:"data"`
}

type promVectorResult struct {
	Metric map[string]string `json:"metric"`
	Value  []any             `json:"value"`
}

type promMatrixResult struct {
	Metric map[string]string `json:"metric"`
	Values [][]any           `json:"values"`
}

type NodeStat struct {
	Name           string  `json:"name"`
	CPUPercent     float64 `json:"cpuPercent"`
	MemoryPercent  float64 `json:"memoryPercent"`
	StoragePercent float64 `json:"storagePercent"`
}

type TimeseriesPoint struct {
	Timestamp int64   `json:"timestamp"`
	Value     float64 `json:"value"`
}

type OverviewResponse struct {
	UpdatedAt string `json:"updatedAt"`
	Summary   struct {
		Nodes          float64 `json:"nodes"`
		CPUPercent     float64 `json:"cpuPercent"`
		MemoryPercent  float64 `json:"memoryPercent"`
		StoragePercent float64 `json:"storagePercent"`
	} `json:"summary"`
	Workloads struct {
		PodsRunning float64 `json:"podsRunning"`
		Namespaces  float64 `json:"namespaces"`
	} `json:"workloads"`
	NodeStats []NodeStat `json:"nodeStats"`
	Resources struct {
		NodeNames []string `json:"nodeNames"`
		AppNames  []string `json:"appNames"`
		PodNames  []string `json:"podNames"`
	} `json:"resources"`
	Trends struct {
		CPUUsage    []TimeseriesPoint `json:"cpuUsage"`
		MemoryUsage []TimeseriesPoint `json:"memoryUsage"`
	} `json:"trends"`
}

type Server struct {
	prometheus *PrometheusClient
}

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	prometheusURL := os.Getenv("PROMETHEUS_URL")
	if strings.TrimSpace(prometheusURL) == "" {
		prometheusURL = defaultPrometheusURL
	}

	server := &Server{
		prometheus: &PrometheusClient{
			baseURL: strings.TrimRight(prometheusURL, "/"),
			client:  &http.Client{Timeout: defaultRequestTTL},
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/overview", server.handleOverview)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	webRoot, err := fs.Sub(webFS, "web")
	if err != nil {
		logger.Error("failed to load embedded web assets", "error", err)
		os.Exit(1)
	}
	mux.Handle("/", http.FileServer(http.FS(webRoot)))

	httpServer := &http.Server{
		Addr:              ":" + envOrDefault("PORT", defaultPort),
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	logger.Info("starting lablens", "address", httpServer.Addr, "prometheusURL", prometheusURL)
	if err := httpServer.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		logger.Error("server failed", "error", err)
		os.Exit(1)
	}
}

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	payload, err := s.buildOverview(ctx)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to fetch metrics: %v", err), http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		http.Error(w, fmt.Sprintf("failed to encode response: %v", err), http.StatusInternalServerError)
	}
}

func (s *Server) buildOverview(ctx context.Context) (*OverviewResponse, error) {
	response := &OverviewResponse{
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
	}

	var err error
	response.Summary.Nodes, err = s.prometheus.queryScalar(ctx, `count(kube_node_info)`)
	if err != nil {
		return nil, fmt.Errorf("nodes query: %w", err)
	}

	response.Summary.CPUPercent, err = s.prometheus.queryScalar(ctx, `100 * (1 - avg(rate(node_cpu_seconds_total{mode="idle"}[5m])))`)
	if err != nil {
		return nil, fmt.Errorf("cluster cpu query: %w", err)
	}

	response.Summary.MemoryPercent, err = s.prometheus.queryScalar(ctx, `100 * (1 - (sum(node_memory_MemAvailable_bytes) / sum(node_memory_MemTotal_bytes)))`)
	if err != nil {
		return nil, fmt.Errorf("cluster memory query: %w", err)
	}

	response.Summary.StoragePercent, err = s.prometheus.queryScalar(ctx, `100 * (1 - (sum(node_filesystem_avail_bytes{fstype!~"tmpfs|overlay"}) / sum(node_filesystem_size_bytes{fstype!~"tmpfs|overlay"})))`)
	if err != nil {
		return nil, fmt.Errorf("cluster storage query: %w", err)
	}

	response.Workloads.PodsRunning, err = s.prometheus.queryScalar(ctx, `sum(kube_pod_status_phase{phase="Running"})`)
	if err != nil {
		return nil, fmt.Errorf("running pods query: %w", err)
	}

	response.Workloads.Namespaces, err = s.prometheus.queryScalar(ctx, `count(kube_namespace_created)`)
	if err != nil {
		return nil, fmt.Errorf("namespace count query: %w", err)
	}

	response.NodeStats, err = s.buildNodeStats(ctx)
	if err != nil {
		return nil, fmt.Errorf("node stats query: %w", err)
	}
	response.Resources.NodeNames, response.Resources.AppNames, response.Resources.PodNames, err = s.buildResourceNames(ctx)
	if err != nil {
		return nil, fmt.Errorf("resource names query: %w", err)
	}

	windowEnd := time.Now().UTC()
	windowStart := windowEnd.Add(-1 * time.Hour)
	response.Trends.CPUUsage, err = s.prometheus.queryRange(ctx, `100 * (1 - avg(rate(node_cpu_seconds_total{mode="idle"}[5m])))`, windowStart, windowEnd, time.Minute)
	if err != nil {
		return nil, fmt.Errorf("cpu trend query: %w", err)
	}

	response.Trends.MemoryUsage, err = s.prometheus.queryRange(ctx, `100 * (1 - (sum(node_memory_MemAvailable_bytes) / sum(node_memory_MemTotal_bytes)))`, windowStart, windowEnd, time.Minute)
	if err != nil {
		return nil, fmt.Errorf("memory trend query: %w", err)
	}

	return response, nil
}

func (s *Server) buildResourceNames(ctx context.Context) ([]string, []string, []string, error) {
	nodeSamples, err := s.prometheus.queryVector(ctx, `kube_node_info`)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("node list query: %w", err)
	}

	appSamples, err := s.prometheus.queryVector(ctx, `kube_deployment_status_replicas_available > 0`)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("app list query: %w", err)
	}

	podSamples, err := s.prometheus.queryVector(ctx, `kube_pod_status_phase{phase="Running"} == 1`)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("pod list query: %w", err)
	}

	nodeSet := map[string]struct{}{}
	for _, sample := range nodeSamples {
		name := strings.TrimSpace(sample.Metric["node"])
		if name == "" {
			name = strings.TrimSpace(sample.Metric["instance"])
		}
		if name == "" {
			continue
		}
		nodeSet[formatNodeName(name)] = struct{}{}
	}

	appSet := map[string]struct{}{}
	for _, sample := range appSamples {
		deployment := strings.TrimSpace(sample.Metric["deployment"])
		if deployment == "" {
			continue
		}
		appSet[namespacedName(sample.Metric["namespace"], deployment)] = struct{}{}
	}

	podSet := map[string]struct{}{}
	for _, sample := range podSamples {
		pod := strings.TrimSpace(sample.Metric["pod"])
		if pod == "" {
			continue
		}
		podSet[namespacedName(sample.Metric["namespace"], pod)] = struct{}{}
	}

	return sortedSet(nodeSet), sortedSet(appSet), sortedSet(podSet), nil
}

func (s *Server) buildNodeStats(ctx context.Context) ([]NodeStat, error) {
	cpuByInstance, err := s.prometheus.queryLabeled(ctx, `100 * (1 - avg by (instance) (rate(node_cpu_seconds_total{mode="idle"}[5m])))`, "instance")
	if err != nil {
		return nil, fmt.Errorf("node cpu query: %w", err)
	}

	memoryByInstance, err := s.prometheus.queryLabeled(ctx, `100 * (1 - (node_memory_MemAvailable_bytes / node_memory_MemTotal_bytes))`, "instance")
	if err != nil {
		return nil, fmt.Errorf("node memory query: %w", err)
	}

	storageByInstance, err := s.prometheus.queryLabeled(ctx, `100 * (1 - (sum by(instance) (node_filesystem_avail_bytes{fstype!~"tmpfs|overlay"}) / sum by(instance) (node_filesystem_size_bytes{fstype!~"tmpfs|overlay"})))`, "instance")
	if err != nil {
		return nil, fmt.Errorf("node storage query: %w", err)
	}

	names := map[string]struct{}{}
	for name := range cpuByInstance {
		names[name] = struct{}{}
	}
	for name := range memoryByInstance {
		names[name] = struct{}{}
	}
	for name := range storageByInstance {
		names[name] = struct{}{}
	}

	var stats []NodeStat
	for instance := range names {
		stats = append(stats, NodeStat{
			Name:           formatNodeName(instance),
			CPUPercent:     cpuByInstance[instance],
			MemoryPercent:  memoryByInstance[instance],
			StoragePercent: storageByInstance[instance],
		})
	}

	sort.Slice(stats, func(i, j int) bool {
		return stats[i].Name < stats[j].Name
	})

	return stats, nil
}

func (c *PrometheusClient) queryScalar(ctx context.Context, expression string) (float64, error) {
	values, err := c.queryVector(ctx, expression)
	if err != nil {
		return 0, err
	}
	if len(values) == 0 {
		return 0, fmt.Errorf("empty result for query: %s", expression)
	}
	return values[0].Value, nil
}

func (c *PrometheusClient) queryLabeled(ctx context.Context, expression string, label string) (map[string]float64, error) {
	values, err := c.queryVector(ctx, expression)
	if err != nil {
		return nil, err
	}

	result := make(map[string]float64, len(values))
	for _, sample := range values {
		key := sample.Metric[label]
		if strings.TrimSpace(key) == "" {
			continue
		}
		result[key] = sample.Value
	}
	return result, nil
}

type promSample struct {
	Metric map[string]string
	Value  float64
}

func (c *PrometheusClient) queryVector(ctx context.Context, expression string) ([]promSample, error) {
	requestURL, err := url.Parse(c.baseURL + "/api/v1/query")
	if err != nil {
		return nil, fmt.Errorf("invalid prometheus URL: %w", err)
	}

	query := requestURL.Query()
	query.Set("query", expression)
	requestURL.RawQuery = query.Encode()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("build query request: %w", err)
	}

	response, err := c.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("execute query request: %w", err)
	}
	defer response.Body.Close()

	body, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, fmt.Errorf("read query response: %w", err)
	}

	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("query failed with status %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
	}

	var payload promResponse
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("decode query response: %w", err)
	}
	if payload.Status != "success" {
		return nil, fmt.Errorf("prometheus query failed: %s", payload.Error)
	}

	var vectorValues []promVectorResult
	if err := json.Unmarshal(payload.Data.Result, &vectorValues); err != nil {
		return nil, fmt.Errorf("decode vector result: %w", err)
	}

	result := make([]promSample, 0, len(vectorValues))
	for _, value := range vectorValues {
		parsedValue, err := parsePromValue(value.Value)
		if err != nil {
			return nil, err
		}

		result = append(result, promSample{
			Metric: value.Metric,
			Value:  parsedValue,
		})
	}
	return result, nil
}

func (c *PrometheusClient) queryRange(ctx context.Context, expression string, start time.Time, end time.Time, step time.Duration) ([]TimeseriesPoint, error) {
	requestURL, err := url.Parse(c.baseURL + "/api/v1/query_range")
	if err != nil {
		return nil, fmt.Errorf("invalid prometheus URL: %w", err)
	}

	query := requestURL.Query()
	query.Set("query", expression)
	query.Set("start", strconv.FormatFloat(float64(start.Unix()), 'f', -1, 64))
	query.Set("end", strconv.FormatFloat(float64(end.Unix()), 'f', -1, 64))
	query.Set("step", strconv.Itoa(int(step.Seconds())))
	requestURL.RawQuery = query.Encode()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("build query_range request: %w", err)
	}

	response, err := c.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("execute query_range request: %w", err)
	}
	defer response.Body.Close()

	body, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, fmt.Errorf("read query_range response: %w", err)
	}

	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("query_range failed with status %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
	}

	var payload promResponse
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("decode query_range response: %w", err)
	}
	if payload.Status != "success" {
		return nil, fmt.Errorf("prometheus query_range failed: %s", payload.Error)
	}

	var matrixValues []promMatrixResult
	if err := json.Unmarshal(payload.Data.Result, &matrixValues); err != nil {
		return nil, fmt.Errorf("decode matrix result: %w", err)
	}
	if len(matrixValues) == 0 {
		return nil, fmt.Errorf("empty range result for query: %s", expression)
	}

	points := make([]TimeseriesPoint, 0, len(matrixValues[0].Values))
	for _, rawValue := range matrixValues[0].Values {
		if len(rawValue) != 2 {
			return nil, fmt.Errorf("unexpected matrix point format")
		}

		ts, err := parsePromNumber(rawValue[0])
		if err != nil {
			return nil, fmt.Errorf("invalid matrix timestamp: %w", err)
		}
		value, err := parsePromNumber(rawValue[1])
		if err != nil {
			return nil, fmt.Errorf("invalid matrix value: %w", err)
		}

		points = append(points, TimeseriesPoint{
			Timestamp: int64(ts),
			Value:     value,
		})
	}

	return points, nil
}

func parsePromValue(raw []any) (float64, error) {
	if len(raw) != 2 {
		return 0, fmt.Errorf("unexpected vector point format")
	}
	return parsePromNumber(raw[1])
}

func parsePromNumber(raw any) (float64, error) {
	switch value := raw.(type) {
	case string:
		parsed, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return 0, fmt.Errorf("parse float from string: %w", err)
		}
		return parsed, nil
	case float64:
		return value, nil
	case json.Number:
		parsed, err := value.Float64()
		if err != nil {
			return 0, fmt.Errorf("parse json number: %w", err)
		}
		return parsed, nil
	default:
		return 0, fmt.Errorf("unsupported numeric type %T", raw)
	}
}

func envOrDefault(name string, fallback string) string {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	return value
}

func formatNodeName(instance string) string {
	host, _, err := net.SplitHostPort(instance)
	if err == nil && host != "" {
		return host
	}
	return instance
}

func namespacedName(namespace string, name string) string {
	namespace = strings.TrimSpace(namespace)
	name = strings.TrimSpace(name)
	if namespace == "" {
		return name
	}
	return namespace + "/" + name
}

func sortedSet(values map[string]struct{}) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}
