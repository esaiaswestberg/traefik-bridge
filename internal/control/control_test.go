package control

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"net"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	controlv1 "github.com/traefik/traefik-bridge/api/gen/go/control/v1"
	bridgecrypto "github.com/traefik/traefik-bridge/internal/crypto"
	"github.com/traefik/traefik-bridge/internal/state"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/test/bufconn"
)

const bufnetTarget = "passthrough:///bufnet"

func TestMTLSIdentityMustMatchHello(t *testing.T) {
	authority := newAuthority(t)
	pair := issueSlave(t, authority, "slave-a")
	_, dial := startServer(t, authority, ServerConfig{EndpointAllowlist: EndpointAllowlist{DNSNames: []string{"node.example"}}})
	connection, err := dial(pair)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close() }()
	stream, err := controlv1.NewControlServiceClient(connection).Connect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(hello("slave-b", "node.example")); err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Recv(); err == nil {
		t.Fatal("Connect succeeded with a certificate for a different slave")
	}
}

func TestEndpointAllowlist(t *testing.T) {
	authority := newAuthority(t)
	pair := issueSlave(t, authority, "slave-a")
	_, dial := startServer(t, authority, ServerConfig{EndpointAllowlist: EndpointAllowlist{CIDRs: []string{"10.0.0.0/8"}, DNSNames: []string{"node.example"}}})
	connection, err := dial(pair)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close() }()
	stream, err := controlv1.NewControlServiceClient(connection).Connect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(hello("slave-a", "192.168.1.10")); err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Recv(); err == nil {
		t.Fatal("Connect accepted an endpoint outside the CIDR allowlist")
	}
}

func TestClientResyncAndDisconnectGrace(t *testing.T) {
	authority := newAuthority(t)
	pair := issueSlave(t, authority, "slave-a")
	server, dial := startServer(t, authority, ServerConfig{
		EndpointAllowlist: EndpointAllowlist{DNSNames: []string{"node.example"}},
		DisconnectGrace:   80 * time.Millisecond,
	})
	var snapshots atomic.Int32
	client, err := NewClient(ClientConfig{
		SlaveID:           "slave-a",
		Endpoint:          &controlv1.SlaveEndpoint{Host: "node.example", Port: 443},
		HeartbeatInterval: 10 * time.Millisecond,
		ReconnectDelay:    10 * time.Millisecond,
		Dial:              func(context.Context) (*grpc.ClientConn, error) { return dial(pair) },
		Snapshot: func(context.Context) (*controlv1.FullSnapshot, error) {
			snapshots.Add(1)
			return &controlv1.FullSnapshot{Revision: 7}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- client.Run(ctx) }()
	waitFor(t, time.Second, func() bool {
		status, ok := server.Status("slave-a")
		return ok && status.Connected && status.Revision == 7 && snapshots.Load() == 1
	})
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	waitFor(t, time.Second, func() bool {
		status, ok := server.Status("slave-a")
		return ok && !status.Connected
	})
	status, ok := server.Status("slave-a")
	if !ok || status.Connected || !status.Available {
		t.Fatalf("status after disconnect = %+v, exists=%v; want disconnected but within grace", status, ok)
	}
	time.Sleep(100 * time.Millisecond)
	status, _ = server.Status("slave-a")
	if status.Available {
		t.Fatalf("status after grace = %+v, want unavailable", status)
	}
}

func TestClientPublishesQueuedSnapshots(t *testing.T) {
	authority := newAuthority(t)
	pair := issueSlave(t, authority, "slave-a")
	var revision atomic.Uint64
	server, dial := startServer(t, authority, ServerConfig{
		EndpointAllowlist: EndpointAllowlist{DNSNames: []string{"node.example"}},
		SnapshotHandler: snapshotHandler(func(_ context.Context, _ string, _ *controlv1.SlaveEndpoint, snapshot *controlv1.FullSnapshot) ([]*controlv1.ResourceRejection, error) {
			revision.Store(snapshot.GetRevision())
			return nil, nil
		}),
	})
	_ = server
	client, err := NewClient(ClientConfig{SlaveID: "slave-a", Endpoint: &controlv1.SlaveEndpoint{Host: "node.example", Port: 443}, ReconnectDelay: time.Millisecond, Dial: func(context.Context) (*grpc.ClientConn, error) { return dial(pair) }})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.PublishSnapshot(context.Background(), &controlv1.FullSnapshot{Revision: 1}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- client.Run(ctx) }()
	waitFor(t, time.Second, func() bool { return revision.Load() == 1 })
	if err := client.PublishSnapshot(context.Background(), &controlv1.FullSnapshot{Revision: 2}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, time.Second, func() bool { return revision.Load() == 2 })
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

type snapshotHandler func(context.Context, string, *controlv1.SlaveEndpoint, *controlv1.FullSnapshot) ([]*controlv1.ResourceRejection, error)

func (f snapshotHandler) Apply(ctx context.Context, slaveID string, endpoint *controlv1.SlaveEndpoint, snapshot *controlv1.FullSnapshot) ([]*controlv1.ResourceRejection, error) {
	return f(ctx, slaveID, endpoint, snapshot)
}

func newAuthority(t *testing.T) *bridgecrypto.Authority {
	t.Helper()
	authority, err := bridgecrypto.Initialize(state.NewStore(filepath.Join(t.TempDir(), "state")))
	if err != nil {
		t.Fatal(err)
	}
	return authority
}

func issueSlave(t *testing.T, authority *bridgecrypto.Authority, id string) state.KeyPair {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: id}}, key)
	if err != nil {
		t.Fatal(err)
	}
	issued, err := authority.IssueSlave(id, csr)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return state.KeyPair{CertificatePEM: issued.CertificatePEM, PrivateKeyPEM: pemEncode(keyDER)}
}

func pemEncode(bytes []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: bytes})
}

func startServer(t *testing.T, authority *bridgecrypto.Authority, config ServerConfig) (*Server, func(state.KeyPair) (*grpc.ClientConn, error)) {
	t.Helper()
	server, err := NewServer(authority, config)
	if err != nil {
		t.Fatal(err)
	}
	tlsConfig, err := server.TLSConfig()
	if err != nil {
		t.Fatal(err)
	}
	listener := bufconn.Listen(1024 * 1024)
	grpcServer := grpc.NewServer(grpc.Creds(credentials.NewTLS(tlsConfig)))
	server.Register(grpcServer)
	go func() { _ = grpcServer.Serve(listener) }()
	t.Cleanup(func() { grpcServer.Stop() })
	return server, func(pair state.KeyPair) (*grpc.ClientConn, error) {
		clientTLS, tlsErr := TLSConfig(pair, authority.CACertificate(), "bridge-master")
		if tlsErr != nil {
			return nil, tlsErr
		}
		return grpc.NewClient(bufnetTarget, grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
			return listener.Dial()
		}), grpc.WithTransportCredentials(credentials.NewTLS(clientTLS)))
	}
}

func hello(id, host string) *controlv1.SlaveToMaster {
	return &controlv1.SlaveToMaster{Payload: &controlv1.SlaveToMaster_Hello{Hello: &controlv1.ControlHello{SlaveId: id, Endpoint: &controlv1.SlaveEndpoint{Host: host, Port: 443}}}}
}

func waitFor(t *testing.T, timeout time.Duration, predicate func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if predicate() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition was not met before timeout")
}
