package control

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"

	"github.com/bytemare/opaque"
	controlv1 "github.com/traefik/traefik-bridge/api/gen/go/control/v1"
	bridgecrypto "github.com/traefik/traefik-bridge/internal/crypto"
	"github.com/traefik/traefik-bridge/internal/state"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
)

// EnrollmentServer provides development-only OPAQUE enrollment. It must be
// served with TLS server authentication, but never requires a client cert.
type EnrollmentServer struct {
	controlv1.UnimplementedEnrollmentServiceServer
	authority  *bridgecrypto.Authority
	enrollment *bridgecrypto.Enrollment
	rotate     bool
	onRotate   func(string)
	mu         sync.Mutex
}

// NewPairingEnrollmentServer starts a development pairing session and returns
// the code which a slave must use to bootstrap TLS and OPAQUE.
func NewPairingEnrollmentServer(authority *bridgecrypto.Authority, onRotate func(string)) (*EnrollmentServer, string, error) {
	if authority == nil {
		return nil, "", errors.New("certificate authority is required")
	}
	enrollment, code, err := authority.RotateEnrollment()
	if err != nil {
		return nil, "", err
	}
	return &EnrollmentServer{authority: authority, enrollment: enrollment, rotate: true, onRotate: onRotate}, code, nil
}

func NewEnrollmentServer(authority *bridgecrypto.Authority, secret []byte) (*EnrollmentServer, error) {
	if authority == nil {
		return nil, errors.New("certificate authority is required")
	}
	enrollment, err := authority.InitializeEnrollment(secret)
	if err != nil {
		return nil, err
	}
	return &EnrollmentServer{authority: authority, enrollment: enrollment}, nil
}

func (s *EnrollmentServer) Register(registrar grpc.ServiceRegistrar) {
	controlv1.RegisterEnrollmentServiceServer(registrar, s)
}

// TLSConfig returns server-only TLS for pre-certificate enrollment.
func (s *EnrollmentServer) TLSConfig() (*tls.Config, error) {
	pair := s.authority.MasterServer()
	certificate, err := tls.X509KeyPair(pair.CertificatePEM, pair.PrivateKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("load master enrollment certificate: %w", err)
	}
	return &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS13}, nil
}

func (s *EnrollmentServer) Enroll(stream controlv1.EnrollmentService_EnrollServer) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	if len(first.GetEnrollmentId()) == 0 || first.GetSlaveId() == "" || len(first.GetPakeMessage()) == 0 || first.GetCsrDer() == nil {
		return status.Error(codes.InvalidArgument, "first enrollment message must contain an ID, slave ID, OPAQUE KE1, and CSR")
	}
	if err := validateEnrollmentCSR(first.GetCsrDer()); err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	ke2, output, err := s.enrollment.BeginEnrollment(first.GetPakeMessage())
	if err != nil {
		return status.Error(codes.Unauthenticated, "enrollment authentication failed")
	}
	if err := stream.Send(&controlv1.EnrollmentResponse{EnrollmentId: first.GetEnrollmentId(), PakeMessage: ke2, ServerIdentity: []byte("traefik-bridge:master:" + s.authority.MasterID())}); err != nil {
		return err
	}
	third, err := stream.Recv()
	if err != nil {
		return err
	}
	if string(third.GetEnrollmentId()) != string(first.GetEnrollmentId()) || len(third.GetPakeMessage()) == 0 {
		return status.Error(codes.InvalidArgument, "invalid OPAQUE KE3 enrollment message")
	}
	if err := s.enrollment.FinishEnrollment(third.GetPakeMessage(), output); err != nil {
		return status.Error(codes.Unauthenticated, "enrollment authentication failed")
	}
	slaveID := first.GetSlaveId()
	if s.authority.HasSlave(slaveID) {
		return status.Error(codes.AlreadyExists, "slave already has credentials")
	}
	issued, err := s.authority.IssueSlave(slaveID, first.GetCsrDer())
	if err != nil {
		return status.Error(codes.Internal, "issue slave certificate: "+err.Error())
	}
	if err := stream.Send(&controlv1.EnrollmentResponse{EnrollmentId: first.GetEnrollmentId(), Result: &controlv1.EnrollmentResponse_Accepted{Accepted: &controlv1.EnrollmentAccepted{SlaveId: slaveID, Certificate: &controlv1.CertificateBundle{LeafCertificatePem: issued.CertificatePEM}, MasterCaPem: s.authority.CACertificate()}}}); err != nil {
		return err
	}
	if s.rotate {
		next, code, err := s.authority.RotateEnrollment()
		if err != nil {
			return status.Error(codes.Internal, "rotate development pairing code: "+err.Error())
		}
		s.enrollment = next
		if s.onRotate != nil {
			s.onRotate(code)
		}
	}
	return nil
}

func validateEnrollmentCSR(csrDER []byte) error {
	csr, err := x509.ParseCertificateRequest(csrDER)
	if err != nil {
		return errors.New("invalid slave CSR")
	}
	return csr.CheckSignature()
}

// EnrollmentClientConfig contains bootstrap inputs and target credential paths.
type EnrollmentClientConfig struct {
	Address         string
	ServerName      string
	DataHost        string
	BootstrapCA     []byte
	Secret          []byte
	PairingCode     string
	SlaveID         string
	CertificateFile string
	PrivateKeyFile  string
	CAFile          string
	CSRFile         string
	DialContext     func(context.Context, string) (net.Conn, error)
	pinnedSPKI      []byte
}

// Enroll creates a private key and CSR, mutually authenticates with OPAQUE,
// and atomically persists the issued credentials. Existing files are refused.
func Enroll(ctx context.Context, config EnrollmentClientConfig) (state.KeyPair, []byte, string, error) {
	if config.PairingCode != "" {
		pairing, err := bridgecrypto.ParsePairingCode(config.PairingCode)
		if err != nil {
			return state.KeyPair{}, nil, "", err
		}
		config.Secret = pairing.Secret
		config.BootstrapCA = nil
		config.ServerName = ""
		config.pinnedSPKI = pairing.SPKIPin
	}
	if len(config.Secret) < 32 {
		return state.KeyPair{}, nil, "", errors.New("development enrollment secret must contain at least 32 bytes")
	}
	if err := ensureCredentialsAbsent(config.CertificateFile, config.PrivateKeyFile, config.CAFile, config.CSRFile); err != nil {
		return state.KeyPair{}, nil, "", err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return state.KeyPair{}, nil, "", fmt.Errorf("generate slave key: %w", err)
	}
	request := &x509.CertificateRequest{Subject: pkix.Name{CommonName: config.SlaveID}}
	if ip := net.ParseIP(config.DataHost); ip != nil {
		request.IPAddresses = []net.IP{ip}
	} else if config.DataHost != "" {
		request.DNSNames = []string{config.DataHost}
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, request, key)
	if err != nil {
		return state.KeyPair{}, nil, "", fmt.Errorf("create slave CSR: %w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return state.KeyPair{}, nil, "", err
	}
	configuration := opaque.DefaultConfiguration()
	client, err := configuration.Client()
	if err != nil {
		return state.KeyPair{}, nil, "", fmt.Errorf("create OPAQUE client: %w", err)
	}
	defer client.ClearState()
	ke1, err := client.GenerateKE1(config.Secret)
	if err != nil {
		return state.KeyPair{}, nil, "", fmt.Errorf("create OPAQUE KE1: %w", err)
	}
	id, err := enrollmentID()
	if err != nil {
		return state.KeyPair{}, nil, "", err
	}
	tlsConfig, err := enrollmentTLSConfig(config.BootstrapCA, config.ServerName, config.pinnedSPKI)
	if err != nil {
		return state.KeyPair{}, nil, "", err
	}
	dialOptions := []grpc.DialOption{grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig))}
	if config.DialContext != nil {
		dialOptions = append(dialOptions, grpc.WithContextDialer(config.DialContext))
	}
	connection, err := grpc.NewClient(config.Address, dialOptions...)
	if err != nil {
		return state.KeyPair{}, nil, "", err
	}
	defer func() { _ = connection.Close() }()
	stream, err := controlv1.NewEnrollmentServiceClient(connection).Enroll(ctx)
	if err != nil {
		return state.KeyPair{}, nil, "", err
	}
	if err := stream.Send(&controlv1.EnrollmentRequest{ProtocolVersion: "v1", EnrollmentId: id, SlaveId: config.SlaveID, PakeMessage: ke1.Serialize(), CsrDer: csrDER}); err != nil {
		return state.KeyPair{}, nil, "", err
	}
	response, err := stream.Recv()
	if err != nil {
		return state.KeyPair{}, nil, "", err
	}
	ke2, err := client.Deserialize.KE2(response.GetPakeMessage())
	if err != nil {
		return state.KeyPair{}, nil, "", errors.New("enrollment authentication failed")
	}
	ke3, _, _, err := client.GenerateKE3(ke2, []byte("traefik-bridge:cluster"), response.GetServerIdentity())
	if err != nil {
		return state.KeyPair{}, nil, "", errors.New("enrollment authentication failed")
	}
	if err := stream.Send(&controlv1.EnrollmentRequest{EnrollmentId: id, PakeMessage: ke3.Serialize()}); err != nil {
		return state.KeyPair{}, nil, "", err
	}
	accepted, err := stream.Recv()
	if err != nil {
		return state.KeyPair{}, nil, "", err
	}
	if accepted.GetAccepted() == nil {
		return state.KeyPair{}, nil, "", errors.New("enrollment was rejected")
	}
	pair := state.KeyPair{CertificatePEM: accepted.GetAccepted().GetCertificate().GetLeafCertificatePem(), PrivateKeyPEM: pemEncodePrivateKey(keyDER)}
	ca := accepted.GetAccepted().GetMasterCaPem()
	if err := persistCredentials(config.CertificateFile, config.PrivateKeyFile, config.CAFile, config.CSRFile, pair, ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})); err != nil {
		return state.KeyPair{}, nil, "", err
	}
	return pair, ca, accepted.GetAccepted().GetSlaveId(), nil
}

func enrollmentTLSConfig(ca []byte, serverName string, pinnedSPKI []byte) (*tls.Config, error) {
	if len(pinnedSPKI) != 0 {
		if len(pinnedSPKI) != sha256.Size {
			return nil, errors.New("invalid enrollment SPKI pin")
		}
		return &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS13, VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return errors.New("enrollment server did not provide a certificate")
			}
			certificate, err := x509.ParseCertificate(rawCerts[0])
			if err != nil {
				return fmt.Errorf("parse enrollment server certificate: %w", err)
			}
			actual := sha256.Sum256(certificate.RawSubjectPublicKeyInfo)
			if subtle.ConstantTimeCompare(actual[:], pinnedSPKI) != 1 {
				return errors.New("enrollment server SPKI pin mismatch")
			}
			return nil
		}}, nil
	}
	pool, err := certificatePool(ca)
	if err != nil {
		return nil, fmt.Errorf("load enrollment CA: %w", err)
	}
	return &tls.Config{RootCAs: pool, ServerName: serverName, MinVersion: tls.VersionTLS13}, nil
}

func ensureCredentialsAbsent(paths ...string) error {
	for _, path := range paths {
		if path == "" {
			return errors.New("credential path is required")
		}
		if _, err := os.Lstat(path); err == nil {
			return fmt.Errorf("refusing to overwrite existing credential %q", path)
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect credential %q: %w", path, err)
		}
	}
	return nil
}

func persistCredentials(certificateFile, privateKeyFile, caFile, csrFile string, pair state.KeyPair, ca, csr []byte) (err error) {
	paths := []string{certificateFile, privateKeyFile, caFile, csrFile}
	for _, path := range paths {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return err
		}
		if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
			return err
		}
	}
	written := make([]string, 0, len(paths))
	defer func() {
		if err != nil {
			for _, path := range written {
				_ = os.Remove(path)
			}
		}
	}()
	for _, item := range []struct {
		path string
		data []byte
	}{{certificateFile, pair.CertificatePEM}, {privateKeyFile, pair.PrivateKeyPEM}, {caFile, ca}, {csrFile, csr}} {
		if err := atomicPrivateWrite(item.path, item.data); err != nil {
			return err
		}
		written = append(written, item.path)
	}
	return nil
}

func enrollmentID() ([]byte, error) {
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return nil, err
	}
	return id, nil
}

func pemEncodePrivateKey(keyDER []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}

func atomicPrivateWrite(path string, data []byte) error {
	temporary, err := os.CreateTemp(filepath.Dir(path), ".bridge-credential-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}
