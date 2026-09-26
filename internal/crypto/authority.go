// Package crypto initializes the bridge certificate authority and issues slave certificates.
package crypto

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/traefik/traefik-bridge/internal/state"
)

const (
	stateVersion = 1
	caLifetime   = 10 * 365 * 24 * time.Hour
	leafLifetime = 365 * 24 * time.Hour
)

// Authority owns the persisted bridge CA and issued slave certificates.
type Authority struct {
	store *state.Store
	state *state.State
	ca    *x509.Certificate
	key   *ecdsa.PrivateKey
	mu    sync.Mutex
}

// Initialize loads an existing authority or creates and persists a new one.
func Initialize(store *state.Store) (*Authority, error) {
	if store == nil {
		return nil, errors.New("state store is required")
	}

	persisted, err := store.Load()
	if state.IsNotExist(err) {
		persisted, err = newState()
		if err == nil {
			err = store.Save(persisted)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("initialize certificate authority: %w", err)
	}
	if persisted.Version != stateVersion {
		return nil, fmt.Errorf("unsupported master state version %d", persisted.Version)
	}
	ca, key, err := parseCA(persisted.CA)
	if err != nil {
		return nil, fmt.Errorf("load certificate authority: %w", err)
	}
	return &Authority{store: store, state: persisted, ca: ca, key: key}, nil
}

// MasterServer returns the PEM certificate and key for enrollment/control TLS.
func (a *Authority) MasterServer() state.KeyPair { return a.state.MasterServer }

// MasterClient returns the PEM certificate and key for master-to-slave TLS.
func (a *Authority) MasterClient() state.KeyPair { return a.state.MasterClient }

// MatchesSlave reports whether cert is the currently issued certificate for
// slaveID. TLS verification is performed by the caller.
func (a *Authority) MatchesSlave(slaveID string, cert *x509.Certificate) bool {
	if cert == nil || cert.Subject.CommonName != "bridge-slave:"+slaveID {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	record, ok := a.state.Slaves[slaveID]
	return ok && record.Serial == cert.SerialNumber.Text(16)
}

// CACertificate returns the PEM-encoded bridge CA certificate.
func (a *Authority) CACertificate() []byte { return append([]byte(nil), a.state.CA.CertificatePEM...) }

// IssueSlave signs csrDER for slaveID and records the issued certificate. The
// caller must authenticate enrollment before calling this method.
func (a *Authority) IssueSlave(slaveID string, csrDER []byte) (state.Slave, error) {
	if strings.TrimSpace(slaveID) == "" {
		return state.Slave{}, errors.New("slave ID is required")
	}
	csr, err := x509.ParseCertificateRequest(csrDER)
	if err != nil {
		return state.Slave{}, fmt.Errorf("parse slave CSR: %w", err)
	}
	if err := csr.CheckSignature(); err != nil {
		return state.Slave{}, fmt.Errorf("verify slave CSR: %w", err)
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	serial, err := randomSerial()
	if err != nil {
		return state.Slave{}, err
	}
	now := time.Now().UTC()
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "bridge-slave:" + slaveID},
		NotBefore:             now.Add(-5 * time.Minute),
		NotAfter:              now.Add(leafLifetime),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              csr.DNSNames,
		IPAddresses:           csr.IPAddresses,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, a.ca, csr.PublicKey, a.key)
	if err != nil {
		return state.Slave{}, fmt.Errorf("sign slave certificate: %w", err)
	}
	record := state.Slave{CertificatePEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), Serial: serial.Text(16), IssuedAt: now, NotAfter: template.NotAfter}
	a.state.Slaves[slaveID] = record
	if err := a.store.Save(a.state); err != nil {
		return state.Slave{}, fmt.Errorf("persist slave certificate: %w", err)
	}
	return record, nil
}

// Enrollment is intentionally not implemented here. It must use an audited
// PAKE library before IssueSlave is exposed to unauthenticated peers.
type Enrollment interface {
	Authenticate() error
}

func newState() (*state.State, error) {
	masterID, err := randomID()
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate CA key: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	caTemplate := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "Traefik Bridge CA"}, NotBefore: now.Add(-5 * time.Minute), NotAfter: now.Add(caLifetime), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, fmt.Errorf("create CA certificate: %w", err)
	}
	ca := state.KeyPair{CertificatePEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), PrivateKeyPEM: encodeKey(caKey)}
	server, err := issueIdentity(caTemplate, caKey, "bridge-master-server", []string{"bridge-master"}, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth})
	if err != nil {
		return nil, err
	}
	client, err := issueIdentity(caTemplate, caKey, "bridge-master-client", nil, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth})
	if err != nil {
		return nil, err
	}
	return &state.State{Version: stateVersion, MasterID: masterID, CA: ca, MasterServer: server, MasterClient: client, Slaves: make(map[string]state.Slave)}, nil
}

func issueIdentity(ca *x509.Certificate, caKey *ecdsa.PrivateKey, commonName string, dnsNames []string, usages []x509.ExtKeyUsage) (state.KeyPair, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return state.KeyPair{}, err
	}
	serial, err := randomSerial()
	if err != nil {
		return state.KeyPair{}, err
	}
	now := time.Now().UTC()
	template := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: commonName}, DNSNames: dnsNames, NotBefore: now.Add(-5 * time.Minute), NotAfter: now.Add(leafLifetime), KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment, ExtKeyUsage: usages, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, template, ca, &key.PublicKey, caKey)
	if err != nil {
		return state.KeyPair{}, fmt.Errorf("create %s certificate: %w", commonName, err)
	}
	return state.KeyPair{CertificatePEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), PrivateKeyPEM: encodeKey(key)}, nil
}

func parseCA(pair state.KeyPair) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	certBlock, _ := pem.Decode(pair.CertificatePEM)
	keyBlock, _ := pem.Decode(pair.PrivateKeyPEM)
	if certBlock == nil || keyBlock == nil {
		return nil, nil, errors.New("invalid CA PEM")
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, nil, err
	}
	key, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, nil, err
	}
	if !cert.IsCA {
		return nil, nil, errors.New("certificate is not a CA")
	}
	return cert, key, nil
}

func encodeKey(key *ecdsa.PrivateKey) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: mustMarshalKey(key)})
}

func mustMarshalKey(key *ecdsa.PrivateKey) []byte {
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		panic(err)
	}
	return der
}

func randomID() (string, error) {
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes), nil
}

func randomSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return nil, fmt.Errorf("generate certificate serial: %w", err)
	}
	return serial, nil
}
