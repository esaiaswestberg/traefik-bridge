// bridge-proxy receives Traefik requests and forwards them to a bridge slave.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/traefik/traefik-bridge/internal/config"
	"github.com/traefik/traefik-bridge/internal/observability"
	"github.com/traefik/traefik-bridge/internal/proxy"
)

func main() {
	logger := observability.NewJSONLogger(os.Stderr, slog.LevelInfo)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	if err := run(ctx, logger); err != nil {
		stop()
		logger.Error("bridge-proxy fatal error", "error", err)
		os.Exit(1)
	}
	stop()
}

func run(ctx context.Context, logger *slog.Logger) error {
	cfg, err := config.LoadProxy()
	if err != nil {
		return fmt.Errorf("load configuration: %w", err)
	}
	upstream, err := url.Parse(cfg.Upstream)
	if err != nil || upstream.Scheme != "https" || upstream.Host == "" {
		return errors.New("BRIDGE_UPSTREAM must be an HTTPS URL")
	}
	tlsConfig, err := clientTLSConfig(cfg)
	if err != nil {
		return err
	}
	servers := make([]*http.Server, 0, len(cfg.ListenerPorts))
	listeners := make([]net.Listener, 0, len(cfg.ListenerPorts))
	for service, port := range cfg.ListenerPorts {
		handler, err := proxy.NewMaster(proxy.MasterConfig{Upstream: upstream, RouteToken: cfg.RouteTokens[service], TLSConfig: tlsConfig})
		if err != nil {
			return fmt.Errorf("initialize %q listener: %w", service, err)
		}
		listener, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
		if err != nil {
			for _, open := range listeners {
				_ = open.Close()
			}
			return fmt.Errorf("listen for %q: %w", service, err)
		}
		servers = append(servers, &http.Server{Handler: handler})
		listeners = append(listeners, listener)
	}
	errs := make(chan error, len(servers))
	for i, server := range servers {
		go func(server *http.Server, listener net.Listener) {
			if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errs <- err
			}
		}(server, listeners[i])
	}
	logger.Info("bridge-proxy started", "upstream", upstream.Redacted(), "listeners", len(servers))
	select {
	case <-ctx.Done():
	case err := <-errs:
		return fmt.Errorf("serve proxy: %w", err)
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, server := range servers {
		if err := server.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shutdown proxy: %w", err)
		}
	}
	return nil
}

func clientTLSConfig(cfg config.Proxy) (*tls.Config, error) {
	certificate, err := tls.LoadX509KeyPair(cfg.CertificateFile, cfg.PrivateKeyFile)
	if err != nil {
		return nil, fmt.Errorf("load client certificate: %w", err)
	}
	ca, err := os.ReadFile(cfg.CAFile)
	if err != nil {
		return nil, fmt.Errorf("read bridge CA: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		return nil, errors.New("invalid bridge CA certificate")
	}
	return &tls.Config{Certificates: []tls.Certificate{certificate}, RootCAs: roots, ServerName: cfg.ServerName, MinVersion: tls.VersionTLS13}, nil
}
