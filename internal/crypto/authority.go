// Package crypto initializes the bridge certificate authority and issues slave certificates.
package crypto

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/bytemare/opaque"
	"github.com/traefik/traefik-bridge/internal/state"
)

const (
	stateVersion     = 1
	caLifetime       = 10 * 365 * 24 * time.Hour
	leafLifetime     = 365 * 24 * time.Hour
	traefikTransport = `http:
  serversTransports:
    bridge-mtls:
      rootCAs:
        - /bridge/ca.crt
      certificates:
        - certFile: /bridge/master-client.crt
          keyFile: /bridge/master-client.key
      insecureSkipVerify: false
`
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
	return load(store, true)
}

// Load opens an existing authority. Unlike Initialize, it never creates
// certificate authority state, which makes it suitable for offline operators.
func Load(store *state.Store) (*Authority, error) {
	return load(store, false)
}

func load(store *state.Store, create bool) (*Authority, error) {
	if store == nil {
		return nil, errors.New("state store is required")
	}

	persisted, err := store.Load()
	if create && state.IsNotExist(err) {
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
	if len(persisted.RouteSigningKey) == 0 {
		persisted.RouteSigningKey, err = randomBytes(32)
		if err == nil {
			err = store.Save(persisted)
		}
		if err != nil {
			return nil, fmt.Errorf("migrate route signing key: %w", err)
		}
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

// MasterID returns the stable identifier assigned when the authority is created.
func (a *Authority) MasterID() string { return a.state.MasterID }

// RouteSigningKey returns the private key used to authenticate proxy routes.
func (a *Authority) RouteSigningKey() []byte { return append([]byte(nil), a.state.RouteSigningKey...) }

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

// HasSlave reports whether slaveID already has an active tracked certificate.
func (a *Authority) HasSlave(slaveID string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	_, ok := a.state.Slaves[slaveID]
	return ok
}

// ExportSlavePEM writes the CA certificate and issued slave certificate for
// manual installation. The corresponding private key remains with the CSR
// generator and is never handled by the master.
func (a *Authority) ExportSlavePEM(dir string, certificate []byte) error {
	if strings.TrimSpace(dir) == "" {
		return errors.New("certificate directory is required")
	}
	if len(certificate) == 0 {
		return errors.New("slave certificate is required")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create certificate directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("secure certificate directory: %w", err)
	}
	if err := writePEM(filepath.Join(dir, "ca.crt"), a.state.CA.CertificatePEM, 0o644); err != nil {
		return err
	}
	return writePEM(filepath.Join(dir, "slave.crt"), certificate, 0o644)
}

// ExportPEM atomically writes the CA and master client TLS material for
// Traefik's shared ServersTransport. Private keys remain owner-readable only.
func (a *Authority) ExportPEM(dir string) error {
	if strings.TrimSpace(dir) == "" {
		return errors.New("certificate directory is required")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create certificate directory: %w", err)
	}
	if err := writePEM(filepath.Join(dir, "ca.crt"), a.state.CA.CertificatePEM, 0o644); err != nil {
		return err
	}
	if err := writePEM(filepath.Join(dir, "master-client.crt"), a.state.MasterClient.CertificatePEM, 0o644); err != nil {
		return err
	}
	if err := writePEM(filepath.Join(dir, "master-client.key"), a.state.MasterClient.PrivateKeyPEM, 0o600); err != nil {
		return err
	}
	return nil
}

// ExportTraefikTransport atomically writes Traefik's shared bridge transport.
// It contains only certificate paths, so Traefik can read it as a non-secret.
func ExportTraefikTransport(dir string) error {
	if strings.TrimSpace(dir) == "" {
		return errors.New("transport directory is required")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create transport directory: %w", err)
	}
	return writeFile(filepath.Join(dir, "bridge-transport.yml"), []byte(traefikTransport), 0o644)
}

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

// Enrollment is the persisted OPAQUE server state used by development-only
// certificate enrollment. It never stores the shared secret.
type Enrollment struct {
	configuration *opaque.Configuration
	server        *opaque.Server
	record        *opaque.ClientRecord
}

const pairingCodePrefix = "bridge-pair-v1"

// PairingCode contains the secret used by OPAQUE and the expected SHA-256
// digest of the master's enrollment certificate SPKI.
type PairingCode struct {
	Secret  []byte
	SPKIPin []byte
}

// String serializes the code for transport through an environment variable.
func (c PairingCode) String() string {
	return pairingCodePrefix + "." + base64.RawURLEncoding.EncodeToString(c.Secret) + "." + base64.RawURLEncoding.EncodeToString(c.SPKIPin)
}

// ParsePairingCode validates a development pairing code.
func ParsePairingCode(value string) (PairingCode, error) {
	parts := strings.Split(strings.TrimSpace(value), ".")
	if len(parts) != 3 || parts[0] != pairingCodePrefix {
		return PairingCode{}, errors.New("invalid development pairing code")
	}
	secret, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || len(secret) != 32 {
		return PairingCode{}, errors.New("invalid development pairing code secret")
	}
	pin, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(pin) != sha256.Size {
		return PairingCode{}, errors.New("invalid development pairing code SPKI pin")
	}
	return PairingCode{Secret: secret, SPKIPin: pin}, nil
}

// InitializeEnrollment loads or creates the OPAQUE record derived locally
// from secret. The secret is never serialized or sent over the network.
func (a *Authority) InitializeEnrollment(secret []byte) (*Enrollment, error) {
	if len(secret) < 32 {
		return nil, errors.New("development enrollment secret must contain at least 32 bytes")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.state.Enrollment.Configuration) == 0 {
		if err := a.createEnrollment(secret); err != nil {
			return nil, err
		}
	}
	return a.enrollmentLocked()
}

// RotateEnrollment replaces the OPAQUE registration and returns its one-time
// pairing code. Callers must stop using the previous Enrollment before calling it.
func (a *Authority) RotateEnrollment() (*Enrollment, string, error) {
	secret, err := randomBytes(32)
	if err != nil {
		return nil, "", err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.createEnrollment(secret); err != nil {
		return nil, "", err
	}
	enrollment, err := a.enrollmentLocked()
	if err != nil {
		return nil, "", err
	}
	pair := a.MasterServer()
	block, _ := pem.Decode(pair.CertificatePEM)
	if block == nil {
		return nil, "", errors.New("invalid master enrollment certificate")
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, "", fmt.Errorf("parse master enrollment certificate: %w", err)
	}
	pin := sha256.Sum256(certificate.RawSubjectPublicKeyInfo)
	return enrollment, (PairingCode{Secret: secret, SPKIPin: pin[:]}).String(), nil
}

func (a *Authority) enrollmentLocked() (*Enrollment, error) {
	configuration, err := opaque.DeserializeConfiguration(a.state.Enrollment.Configuration)
	if err != nil {
		return nil, fmt.Errorf("decode OPAQUE configuration: %w", err)
	}
	keyMaterial, err := configuration.DecodeServerKeyMaterial(a.state.Enrollment.ServerKeyMaterial)
	if err != nil {
		return nil, fmt.Errorf("decode OPAQUE server key material: %w", err)
	}
	server, err := configuration.Server()
	if err != nil {
		return nil, fmt.Errorf("create OPAQUE server: %w", err)
	}
	if err := server.SetKeyMaterial(keyMaterial); err != nil {
		return nil, fmt.Errorf("set OPAQUE server key material: %w", err)
	}
	record, err := server.Deserialize.RegistrationRecord(a.state.Enrollment.RegistrationRecord)
	if err != nil {
		return nil, fmt.Errorf("decode OPAQUE registration record: %w", err)
	}
	return &Enrollment{configuration: configuration, server: server, record: &opaque.ClientRecord{RegistrationRecord: record, CredentialIdentifier: a.state.Enrollment.CredentialID, ClientIdentity: a.state.Enrollment.ClientIdentity}}, nil
}

func (a *Authority) createEnrollment(secret []byte) error {
	configuration := opaque.DefaultConfiguration()
	server, err := configuration.Server()
	if err != nil {
		return fmt.Errorf("create OPAQUE server: %w", err)
	}
	privateKey, publicKey := configuration.KeyGen()
	keyMaterial := &opaque.ServerKeyMaterial{Identity: []byte("traefik-bridge:master:" + a.state.MasterID), PrivateKey: privateKey, PublicKeyBytes: publicKey.Encode(), OPRFGlobalSeed: configuration.GenerateOPRFSeed()}
	if err := server.SetKeyMaterial(keyMaterial); err != nil {
		return fmt.Errorf("set OPAQUE server key material: %w", err)
	}
	client, err := configuration.Client()
	if err != nil {
		return fmt.Errorf("create OPAQUE client: %w", err)
	}
	request, err := client.RegistrationInit(secret)
	if err != nil {
		return fmt.Errorf("initialize OPAQUE registration: %w", err)
	}
	credentialID, err := randomBytes(32)
	if err != nil {
		return err
	}
	response, err := server.RegistrationResponse(request, credentialID, nil)
	if err != nil {
		return fmt.Errorf("create OPAQUE registration response: %w", err)
	}
	clientID := []byte("traefik-bridge:cluster")
	record, _, err := client.RegistrationFinalize(response, clientID, keyMaterial.Identity)
	if err != nil {
		return fmt.Errorf("finalize OPAQUE registration: %w", err)
	}
	a.state.Enrollment = state.Enrollment{Configuration: configuration.Serialize(), ServerKeyMaterial: keyMaterial.Encode(), RegistrationRecord: record.Serialize(), CredentialID: credentialID, ClientIdentity: clientID}
	if err := a.store.Save(a.state); err != nil {
		return fmt.Errorf("persist OPAQUE enrollment state: %w", err)
	}
	keyMaterial.Flush()
	client.ClearState()
	return nil
}

// BeginEnrollment processes KE1 and returns KE2 plus state required to verify KE3.
func (e *Enrollment) BeginEnrollment(message []byte) ([]byte, *opaque.ServerOutput, error) {
	ke1, err := e.server.Deserialize.KE1(message)
	if err != nil {
		return nil, nil, err
	}
	ke2, output, err := e.server.GenerateKE2(ke1, e.record)
	if err != nil {
		return nil, nil, err
	}
	return ke2.Serialize(), output, nil
}

// FinishEnrollment verifies the client proof.
func (e *Enrollment) FinishEnrollment(message []byte, output *opaque.ServerOutput) error {
	ke3, err := e.server.Deserialize.KE3(message)
	if err != nil {
		return err
	}
	return e.server.LoginFinish(ke3, output.ClientMAC)
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
	routeSigningKey, err := randomBytes(32)
	if err != nil {
		return nil, err
	}
	return &state.State{Version: stateVersion, MasterID: masterID, CA: ca, MasterServer: server, MasterClient: client, RouteSigningKey: routeSigningKey, Slaves: make(map[string]state.Slave)}, nil
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

func randomBytes(length int) ([]byte, error) {
	bytes := make([]byte, length)
	if _, err := rand.Read(bytes); err != nil {
		return nil, err
	}
	return bytes, nil
}

func writePEM(path string, contents []byte, mode os.FileMode) error {
	return writeFile(path, contents, mode)
}

func writeFile(path string, contents []byte, mode os.FileMode) error {
	if len(contents) == 0 {
		return fmt.Errorf("file contents for %q are empty", path)
	}
	if info, err := os.Lstat(path); err == nil && (!info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0) {
		return fmt.Errorf("file path %q is not a regular file", path)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect file path %q: %w", path, err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".bridge-file-*")
	if err != nil {
		return fmt.Errorf("create temporary file: %w", err)
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if err := temporary.Chmod(mode); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("secure temporary file: %w", err)
	}
	if _, err := temporary.Write(contents); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write file: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close file: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("replace file: %w", err)
	}
	return nil
}
