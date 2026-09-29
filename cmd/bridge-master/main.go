// bridge-master manages remote bridge slaves and Traefik-facing proxies.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/traefik/traefik-bridge/internal/config"
	bridgecontrol "github.com/traefik/traefik-bridge/internal/control"
	bridgecrypto "github.com/traefik/traefik-bridge/internal/crypto"
	bridgedocker "github.com/traefik/traefik-bridge/internal/docker"
	"github.com/traefik/traefik-bridge/internal/observability"
	"github.com/traefik/traefik-bridge/internal/proxy"
	"github.com/traefik/traefik-bridge/internal/state"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

func main() {
	logger := observability.NewJSONLogger(os.Stderr, slog.LevelInfo)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	if err := run(ctx, logger); err != nil {
		stop()
		logger.Error("bridge-master fatal error", "error", err)
		os.Exit(1)
	}
	stop()
}

func run(ctx context.Context, logger *slog.Logger) error {
	cfg, err := config.LoadMaster()
	if err != nil {
		return fmt.Errorf("load configuration: %w", err)
	}
	authority, err := bridgecrypto.Initialize(state.NewStore(cfg.DataDir))
	if err != nil {
		return err
	}
	if err := authority.ExportPEM(cfg.DataDir); err != nil {
		return fmt.Errorf("export TLS material: %w", err)
	}
	routeKey, err := os.ReadFile(cfg.RouteSigningKeyFile)
	if err != nil {
		return fmt.Errorf("read route signing key: %w", err)
	}
	signer, err := proxy.NewSigner(routeKey)
	if err != nil {
		return fmt.Errorf("initialize route signer: %w", err)
	}
	certificateMount, err := bridgedocker.ParseMount(cfg.ProxyCertificateMount)
	if err != nil {
		return fmt.Errorf("parse proxy certificate mount: %w", err)
	}
	registry := prometheus.NewRegistry()
	metrics, err := observability.NewMetrics(registry)
	if err != nil {
		return fmt.Errorf("initialize metrics: %w", err)
	}
	docker, err := bridgedocker.NewAdapterForHost(cfg.DockerHost)
	if err != nil {
		return err
	}
	reconciler, err := bridgedocker.NewReconciler(docker, bridgedocker.ReconcileConfig{MasterID: authority.MasterID(), ProxyImage: cfg.ProxyImage, PortStart: cfg.ProxyPortStart, PortEnd: cfg.ProxyPortEnd, Signer: signer, RouteTokenLifetime: cfg.RouteTokenLifetime, RouteTokenRefreshBefore: cfg.RouteTokenRefreshBefore, CertificateMount: certificateMount, StateStore: state.NewReconcilerStore(cfg.DataDir)})
	if err != nil {
		return fmt.Errorf("initialize Docker reconciler: %w", err)
	}
	if err := reconciler.Restore(); err != nil {
		return err
	}
	control, err := bridgecontrol.NewServer(authority, bridgecontrol.ServerConfig{
		EndpointAllowlist: bridgecontrol.EndpointAllowlist{CIDRs: cfg.EndpointCIDRs, DNSNames: cfg.EndpointDNSNames},
		SnapshotHandler:   reconciler,
		Metrics:           metrics,
	})
	if err != nil {
		return fmt.Errorf("initialize control server: %w", err)
	}
	tlsConfig, err := control.TLSConfig()
	if err != nil {
		return err
	}
	controlListener, err := net.Listen("tcp", cfg.ControlAddress)
	if err != nil {
		return fmt.Errorf("listen for control connections: %w", err)
	}
	defer func() { _ = controlListener.Close() }()
	var enrollmentListener net.Listener
	var enrollmentServer *grpc.Server
	if cfg.EnrollmentAddress != "" {
		enrollment, code, enrollmentErr := bridgecontrol.NewPairingEnrollmentServer(authority, func(next string) {
			logger.Info("bridge-master development pairing code", "pairing_code", next)
		})
		if enrollmentErr != nil {
			return fmt.Errorf("initialize development enrollment: %w", enrollmentErr)
		}
		logger.Info("bridge-master development pairing code", "pairing_code", code)
		enrollmentTLS, enrollmentErr := enrollment.TLSConfig()
		if enrollmentErr != nil {
			return enrollmentErr
		}
		enrollmentListener, enrollmentErr = net.Listen("tcp", cfg.EnrollmentAddress)
		if enrollmentErr != nil {
			return fmt.Errorf("listen for enrollment connections: %w", enrollmentErr)
		}
		defer func() { _ = enrollmentListener.Close() }()
		enrollmentServer = grpc.NewServer(grpc.Creds(credentials.NewTLS(enrollmentTLS)))
		enrollment.Register(enrollmentServer)
	}

	var ready atomic.Bool
	operations := &http.Server{Addr: cfg.ObservabilityAddress, Handler: observability.NewHTTPHandler(observability.HTTPConfig{Registry: registry, Ready: ready.Load})}
	operationsListener, err := net.Listen("tcp", cfg.ObservabilityAddress)
	if err != nil {
		return fmt.Errorf("listen for operational HTTP: %w", err)
	}
	defer func() { _ = operationsListener.Close() }()

	grpcServer := grpc.NewServer(grpc.Creds(credentials.NewTLS(tlsConfig)))
	control.Register(grpcServer)
	errs := make(chan error, 4)
	go serve(errs, "route token refresh", func() error { return reconciler.Run(ctx) })
	go serve(errs, "control gRPC", func() error { return grpcServer.Serve(controlListener) })
	if enrollmentServer != nil {
		go serve(errs, "development enrollment gRPC", func() error { return enrollmentServer.Serve(enrollmentListener) })
	}
	go serve(errs, "operational HTTP", func() error { return operations.Serve(operationsListener) })
	ready.Store(true)
	logger.Info("bridge-master started", "control_address", cfg.ControlAddress, "observability_address", cfg.ObservabilityAddress)

	var serveErr error
	select {
	case <-ctx.Done():
		logger.Info("bridge-master shutting down", "reason", ctx.Err())
	case serveErr = <-errs:
	}
	ready.Store(false)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := operations.Shutdown(shutdownCtx); err != nil {
		logger.Error("shutdown operational HTTP", "error", err)
	}
	gracefulStop(grpcServer, shutdownCtx)
	if enrollmentServer != nil {
		gracefulStop(enrollmentServer, shutdownCtx)
	}
	if serveErr != nil {
		return serveErr
	}
	return nil
}

func serve(errs chan<- error, name string, serve func() error) {
	if err := serve(); err != nil && !errors.Is(err, net.ErrClosed) && !errors.Is(err, http.ErrServerClosed) {
		errs <- fmt.Errorf("serve %s: %w", name, err)
	}
}

func gracefulStop(server *grpc.Server, ctx context.Context) {
	done := make(chan struct{})
	go func() {
		server.GracefulStop()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		server.Stop()
	}
}
