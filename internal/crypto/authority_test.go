package crypto

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"

	"github.com/traefik/traefik-bridge/internal/state"
)

func TestInitializeReusesPersistedAuthority(t *testing.T) {
	store := state.NewStore(filepath.Join(t.TempDir(), "master"))
	first, err := Initialize(store)
	if err != nil {
		t.Fatalf("first Initialize() error = %v", err)
	}
	second, err := Initialize(store)
	if err != nil {
		t.Fatalf("second Initialize() error = %v", err)
	}
	if string(first.CACertificate()) != string(second.CACertificate()) {
		t.Error("CA certificate changed after restart")
	}
	if string(first.MasterServer().CertificatePEM) != string(second.MasterServer().CertificatePEM) {
		t.Error("master server certificate changed after restart")
	}
	if string(first.MasterClient().CertificatePEM) != string(second.MasterClient().CertificatePEM) {
		t.Error("master client certificate changed after restart")
	}
	if string(first.RouteSigningKey()) != string(second.RouteSigningKey()) || len(first.RouteSigningKey()) < 32 {
		t.Error("route signing key was not persisted")
	}
}

func TestInitializeMigratesMissingRouteSigningKey(t *testing.T) {
	store := state.NewStore(filepath.Join(t.TempDir(), "master"))
	authority, err := Initialize(store)
	if err != nil {
		t.Fatal(err)
	}
	authority.state.RouteSigningKey = nil
	if err := store.Save(authority.state); err != nil {
		t.Fatal(err)
	}
	migrated, err := Initialize(store)
	if err != nil {
		t.Fatal(err)
	}
	if len(migrated.RouteSigningKey()) < 32 {
		t.Fatal("migration did not create a route signing key")
	}
	persisted, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if string(persisted.RouteSigningKey) != string(migrated.RouteSigningKey()) {
		t.Fatal("migration did not persist route signing key")
	}
}

func TestExportPEM(t *testing.T) {
	authority, err := Initialize(state.NewStore(filepath.Join(t.TempDir(), "master")))
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "certs")
	if err := authority.ExportPEM(dir); err != nil {
		t.Fatal(err)
	}
	for _, file := range []struct {
		name string
		want []byte
		mode os.FileMode
	}{
		{"ca.crt", authority.CACertificate(), 0o644},
		{"master-client.crt", authority.MasterClient().CertificatePEM, 0o644},
		{"master-client.key", authority.MasterClient().PrivateKeyPEM, 0o600},
	} {
		contents, err := os.ReadFile(filepath.Join(dir, file.name))
		if err != nil {
			t.Fatal(err)
		}
		if string(contents) != string(file.want) {
			t.Errorf("%s contents differ", file.name)
		}
		info, err := os.Stat(filepath.Join(dir, file.name))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != file.mode {
			t.Errorf("%s mode = %o, want %o", file.name, info.Mode().Perm(), file.mode)
		}
	}
}

func TestIssueSlavePersistsTrackedCertificate(t *testing.T) {
	store := state.NewStore(filepath.Join(t.TempDir(), "master"))
	authority, err := Initialize(store)
	if err != nil {
		t.Fatal(err)
	}
	csrDER := newCSR(t, "slave-a")
	issued, err := authority.IssueSlave("slave-a", csrDER)
	if err != nil {
		t.Fatalf("IssueSlave() error = %v", err)
	}
	if issued.Serial == "" || len(issued.CertificatePEM) == 0 {
		t.Errorf("IssueSlave() = %+v, want serial and certificate", issued)
	}

	certBlock, _ := pem.Decode(issued.CertificatePEM)
	if certBlock == nil {
		t.Fatal("issued certificate is not PEM")
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if cert.Subject.CommonName != "bridge-slave:slave-a" {
		t.Errorf("certificate common name = %q", cert.Subject.CommonName)
	}
	if !hasUsage(cert.ExtKeyUsage, x509.ExtKeyUsageClientAuth) || !hasUsage(cert.ExtKeyUsage, x509.ExtKeyUsageServerAuth) {
		t.Errorf("certificate usages = %v, want client and server auth", cert.ExtKeyUsage)
	}

	restarted, err := Initialize(store)
	if err != nil {
		t.Fatal(err)
	}
	tracked := restarted.state.Slaves["slave-a"]
	if tracked.Serial != issued.Serial || string(tracked.CertificatePEM) != string(issued.CertificatePEM) {
		t.Errorf("tracked certificate = %+v, want %+v", tracked, issued)
	}
}

func TestPairingCodeRoundTrip(t *testing.T) {
	authority, err := Initialize(state.NewStore(filepath.Join(t.TempDir(), "master")))
	if err != nil {
		t.Fatal(err)
	}
	_, code, err := authority.RotateEnrollment()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParsePairingCode(code)
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.Secret) != 32 || len(parsed.SPKIPin) != 32 || parsed.String() != code {
		t.Fatalf("invalid pairing code round trip: %#v", parsed)
	}
	if _, err := ParsePairingCode("bridge-pair-v1.bad.code"); err == nil {
		t.Fatal("accepted malformed pairing code")
	}
}

func newCSR(t *testing.T, commonName string) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: commonName}}, key)
	if err != nil {
		t.Fatal(err)
	}
	return csr
}

func hasUsage(usages []x509.ExtKeyUsage, want x509.ExtKeyUsage) bool {
	for _, usage := range usages {
		if usage == want {
			return true
		}
	}
	return false
}
