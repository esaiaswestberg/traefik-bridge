package crypto

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
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
