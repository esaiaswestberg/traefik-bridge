// Package observability provides metrics, JSON logging, and operational HTTP endpoints.
package observability

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strconv"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Recorder receives optional observations from bridge primitives.
type Recorder interface {
	Connection(bool)
	Snapshot(bool)
	ProxyRequest(string, int)
	TargetHealth(string, string, bool)
}

// Metrics is the Prometheus implementation of Recorder.
type Metrics struct {
	connections  prometheus.Gauge
	snapshots    *prometheus.CounterVec
	requests     *prometheus.CounterVec
	targetHealth *prometheus.GaugeVec
}

// NewMetrics registers bridge metrics on registerer. Pass a dedicated registry
// in tests or an existing application registry in production.
func NewMetrics(registerer prometheus.Registerer) (*Metrics, error) {
	metrics := &Metrics{
		connections:  prometheus.NewGauge(prometheus.GaugeOpts{Name: "traefik_bridge_control_connections", Help: "Current connected bridge control streams."}),
		snapshots:    prometheus.NewCounterVec(prometheus.CounterOpts{Name: "traefik_bridge_snapshots_total", Help: "Bridge snapshots processed."}, []string{"result"}),
		requests:     prometheus.NewCounterVec(prometheus.CounterOpts{Name: "traefik_bridge_proxy_requests_total", Help: "Bridge proxy requests."}, []string{"component", "code"}),
		targetHealth: prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "traefik_bridge_target_healthy", Help: "Current local bridge target health."}, []string{"service", "target"}),
	}
	if registerer == nil {
		registerer = prometheus.DefaultRegisterer
	}
	for _, collector := range []prometheus.Collector{metrics.connections, metrics.snapshots, metrics.requests, metrics.targetHealth} {
		if err := registerer.Register(collector); err != nil {
			return nil, err
		}
	}
	return metrics, nil
}

// Connection records a control stream opening or closing.
func (m *Metrics) Connection(connected bool) {
	if m == nil {
		return
	}
	if connected {
		m.connections.Inc()
		return
	}
	m.connections.Dec()
}

// Snapshot records an accepted or rejected snapshot.
func (m *Metrics) Snapshot(accepted bool) {
	if m == nil {
		return
	}
	result := "rejected"
	if accepted {
		result = "accepted"
	}
	m.snapshots.WithLabelValues(result).Inc()
}

// ProxyRequest records one master or slave proxy response.
func (m *Metrics) ProxyRequest(component string, code int) {
	if m != nil {
		m.requests.WithLabelValues(component, strconv.Itoa(code)).Inc()
	}
}

// TargetHealth records the current health of a local service target.
func (m *Metrics) TargetHealth(service, target string, healthy bool) {
	if m == nil {
		return
	}
	value := 0.0
	if healthy {
		value = 1
	}
	m.targetHealth.WithLabelValues(service, target).Set(value)
}

// NewJSONLogger returns a structured JSON logger writing to output.
func NewJSONLogger(output io.Writer, level slog.Level) *slog.Logger {
	return slog.New(slog.NewJSONHandler(output, &slog.HandlerOptions{Level: level}))
}

// RouteStatus is a safe route description for operational inspection. It
// intentionally excludes route tokens, credentials, and certificate material.
type RouteStatus struct {
	ID      string `json:"id"`
	SlaveID string `json:"slave_id"`
	Service string `json:"service"`
	Ready   bool   `json:"ready"`
}

// TargetStatus is the safe health state of one local target.
type TargetStatus struct {
	Target  string `json:"target"`
	Healthy bool   `json:"healthy"`
}

// Status is the safe JSON document returned by the inspection endpoint.
type Status struct {
	Routes  []RouteStatus             `json:"routes"`
	Targets map[string][]TargetStatus `json:"targets,omitempty"`
}

// HTTPConfig configures the injectable operational HTTP handler.
type HTTPConfig struct {
	Registry prometheus.Gatherer
	Ready    func() bool
	Status   func() Status
}

// NewHTTPHandler serves liveness, readiness, metrics, and safe status data at
// /healthz, /readyz, /metrics, and /status respectively.
func NewHTTPHandler(config HTTPConfig) http.Handler {
	registry := config.Registry
	if registry == nil {
		registry = prometheus.DefaultGatherer
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(writer http.ResponseWriter, _ *http.Request) {
		writeJSON(writer, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /readyz", func(writer http.ResponseWriter, _ *http.Request) {
		if config.Ready != nil && !config.Ready() {
			writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"status": "not_ready"})
			return
		}
		writeJSON(writer, http.StatusOK, map[string]string{"status": "ready"})
	})
	mux.Handle("GET /metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))
	mux.HandleFunc("GET /status", func(writer http.ResponseWriter, _ *http.Request) {
		status := Status{}
		if config.Status != nil {
			status = config.Status()
		}
		sort.Slice(status.Routes, func(i, j int) bool { return status.Routes[i].ID < status.Routes[j].ID })
		writeJSON(writer, http.StatusOK, status)
	})
	return mux
}

func writeJSON(writer http.ResponseWriter, code int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(code)
	_ = json.NewEncoder(writer).Encode(value)
}
