// Package proxy provides the bridge data plane between master proxies and slave services.
package proxy

import (
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
	Signer    *Signer
	Grace     time.Duration
	Services  map[string][]*url.URL
	Transport http.RoundTripper
}

// Slave validates bridge routes then forwards to round-robin local service targets.
type Slave struct {
	signer    *Signer
	grace     time.Duration
	pools     map[string]*targetPool
	transport http.RoundTripper
}

type targetPool struct {
	targets []*url.URL
	next    uint64
	mu      sync.Mutex
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
		pool := &targetPool{targets: make([]*url.URL, len(targets))}
		for i, target := range targets {
			if target == nil || target.Scheme == "" || target.Host == "" {
				return nil, fmt.Errorf("service %q has an invalid target", service)
			}
			copy := *target
			pool.targets[i] = &copy
		}
		pools[service] = pool
	}
	return &Slave{signer: config.Signer, grace: config.Grace, pools: pools, transport: config.Transport}, nil
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
	target := pool.target()
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

func (p *targetPool) target() *url.URL {
	p.mu.Lock()
	target := p.targets[p.next%uint64(len(p.targets))]
	p.next++
	p.mu.Unlock()
	return target
}
