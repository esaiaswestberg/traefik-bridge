// bridge-slave publishes local Docker configuration and serves remote traffic.
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
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	controlv1 "github.com/traefik/traefik-bridge/api/gen/go/control/v1"
	"github.com/traefik/traefik-bridge/internal/config"
	bridgecontrol "github.com/traefik/traefik-bridge/internal/control"
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
		logger.Error("bridge-slave fatal error", "error", err)
		os.Exit(1)
	}
	stop()
}

func run(ctx context.Context, logger *slog.Logger) error {
	cfg, err := config.LoadSlave()
	if err != nil {
		return fmt.Errorf("load configuration: %w", err)
	}
	pair, ca, routeKey, err := loadCredentials(cfg)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) || (cfg.DevelopmentPairingCode == "" && cfg.EnrollmentSecretFile == "") {
			return err
		}
		enrollmentConfig := bridgecontrol.EnrollmentClientConfig{Address: cfg.EnrollmentAddress, ServerName: cfg.MasterServerName, DataHost: dataHost(cfg.DataAddress), PairingCode: cfg.DevelopmentPairingCode, SlaveID: cfg.SlaveID, CertificateFile: cfg.CertificateFile, PrivateKeyFile: cfg.PrivateKeyFile, CAFile: cfg.CAFile, CSRFile: cfg.EnrollmentCSRFile}
		if cfg.DevelopmentPairingCode == "" {
			secret, readErr := os.ReadFile(cfg.EnrollmentSecretFile)
			if readErr != nil {
				return fmt.Errorf("read development enrollment secret: %w", readErr)
			}
			bootstrapCA, readErr := os.ReadFile(cfg.EnrollmentCAFile)
			if readErr != nil {
				return fmt.Errorf("read enrollment CA: %w", readErr)
			}
			enrollmentConfig.Secret, enrollmentConfig.BootstrapCA = secret, bootstrapCA
		}
		pair, ca, _, err = bridgecontrol.Enroll(ctx, enrollmentConfig)
		if err != nil {
			return fmt.Errorf("development enrollment: %w", err)
		}
		routeKey, err = os.ReadFile(cfg.RouteSigningKeyFile)
		if err != nil {
			return fmt.Errorf("read route signing key: %w", err)
		}
	}
	clientTLS, err := bridgecontrol.TLSConfig(pair, ca, cfg.MasterServerName)
	if err != nil {
		return fmt.Errorf("load control TLS: %w", err)
	}
	endpoint, err := advertisedEndpoint(cfg.DataAddress)
	if err != nil {
		return err
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
	client, err := bridgecontrol.NewClient(bridgecontrol.ClientConfig{
		SlaveID: cfg.SlaveID, Endpoint: endpoint, Metrics: metrics,
		Dial: func(context.Context) (*grpc.ClientConn, error) {
			return grpc.NewClient(cfg.MasterAddress, grpc.WithTransportCredentials(credentials.NewTLS(clientTLS)))
		},
	})
	if err != nil {
		return fmt.Errorf("initialize control client: %w", err)
	}
	discovery, err := bridgedocker.NewDiscovery(docker, client, bridgedocker.Config{DefaultNetwork: cfg.DefaultNetwork, ConstraintLabels: cfg.ConstraintLabels})
	if err != nil {
		return fmt.Errorf("initialize Docker discovery: %w", err)
	}
	services, err := discovery.Services(ctx)
	if err != nil {
		return fmt.Errorf("derive Docker service targets: %w", err)
	}
	signer, err := proxy.NewSigner(routeKey)
	if err != nil {
		return err
	}
	handler, err := proxy.NewSlave(proxy.SlaveConfig{Signer: signer, Services: services, Metrics: metrics})
	if err != nil {
		return fmt.Errorf("initialize data proxy: %w", err)
	}
	defer handler.Close()
	discovery.SetServicePoolUpdater(handler)
	dataTLS, err := dataTLSConfig(pair, ca)
	if err != nil {
		return err
	}
	dataListener, err := net.Listen("tcp", cfg.DataListenAddress)
	if err != nil {
		return fmt.Errorf("listen for data traffic: %w", err)
	}
	operationsListener, err := net.Listen("tcp", cfg.ObservabilityAddress)
	if err != nil {
		_ = dataListener.Close()
		return fmt.Errorf("listen for operational HTTP: %w", err)
	}
	var ready atomic.Bool
	dataServer := &http.Server{Handler: handler}
	operations := &http.Server{Handler: observability.NewHTTPHandler(observability.HTTPConfig{Registry: registry, Ready: ready.Load})}
	errs := make(chan error, 3)
	go serve(errs, "data HTTPS", func() error { return dataServer.Serve(tls.NewListener(dataListener, dataTLS)) })
	go serve(errs, "operational HTTP", func() error { return operations.Serve(operationsListener) })
	go func() { errs <- discovery.Run(ctx) }()
	go func() { errs <- client.Run(ctx) }()
	ready.Store(true)
	logger.Info("bridge-slave started", "master_address", cfg.MasterAddress, "data_address", cfg.DataAddress, "data_listen_address", cfg.DataListenAddress, "observability_address", cfg.ObservabilityAddress)
	var serveErr error
	select {
	case <-ctx.Done():
		logger.Info("bridge-slave shutting down", "reason", ctx.Err())
	case serveErr = <-errs:
	}
	ready.Store(false)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := dataServer.Shutdown(shutdownCtx); err != nil {
		logger.Error("shutdown data HTTPS", "error", err)
	}
	if err := operations.Shutdown(shutdownCtx); err != nil {
		logger.Error("shutdown operational HTTP", "error", err)
	}
	if serveErr != nil {
		return serveErr
	}
	return nil
}

func loadCredentials(cfg config.Slave) (state.KeyPair, []byte, []byte, error) {
	certificate, err := os.ReadFile(cfg.CertificateFile)
	if err != nil {
		return state.KeyPair{}, nil, nil, fmt.Errorf("read slave certificate: %w", err)
	}
	key, err := os.ReadFile(cfg.PrivateKeyFile)
	if err != nil {
		return state.KeyPair{}, nil, nil, fmt.Errorf("read slave private key: %w", err)
	}
	ca, err := os.ReadFile(cfg.CAFile)
	if err != nil {
		return state.KeyPair{}, nil, nil, fmt.Errorf("read bridge CA: %w", err)
	}
	routeKey, err := os.ReadFile(cfg.RouteSigningKeyFile)
	if err != nil {
		return state.KeyPair{}, nil, nil, fmt.Errorf("read route signing key: %w", err)
	}
	return state.KeyPair{CertificatePEM: certificate, PrivateKeyPEM: key}, ca, routeKey, nil
}

func advertisedEndpoint(address string) (*controlv1.SlaveEndpoint, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil || host == "" || host == "0.0.0.0" || host == "::" {
		return nil, fmt.Errorf("BRIDGE_DATA_ADDRESS must contain a concrete host and port")
	}
	parsed, err := net.LookupPort("tcp", port)
	if err != nil || parsed == 0 {
		return nil, fmt.Errorf("BRIDGE_DATA_ADDRESS must contain a valid port")
	}
	return &controlv1.SlaveEndpoint{Host: host, Port: uint32(parsed)}, nil
}

func dataTLSConfig(pair state.KeyPair, ca []byte) (*tls.Config, error) {
	certificate, err := tls.X509KeyPair(pair.CertificatePEM, pair.PrivateKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("load data TLS certificate: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, errors.New("invalid bridge CA certificate")
	}
	return proxy.ServerTLSConfig(certificate, pool), nil
}

func dataHost(address string) string {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return address
	}
	return host
}

func serve(errs chan<- error, name string, serve func() error) {
	if err := serve(); err != nil && !errors.Is(err, net.ErrClosed) && !errors.Is(err, http.ErrServerClosed) {
		errs <- fmt.Errorf("serve %s: %w", name, err)
	}
}
