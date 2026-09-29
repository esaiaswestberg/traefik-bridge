package control

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/bytemare/opaque"
	controlv1 "github.com/traefik/traefik-bridge/api/gen/go/control/v1"
	bridgecrypto "github.com/traefik/traefik-bridge/internal/crypto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/test/bufconn"
)

func TestEnrollmentSuccess(t *testing.T) {
	authority := newAuthority(t)
	listener, dial := startEnrollmentServer(t, authority, []byte("0123456789abcdef0123456789abcdef"))
	_ = listener
	dir := t.TempDir()
	pair, ca, slaveID, err := Enroll(context.Background(), EnrollmentClientConfig{Address: "bufnet", ServerName: "bridge-master", BootstrapCA: authority.CACertificate(), Secret: []byte("0123456789abcdef0123456789abcdef"), SlaveID: "slave-a", CertificateFile: filepath.Join(dir, "slave.crt"), PrivateKeyFile: filepath.Join(dir, "slave.key"), CAFile: filepath.Join(dir, "ca.crt"), CSRFile: filepath.Join(dir, "slave.csr"), DialContext: dial})
	if err != nil {
		t.Fatal(err)
	}
	if slaveID != "slave-a" || len(pair.CertificatePEM) == 0 || len(ca) == 0 {
		t.Fatalf("unexpected enrollment result: slave=%q certificate=%d ca=%d", slaveID, len(pair.CertificatePEM), len(ca))
	}
	for _, path := range []string{filepath.Join(dir, "slave.crt"), filepath.Join(dir, "slave.key"), filepath.Join(dir, "slave.csr"), filepath.Join(dir, "ca.crt")} {
		info, statErr := os.Stat(path)
		if statErr != nil {
			t.Fatal(statErr)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("credential %q permissions = %o, want 600", path, info.Mode().Perm())
		}
	}
	if !authority.HasSlave("slave-a") {
		t.Fatal("master did not record enrolled slave")
	}
}

func TestEnrollmentRejectsBadSecretWithoutCertificate(t *testing.T) {
	authority := newAuthority(t)
	_, dial := startEnrollmentServer(t, authority, []byte("0123456789abcdef0123456789abcdef"))
	dir := t.TempDir()
	_, _, _, err := Enroll(context.Background(), EnrollmentClientConfig{Address: "bufnet", ServerName: "bridge-master", BootstrapCA: authority.CACertificate(), Secret: []byte("fedcba9876543210fedcba9876543210"), SlaveID: "slave-a", CertificateFile: filepath.Join(dir, "slave.crt"), PrivateKeyFile: filepath.Join(dir, "slave.key"), CAFile: filepath.Join(dir, "ca.crt"), CSRFile: filepath.Join(dir, "slave.csr"), DialContext: dial})
	if err == nil {
		t.Fatal("enrollment succeeded with a bad secret")
	}
	if authority.HasSlave("slave-a") {
		t.Fatal("master issued a certificate for a bad secret")
	}
	for _, name := range []string{"slave.crt", "slave.key", "ca.crt", "slave.csr"} {
		if _, statErr := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(statErr) {
			t.Fatalf("failed enrollment left credential %q behind: %v", name, statErr)
		}
	}
}

func TestPairingEnrollmentWithoutBootstrapCA(t *testing.T) {
	authority := newAuthority(t)
	_, code, dial := startPairingEnrollmentServer(t, authority)
	dir := t.TempDir()
	pair, ca, slaveID, err := Enroll(context.Background(), EnrollmentClientConfig{Address: "bufnet", PairingCode: code, DataHost: "slave.example", SlaveID: "slave-a", CertificateFile: filepath.Join(dir, "slave.crt"), PrivateKeyFile: filepath.Join(dir, "slave.key"), CAFile: filepath.Join(dir, "ca.crt"), CSRFile: filepath.Join(dir, "slave.csr"), DialContext: dial})
	if err != nil {
		t.Fatal(err)
	}
	if slaveID != "slave-a" || len(pair.CertificatePEM) == 0 || len(ca) == 0 {
		t.Fatalf("unexpected enrollment result: slave=%q certificate=%d ca=%d", slaveID, len(pair.CertificatePEM), len(ca))
	}
}

func TestPairingEnrollmentRejectsWrongCodeAndPin(t *testing.T) {
	for _, mutate := range []func(*bridgecrypto.PairingCode){
		func(code *bridgecrypto.PairingCode) { code.Secret[0] ^= 1 },
		func(code *bridgecrypto.PairingCode) { code.SPKIPin[0] ^= 1 },
	} {
		authority := newAuthority(t)
		_, code, dial := startPairingEnrollmentServer(t, authority)
		parsed, err := bridgecrypto.ParsePairingCode(code)
		if err != nil {
			t.Fatal(err)
		}
		mutate(&parsed)
		dir := t.TempDir()
		_, _, _, err = Enroll(context.Background(), EnrollmentClientConfig{Address: "bufnet", PairingCode: parsed.String(), SlaveID: "slave-a", CertificateFile: filepath.Join(dir, "slave.crt"), PrivateKeyFile: filepath.Join(dir, "slave.key"), CAFile: filepath.Join(dir, "ca.crt"), CSRFile: filepath.Join(dir, "slave.csr"), DialContext: dial})
		if err == nil {
			t.Fatal("enrollment succeeded with an altered pairing code")
		}
	}
}

func TestPairingCodeRotatesAfterEnrollment(t *testing.T) {
	authority := newAuthority(t)
	_, code, dial := startPairingEnrollmentServer(t, authority)
	enrollWithCode(t, dial, code, "slave-a")
	dir := t.TempDir()
	_, _, _, err := Enroll(context.Background(), EnrollmentClientConfig{Address: "bufnet", PairingCode: code, SlaveID: "slave-b", CertificateFile: filepath.Join(dir, "slave.crt"), PrivateKeyFile: filepath.Join(dir, "slave.key"), CAFile: filepath.Join(dir, "ca.crt"), CSRFile: filepath.Join(dir, "slave.csr"), DialContext: dial})
	if err == nil {
		t.Fatal("reused pairing code enrolled a second slave")
	}
}

func TestEnrollmentRequiresClientProof(t *testing.T) {
	authority := newAuthority(t)
	listener, _ := startEnrollmentServer(t, authority, []byte("0123456789abcdef0123456789abcdef"))
	tlsConfig := &tls.Config{RootCAs: mustPool(t, authority.CACertificate()), ServerName: "bridge-master", MinVersion: tls.VersionTLS13}
	connection, err := grpc.NewClient("bufnet", grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }), grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close() }()
	stream, err := controlv1.NewEnrollmentServiceClient(connection).Enroll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	client, err := opaque.DefaultConfiguration().Client()
	if err != nil {
		t.Fatal(err)
	}
	defer client.ClearState()
	ke1, err := client.GenerateKE1([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: "slave-a"}}, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&controlv1.EnrollmentRequest{EnrollmentId: []byte("request"), SlaveId: "slave-a", PakeMessage: ke1.Serialize(), CsrDer: csr}); err != nil {
		t.Fatal(err)
	}
	response, err := stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	ke2, err := client.Deserialize.KE2(response.GetPakeMessage())
	if err != nil {
		t.Fatal(err)
	}
	// GenerateKE3 verifies the master. Corrupt the resulting client proof to
	// confirm the master refuses to issue before authenticating the slave.
	ke3, _, _, err := client.GenerateKE3(ke2, []byte("traefik-bridge:cluster"), response.GetServerIdentity())
	if err != nil {
		t.Fatal(err)
	}
	ke3.ClientMac[0] ^= 1
	if err := stream.Send(&controlv1.EnrollmentRequest{EnrollmentId: []byte("request"), PakeMessage: ke3.Serialize()}); err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Recv(); err == nil {
		t.Fatal("server accepted a corrupt client proof")
	}
	if authority.HasSlave("slave-a") {
		t.Fatal("server issued a certificate before authenticating the client")
	}
}

func TestEnrollmentDoesNotOverwriteCredentials(t *testing.T) {
	dir := t.TempDir()
	certificate := filepath.Join(dir, "slave.crt")
	if err := os.WriteFile(certificate, []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, _, err := Enroll(context.Background(), EnrollmentClientConfig{Secret: []byte("0123456789abcdef0123456789abcdef"), CertificateFile: certificate, PrivateKeyFile: filepath.Join(dir, "slave.key"), CAFile: filepath.Join(dir, "ca.crt")})
	if err == nil {
		t.Fatal("enrollment overwrote existing credentials")
	}
	contents, err := os.ReadFile(certificate)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "existing" {
		t.Fatal("existing certificate was modified")
	}
}

func startEnrollmentServer(t *testing.T, authority *bridgecrypto.Authority, secret []byte) (*bufconn.Listener, func(context.Context, string) (net.Conn, error)) {
	t.Helper()
	service, err := NewEnrollmentServer(authority, secret)
	if err != nil {
		t.Fatal(err)
	}
	tlsConfig, err := service.TLSConfig()
	if err != nil {
		t.Fatal(err)
	}
	listener := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(tlsConfig)))
	service.Register(server)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	return listener, func(context.Context, string) (net.Conn, error) { return listener.Dial() }
}

func startPairingEnrollmentServer(t *testing.T, authority *bridgecrypto.Authority) (*bufconn.Listener, string, func(context.Context, string) (net.Conn, error)) {
	t.Helper()
	service, code, err := NewPairingEnrollmentServer(authority, nil)
	if err != nil {
		t.Fatal(err)
	}
	tlsConfig, err := service.TLSConfig()
	if err != nil {
		t.Fatal(err)
	}
	listener := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(tlsConfig)))
	service.Register(server)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	return listener, code, func(context.Context, string) (net.Conn, error) { return listener.Dial() }
}

func enrollWithCode(t *testing.T, dial func(context.Context, string) (net.Conn, error), code, slaveID string) {
	t.Helper()
	dir := t.TempDir()
	_, _, _, err := Enroll(context.Background(), EnrollmentClientConfig{Address: "bufnet", PairingCode: code, SlaveID: slaveID, CertificateFile: filepath.Join(dir, "slave.crt"), PrivateKeyFile: filepath.Join(dir, "slave.key"), CAFile: filepath.Join(dir, "ca.crt"), CSRFile: filepath.Join(dir, "slave.csr"), DialContext: dial})
	if err != nil {
		t.Fatal(err)
	}
}

func mustPool(t *testing.T, ca []byte) *x509.CertPool {
	t.Helper()
	pool, err := certificatePool(ca)
	if err != nil {
		t.Fatal(err)
	}
	return pool
}
