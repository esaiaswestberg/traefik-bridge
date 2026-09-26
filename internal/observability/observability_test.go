package observability

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

func TestHTTPHandlerOutput(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics, err := NewMetrics(registry)
	if err != nil {
		t.Fatal(err)
	}
	metrics.ProxyRequest("slave", http.StatusOK)
	handler := NewHTTPHandler(HTTPConfig{
		Registry: registry,
		Ready:    func() bool { return false },
		Status: func() Status {
			return Status{Routes: []RouteStatus{{ID: "route-b", Service: "api"}, {ID: "route-a", Service: "web"}}, Targets: map[string][]TargetStatus{"api": {{Target: "http://127.0.0.1:8080", Healthy: true}}}}
		},
	})

	for _, test := range []struct {
		path string
		code int
		body string
	}{
		{path: "/healthz", code: http.StatusOK, body: `{"status":"ok"}`},
		{path: "/readyz", code: http.StatusServiceUnavailable, body: `{"status":"not_ready"}`},
		{path: "/status", code: http.StatusOK, body: `{"id":"route-a"`},
		{path: "/metrics", code: http.StatusOK, body: `traefik_bridge_proxy_requests_total`},
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, test.path, nil))
		if response.Code != test.code {
			t.Errorf("%s status = %d, want %d", test.path, response.Code, test.code)
		}
		if !strings.Contains(response.Body.String(), test.body) {
			t.Errorf("%s body = %q, want %q", test.path, response.Body.String(), test.body)
		}
	}
}

func TestJSONLoggerOutput(t *testing.T) {
	var output bytes.Buffer
	NewJSONLogger(&output, slog.LevelInfo).Info("connected", "slave_id", "slave-a")
	if got := output.String(); !strings.Contains(got, `"msg":"connected"`) || !strings.Contains(got, `"slave_id":"slave-a"`) {
		t.Errorf("log output = %q", got)
	}
}
