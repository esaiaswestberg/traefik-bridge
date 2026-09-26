// Package proxy provides the bridge data plane between master proxies and slave services.
package proxy

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
	"time"
)

// RouteHeader carries the signed, opaque route identifier between bridge peers.
const RouteHeader = "X-Traefik-Bridge-Route"

// Route is the signed routing claim understood only by bridge peers.
type Route struct {
	RouteID   string    `json:"r"`
	ServiceID string    `json:"s"`
	ExpiresAt time.Time `json:"e"`
}

// Signer creates and verifies signed opaque route identifiers.
type Signer struct {
	key []byte
	now func() time.Time
}

// NewSigner creates a signer from a high-entropy shared secret.
func NewSigner(key []byte) (*Signer, error) {
	if len(key) == 0 {
		return nil, errors.New("route signing key is required")
	}
	return &Signer{key: append([]byte(nil), key...), now: time.Now}, nil
}

// Sign returns an opaque identifier for route.
func (s *Signer) Sign(route Route) (string, error) {
	if s == nil || len(s.key) == 0 {
		return "", errors.New("route signer is required")
	}
	if route.RouteID == "" || route.ServiceID == "" || route.ExpiresAt.IsZero() {
		return "", errors.New("route ID, service ID, and expiry are required")
	}
	payload, err := json.Marshal(route)
	if err != nil {
		return "", fmt.Errorf("marshal route: %w", err)
	}
	mac := hmac.New(sha256.New, s.key)
	_, _ = mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

// Verify validates token and allows expiry for grace after its expiration.
func (s *Signer) Verify(token string, grace time.Duration) (Route, error) {
	if s == nil || len(s.key) == 0 {
		return Route{}, errors.New("route signer is required")
	}
	if grace < 0 {
		return Route{}, errors.New("route grace must not be negative")
	}
	var encodedPayload, encodedMAC string
	for i := range token {
		if token[i] == '.' {
			encodedPayload, encodedMAC = token[:i], token[i+1:]
			break
		}
	}
	if encodedPayload == "" || encodedMAC == "" {
		return Route{}, errors.New("invalid route token")
	}
	payload, err := base64.RawURLEncoding.DecodeString(encodedPayload)
	if err != nil {
		return Route{}, errors.New("invalid route token")
	}
	providedMAC, err := base64.RawURLEncoding.DecodeString(encodedMAC)
	if err != nil {
		return Route{}, errors.New("invalid route token")
	}
	mac := hmac.New(sha256.New, s.key)
	_, _ = mac.Write(payload)
	if !hmac.Equal(providedMAC, mac.Sum(nil)) {
		return Route{}, errors.New("invalid route signature")
	}
	var route Route
	if err := json.Unmarshal(payload, &route); err != nil || route.RouteID == "" || route.ServiceID == "" || route.ExpiresAt.IsZero() {
		return Route{}, errors.New("invalid route token")
	}
	if s.now().After(route.ExpiresAt.Add(grace)) {
		return Route{}, errors.New("route token has expired")
	}
	return route, nil
}

// HTTP2Transport returns an mTLS-capable transport that attempts HTTP/2.
func HTTP2Transport(config *tls.Config) *http.Transport {
	if config == nil {
		config = &tls.Config{}
	} else {
		config = config.Clone()
	}
	config.NextProtos = []string{"h2", "http/1.1"}
	return &http.Transport{TLSClientConfig: config, ForceAttemptHTTP2: true}
}

// ServerTLSConfig returns a TLS 1.3 HTTP/2 server configuration requiring a client certificate.
func ServerTLSConfig(certificate tls.Certificate, clientCAs *x509.CertPool) *tls.Config {
	return &tls.Config{Certificates: []tls.Certificate{certificate}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: clientCAs, MinVersion: tls.VersionTLS13, NextProtos: []string{"h2"}}
}

// MasterConfig configures one generated master-side proxy listener.
type MasterConfig struct {
	Upstream   *url.URL
	RouteToken string
	Transport  http.RoundTripper
	TLSConfig  *tls.Config
}

// NewMaster returns a handler forwarding requests to one slave over the configured upstream.
func NewMaster(config MasterConfig) (http.Handler, error) {
	if config.Upstream == nil || config.Upstream.Scheme == "" || config.Upstream.Host == "" {
		return nil, errors.New("slave upstream URL is required")
	}
	if config.RouteToken == "" {
		return nil, errors.New("route token is required")
	}
	transport := config.Transport
	if transport == nil {
		transport = HTTP2Transport(config.TLSConfig)
	}
	return &httputil.ReverseProxy{
		Transport: transport,
		Rewrite: func(proxyRequest *httputil.ProxyRequest) {
			proxyRequest.Out.Header.Del(RouteHeader)
			proxyRequest.SetURL(config.Upstream)
			proxyRequest.Out.Host = proxyRequest.In.Host
			proxyRequest.SetXForwarded()
			proxyRequest.Out.Header.Set(RouteHeader, config.RouteToken)
		},
		FlushInterval: -1,
	}, nil
}

// SlaveConfig configures validation and local target pools for a slave listener.
type SlaveConfig struct {
	Signer       *Signer
	Grace        time.Duration
	Services     map[string][]*url.URL
	HealthChecks map[string]HealthCheck
	Transport    http.RoundTripper
}

// HealthCheck describes a local target health endpoint. Path and Scheme are
// applied to the outbound request without changing normal request routing.
type HealthCheck struct {
	Path     string
	Scheme   string
	Interval time.Duration
	Timeout  time.Duration
}

// TargetHealth is the current health state of one local service target.
type TargetHealth struct {
	Target  string
	Healthy bool
}

// Slave validates bridge routes then forwards to round-robin local service targets.
type Slave struct {
	signer    *Signer
	grace     time.Duration
	pools     map[string]*targetPool
	transport http.RoundTripper
	cancel    context.CancelFunc
}

type targetPool struct {
	targets []target
	next    uint64
	mu      sync.Mutex
}

type target struct {
	url     *url.URL
	healthy bool
}

// NewSlave creates a slave data listener handler.
func NewSlave(config SlaveConfig) (*Slave, error) {
	if config.Signer == nil {
		return nil, errors.New("route signer is required")
	}
	if config.Grace < 0 {
		return nil, errors.New("route grace must not be negative")
	}
	pools := make(map[string]*targetPool, len(config.Services))
	for service, targets := range config.Services {
		if service == "" || len(targets) == 0 {
			return nil, errors.New("every service requires a target")
		}
		pool := &targetPool{targets: make([]target, len(targets))}
		for i, targetURL := range targets {
			if targetURL == nil || targetURL.Scheme == "" || targetURL.Host == "" {
				return nil, fmt.Errorf("service %q has an invalid target", service)
			}
			copy := *targetURL
			pool.targets[i] = target{url: &copy, healthy: true}
		}
		pools[service] = pool
	}
	checks := make(map[string]HealthCheck, len(config.HealthChecks))
	for service, check := range config.HealthChecks {
		if pools[service] == nil {
			return nil, fmt.Errorf("health check references unknown service %q", service)
		}
		if check.Path == "" || check.Path[0] != '/' {
			return nil, fmt.Errorf("health check for service %q requires an absolute path", service)
		}
		if check.Scheme != "" && check.Scheme != "http" && check.Scheme != "https" {
			return nil, fmt.Errorf("health check for service %q has an invalid scheme", service)
		}
		if check.Interval <= 0 {
			check.Interval = 30 * time.Second
		}
		if check.Timeout <= 0 {
			check.Timeout = 5 * time.Second
		}
		checks[service] = check
	}
	ctx, cancel := context.WithCancel(context.Background())
	slave := &Slave{signer: config.Signer, grace: config.Grace, pools: pools, transport: config.Transport, cancel: cancel}
	for service, check := range checks {
		go slave.runHealthChecks(ctx, service, check)
	}
	return slave, nil
}

// ServeHTTP validates the route token before proxying to the selected local target.
func (s *Slave) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	route, err := s.signer.Verify(request.Header.Get(RouteHeader), s.grace)
	if err != nil {
		http.Error(response, "invalid bridge route", http.StatusForbidden)
		return
	}
	pool := s.pools[route.ServiceID]
	if pool == nil {
		http.Error(response, "unknown bridge service", http.StatusNotFound)
		return
	}
	target, ok := pool.target(request.Context())
	if !ok {
		http.Error(response, "bridge service has no healthy targets", http.StatusServiceUnavailable)
		return
	}
	proxy := &httputil.ReverseProxy{
		Transport: s.transport,
		Rewrite: func(proxyRequest *httputil.ProxyRequest) {
			proxyRequest.Out.Header.Del(RouteHeader)
			proxyRequest.SetURL(target)
			proxyRequest.Out.Host = proxyRequest.In.Host
			proxyRequest.SetXForwarded()
		},
		FlushInterval: -1,
	}
	proxy.ServeHTTP(response, request)
}

type healthTargetContextKey struct{}

type healthTarget struct {
	index int
	url   *url.URL
}

func (p *targetPool) target(ctx context.Context) (*url.URL, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if healthTarget, ok := ctx.Value(healthTargetContextKey{}).(healthTarget); ok {
		if healthTarget.index < 0 || healthTarget.index >= len(p.targets) {
			return nil, false
		}
		return healthTarget.url, true
	}
	for offset := range p.targets {
		index := (p.next + uint64(offset)) % uint64(len(p.targets))
		if p.targets[index].healthy {
			p.next = index + 1
			return p.targets[index].url, true
		}
	}
	return nil, false
}

// Close stops the in-process health checkers.
func (s *Slave) Close() {
	if s != nil && s.cancel != nil {
		s.cancel()
	}
}

// Status returns a consistent, testable snapshot of local target health.
func (s *Slave) Status() map[string][]TargetHealth {
	status := make(map[string][]TargetHealth, len(s.pools))
	for service, pool := range s.pools {
		pool.mu.Lock()
		targets := make([]TargetHealth, len(pool.targets))
		for i, target := range pool.targets {
			targets[i] = TargetHealth{Target: target.url.String(), Healthy: target.healthy}
		}
		pool.mu.Unlock()
		status[service] = targets
	}
	return status
}

func (s *Slave) runHealthChecks(ctx context.Context, service string, check HealthCheck) {
	s.checkService(ctx, service, check)
	ticker := time.NewTicker(check.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.checkService(ctx, service, check)
		}
	}
}

func (s *Slave) checkService(ctx context.Context, service string, check HealthCheck) {
	pool := s.pools[service]
	if pool == nil {
		return
	}
	pool.mu.Lock()
	count := len(pool.targets)
	pool.mu.Unlock()
	for index := range count {
		s.checkTarget(ctx, service, index, check)
	}
}

func (s *Slave) checkTarget(ctx context.Context, service string, index int, check HealthCheck) {
	pool := s.pools[service]
	if pool == nil {
		return
	}
	pool.mu.Lock()
	if index < 0 || index >= len(pool.targets) {
		pool.mu.Unlock()
		return
	}
	target := *pool.targets[index].url
	pool.mu.Unlock()
	if check.Scheme != "" {
		target.Scheme = check.Scheme
	}
	ctx, cancel := context.WithTimeout(ctx, check.Timeout)
	defer cancel()
	route, err := s.signer.Sign(Route{RouteID: "health-check", ServiceID: service, ExpiresAt: s.signer.now().Add(check.Timeout)})
	if err != nil {
		return
	}
	request, err := http.NewRequestWithContext(context.WithValue(ctx, healthTargetContextKey{}, healthTarget{index: index, url: &target}), http.MethodGet, "http://bridge"+check.Path, nil)
	if err != nil {
		return
	}
	request.Header.Set(RouteHeader, route)
	response := &healthResponseWriter{header: make(http.Header)}
	s.ServeHTTP(response, request)
	pool.mu.Lock()
	pool.targets[index].healthy = response.status >= http.StatusOK && response.status < http.StatusMultipleChoices
	pool.mu.Unlock()
}

type healthResponseWriter struct {
	header http.Header
	status int
}

func (w *healthResponseWriter) Header() http.Header { return w.header }
func (w *healthResponseWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}
func (w *healthResponseWriter) Write(value []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return len(value), nil
}
