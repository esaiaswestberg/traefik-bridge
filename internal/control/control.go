// Package control implements the mutually authenticated bridge control stream.
package control

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	controlv1 "github.com/traefik/traefik-bridge/api/gen/go/control/v1"
	"github.com/traefik/traefik-bridge/internal/crypto"
	"github.com/traefik/traefik-bridge/internal/observability"
	"github.com/traefik/traefik-bridge/internal/state"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const defaultDisconnectGrace = 2 * time.Minute

// EndpointAllowlist limits endpoints slaves may advertise. DNS entries are
// exact, case-insensitive host names; CIDRs apply only to literal IP hosts.
type EndpointAllowlist struct {
	CIDRs    []string
	DNSNames []string
}

type allowlist struct {
	cidrs map[string]*net.IPNet
	dns   map[string]struct{}
}

func newAllowlist(config EndpointAllowlist) (allowlist, error) {
	value := allowlist{cidrs: make(map[string]*net.IPNet), dns: make(map[string]struct{})}
	for _, raw := range config.CIDRs {
		_, network, err := net.ParseCIDR(raw)
		if err != nil {
			return allowlist{}, fmt.Errorf("parse endpoint CIDR %q: %w", raw, err)
		}
		value.cidrs[network.String()] = network
	}
	for _, raw := range config.DNSNames {
		host := normalizeHost(raw)
		if !validDNSName(host) {
			return allowlist{}, fmt.Errorf("invalid endpoint DNS name %q", raw)
		}
		value.dns[host] = struct{}{}
	}
	return value, nil
}

func (a allowlist) validate(endpoint *controlv1.SlaveEndpoint) error {
	if endpoint == nil || endpoint.GetPort() == 0 || endpoint.GetPort() > 65535 || endpoint.GetHost() == "" {
		return errors.New("endpoint host and port are required")
	}
	host := normalizeHost(endpoint.GetHost())
	if ip := net.ParseIP(host); ip != nil {
		for _, network := range a.cidrs {
			if network.Contains(ip) {
				return nil
			}
		}
		return fmt.Errorf("endpoint IP %q is not allowed", host)
	}
	if _, ok := a.dns[host]; ok {
		return nil
	}
	return fmt.Errorf("endpoint DNS name %q is not allowed", host)
}

func normalizeHost(host string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
}

func validDNSName(host string) bool {
	if host == "" || len(host) > 253 || net.ParseIP(host) != nil {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, char := range label {
			if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '-' {
				return false
			}
		}
	}
	return true
}

// ServerConfig configures one control-server process.
type ServerConfig struct {
	EndpointAllowlist EndpointAllowlist
	DisconnectGrace   time.Duration
	SnapshotHandler   SnapshotHandler
	Metrics           observability.Recorder
}

// SnapshotHandler applies a full snapshot after its control-stream identity
// has been authenticated. It may reject individual resources while accepting
// the valid remainder.
type SnapshotHandler interface {
	Apply(context.Context, string, *controlv1.FullSnapshot) ([]*controlv1.ResourceRejection, error)
}

// SessionStatus is the observed state of one slave control stream.
type SessionStatus struct {
	Connected      bool
	Available      bool
	LastHeartbeat  time.Time
	LastDisconnect time.Time
	Revision       uint64
	Endpoint       *controlv1.SlaveEndpoint
}

type session struct {
	SessionStatus
	connection uint64
}

// Server implements the generated persistent control service.
type Server struct {
	controlv1.UnimplementedControlServiceServer
	authority *crypto.Authority
	allowlist allowlist
	instance  string
	grace     time.Duration
	handler   SnapshotHandler
	metrics   observability.Recorder
	mu        sync.Mutex
	sessions  map[string]session
	nextID    uint64
}

// NewServer creates a control server. An empty allowlist rejects all endpoints.
func NewServer(authority *crypto.Authority, config ServerConfig) (*Server, error) {
	if authority == nil {
		return nil, errors.New("certificate authority is required")
	}
	allowed, err := newAllowlist(config.EndpointAllowlist)
	if err != nil {
		return nil, err
	}
	grace := config.DisconnectGrace
	if grace == 0 {
		grace = defaultDisconnectGrace
	}
	if grace < 0 {
		return nil, errors.New("disconnect grace must not be negative")
	}
	instance, err := randomID()
	if err != nil {
		return nil, err
	}
	return &Server{authority: authority, allowlist: allowed, instance: instance, grace: grace, handler: config.SnapshotHandler, metrics: config.Metrics, sessions: make(map[string]session)}, nil
}

// Register registers the control service on registrar.
func (s *Server) Register(registrar grpc.ServiceRegistrar) {
	controlv1.RegisterControlServiceServer(registrar, s)
}

// TLSConfig returns the server mTLS configuration for the control listener.
func (s *Server) TLSConfig() (*tls.Config, error) {
	certificate, err := tls.X509KeyPair(s.authority.MasterServer().CertificatePEM, s.authority.MasterServer().PrivateKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("load master server certificate: %w", err)
	}
	pool, err := certificatePool(s.authority.CACertificate())
	if err != nil {
		return nil, err
	}
	return &tls.Config{Certificates: []tls.Certificate{certificate}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: pool, MinVersion: tls.VersionTLS13}, nil
}

// Status returns current stream and grace-period state for slaveID.
func (s *Server) Status(slaveID string) (SessionStatus, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	stored, ok := s.sessions[slaveID]
	if !ok {
		return SessionStatus{}, false
	}
	result := stored.SessionStatus
	result.Available = result.Connected || (!result.LastDisconnect.IsZero() && time.Since(result.LastDisconnect) < s.grace)
	if result.Endpoint != nil {
		result.Endpoint = &controlv1.SlaveEndpoint{Host: result.Endpoint.Host, Port: result.Endpoint.Port}
	}
	return result, true
}

// Connect authenticates a slave and manages its persistent stream.
func (s *Server) Connect(stream controlv1.ControlService_ConnectServer) error {
	certificate, err := peerCertificate(stream.Context())
	if err != nil {
		return status.Error(codes.Unauthenticated, err.Error())
	}
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	hello := first.GetHello()
	if hello == nil || hello.GetSlaveId() == "" {
		return status.Error(codes.InvalidArgument, "first control message must be a hello with a slave ID")
	}
	if !s.authority.MatchesSlave(hello.GetSlaveId(), certificate) {
		return status.Error(codes.PermissionDenied, "TLS certificate does not match claimed slave identity")
	}
	if err := s.allowlist.validate(hello.GetEndpoint()); err != nil {
		return status.Error(codes.PermissionDenied, err.Error())
	}

	snapshotRequired := hello.GetMasterInstanceIdSeen() != s.instance
	connectionID := s.connected(hello.GetSlaveId(), hello.GetEndpoint(), hello.GetLastAcceptedSnapshotRevision())
	defer s.disconnected(hello.GetSlaveId(), connectionID)
	if err := stream.Send(&controlv1.MasterToSlave{Payload: &controlv1.MasterToSlave_Accepted{Accepted: &controlv1.ControlAccepted{MasterInstanceId: s.instance, ServerTime: timestamppb.Now(), SnapshotRequired: snapshotRequired}}}); err != nil {
		return err
	}
	if snapshotRequired {
		if err := stream.Send(&controlv1.MasterToSlave{Payload: &controlv1.MasterToSlave_ResyncRequest{ResyncRequest: &controlv1.ResyncRequest{MasterInstanceId: s.instance, Reason: controlv1.ResyncReason_RESYNC_REASON_MASTER_RESTART, LastAcceptedSnapshotRevision: hello.GetLastAcceptedSnapshotRevision()}}}); err != nil {
			return err
		}
	}
	for {
		message, err := stream.Recv()
		if err != nil {
			return err
		}
		switch payload := message.Payload.(type) {
		case *controlv1.SlaveToMaster_Heartbeat:
			if err := s.allowlist.validate(payload.Heartbeat.GetEndpoint()); err != nil {
				return status.Error(codes.PermissionDenied, err.Error())
			}
			s.heartbeat(hello.GetSlaveId(), payload.Heartbeat.GetEndpoint(), payload.Heartbeat.GetLastAcceptedSnapshotRevision())
		case *controlv1.SlaveToMaster_Snapshot:
			snapshot := payload.Snapshot
			if snapshot == nil {
				return status.Error(codes.InvalidArgument, "snapshot is required")
			}
			var rejections []*controlv1.ResourceRejection
			if s.handler != nil {
				var applyErr error
				rejections, applyErr = s.handler.Apply(stream.Context(), hello.GetSlaveId(), snapshot)
				if applyErr != nil {
					if s.metrics != nil {
						s.metrics.Snapshot(false)
					}
					return status.Error(codes.Internal, fmt.Sprintf("apply snapshot: %v", applyErr))
				}
			}
			s.acceptSnapshot(hello.GetSlaveId(), snapshot.GetRevision())
			if len(rejections) > 0 {
				if s.metrics != nil {
					s.metrics.Snapshot(false)
				}
				if err := stream.Send(&controlv1.MasterToSlave{Payload: &controlv1.MasterToSlave_SnapshotRejected{SnapshotRejected: &controlv1.SnapshotRejected{Revision: snapshot.GetRevision(), SnapshotId: snapshot.GetSnapshotId(), Rejections: rejections}}}); err != nil {
					return err
				}
				continue
			}
			if s.metrics != nil {
				s.metrics.Snapshot(true)
			}
			if err := stream.Send(&controlv1.MasterToSlave{Payload: &controlv1.MasterToSlave_SnapshotAccepted{SnapshotAccepted: &controlv1.SnapshotAccepted{Revision: snapshot.GetRevision(), SnapshotId: snapshot.GetSnapshotId()}}}); err != nil {
				return err
			}
		}
	}
}

func (s *Server) connected(id string, endpoint *controlv1.SlaveEndpoint, revision uint64) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	wasConnected := s.sessions[id].Connected
	s.sessions[id] = session{SessionStatus: SessionStatus{Connected: true, LastHeartbeat: time.Now(), Revision: revision, Endpoint: cloneEndpoint(endpoint)}, connection: s.nextID}
	if !wasConnected && s.metrics != nil {
		s.metrics.Connection(true)
	}
	return s.nextID
}

func (s *Server) disconnected(id string, connectionID uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	current := s.sessions[id]
	if current.connection != connectionID {
		return
	}
	current.Connected = false
	current.LastDisconnect = time.Now()
	s.sessions[id] = current
	if s.metrics != nil {
		s.metrics.Connection(false)
	}
}

func (s *Server) heartbeat(id string, endpoint *controlv1.SlaveEndpoint, revision uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	current := s.sessions[id]
	current.LastHeartbeat = time.Now()
	current.Revision = revision
	current.Endpoint = cloneEndpoint(endpoint)
	s.sessions[id] = current
}

func (s *Server) acceptSnapshot(id string, revision uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	current := s.sessions[id]
	current.Revision = revision
	s.sessions[id] = current
}

func cloneEndpoint(endpoint *controlv1.SlaveEndpoint) *controlv1.SlaveEndpoint {
	if endpoint == nil {
		return nil
	}
	return &controlv1.SlaveEndpoint{Host: endpoint.Host, Port: endpoint.Port}
}

func peerCertificate(ctx context.Context) (*x509.Certificate, error) {
	info, ok := peer.FromContext(ctx)
	if !ok {
		return nil, errors.New("missing peer information")
	}
	tlsInfo, ok := info.AuthInfo.(credentials.TLSInfo)
	if !ok || len(tlsInfo.State.PeerCertificates) == 0 {
		return nil, errors.New("mTLS client certificate is required")
	}
	return tlsInfo.State.PeerCertificates[0], nil
}

func certificatePool(certificatePEM []byte) (*x509.CertPool, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(certificatePEM) {
		return nil, errors.New("invalid CA certificate")
	}
	return pool, nil
}

func randomID() (string, error) {
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes), nil
}

// ClientConfig configures a reconnecting slave control client.
type ClientConfig struct {
	SlaveID           string
	Endpoint          *controlv1.SlaveEndpoint
	HeartbeatInterval time.Duration
	ReconnectDelay    time.Duration
	Snapshot          func(context.Context) (*controlv1.FullSnapshot, error)
	Dial              func(context.Context) (*grpc.ClientConn, error)
}

// Client maintains a control stream until its context is cancelled.
type Client struct {
	config   ClientConfig
	instance string
	revision uint64
}

// NewClient validates client configuration. Dial is injected so callers can
// choose their network transport without coupling this package to CLI config.
func NewClient(config ClientConfig) (*Client, error) {
	if config.SlaveID == "" || config.Endpoint == nil || config.Dial == nil {
		return nil, errors.New("slave ID, endpoint, and dial function are required")
	}
	if config.HeartbeatInterval == 0 {
		config.HeartbeatInterval = 15 * time.Second
	}
	if config.ReconnectDelay == 0 {
		config.ReconnectDelay = time.Second
	}
	return &Client{config: config}, nil
}

// TLSConfig returns an mTLS client configuration for a master control listener.
func TLSConfig(pair state.KeyPair, ca []byte, serverName string) (*tls.Config, error) {
	certificate, err := tls.X509KeyPair(pair.CertificatePEM, pair.PrivateKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("load slave certificate: %w", err)
	}
	pool, err := certificatePool(ca)
	if err != nil {
		return nil, err
	}
	return &tls.Config{Certificates: []tls.Certificate{certificate}, RootCAs: pool, ServerName: serverName, MinVersion: tls.VersionTLS13}, nil
}

// Run reconnects after transport failures and resends a full snapshot whenever
// the master process reports that it needs one.
func (c *Client) Run(ctx context.Context) error {
	for {
		connection, err := c.config.Dial(ctx)
		if err == nil {
			_ = c.runStream(ctx, connection)
			_ = connection.Close()
		}
		if ctx.Err() != nil {
			return nil
		}
		timer := time.NewTimer(c.config.ReconnectDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}

func (c *Client) runStream(ctx context.Context, connection *grpc.ClientConn) error {
	stream, err := controlv1.NewControlServiceClient(connection).Connect(ctx)
	if err != nil {
		return err
	}
	if err := stream.Send(&controlv1.SlaveToMaster{Payload: &controlv1.SlaveToMaster_Hello{Hello: &controlv1.ControlHello{SlaveId: c.config.SlaveID, MasterInstanceIdSeen: c.instance, LastAcceptedSnapshotRevision: c.revision, Endpoint: cloneEndpoint(c.config.Endpoint)}}}); err != nil {
		return err
	}
	received := make(chan *controlv1.MasterToSlave, 1)
	errs := make(chan error, 1)
	go func() {
		for {
			message, recvErr := stream.Recv()
			if recvErr != nil {
				errs <- recvErr
				return
			}
			received <- message
		}
	}()
	ticker := time.NewTicker(c.config.HeartbeatInterval)
	defer ticker.Stop()
	var sequence uint64
	resynced := false
	for {
		select {
		case err := <-errs:
			return err
		case message := <-received:
			if accepted := message.GetAccepted(); accepted != nil {
				c.instance = accepted.GetMasterInstanceId()
				if accepted.GetSnapshotRequired() && !resynced {
					if err := c.sendSnapshot(ctx, stream); err != nil {
						return err
					}
					resynced = true
				}
			}
			if message.GetResyncRequest() != nil && !resynced {
				if err := c.sendSnapshot(ctx, stream); err != nil {
					return err
				}
				resynced = true
			}
			if accepted := message.GetSnapshotAccepted(); accepted != nil {
				c.revision = accepted.GetRevision()
			}
		case <-ticker.C:
			sequence++
			if err := stream.Send(&controlv1.SlaveToMaster{Payload: &controlv1.SlaveToMaster_Heartbeat{Heartbeat: &controlv1.Heartbeat{Sequence: sequence, SentAt: timestamppb.Now(), LastAcceptedSnapshotRevision: c.revision, Endpoint: cloneEndpoint(c.config.Endpoint)}}}); err != nil {
				return err
			}
		}
	}
}

func (c *Client) sendSnapshot(ctx context.Context, stream controlv1.ControlService_ConnectClient) error {
	if c.config.Snapshot == nil {
		return errors.New("master requested a snapshot but no snapshot provider is configured")
	}
	snapshot, err := c.config.Snapshot(ctx)
	if err != nil {
		return err
	}
	if snapshot == nil {
		return errors.New("snapshot provider returned nil")
	}
	if err := stream.Send(&controlv1.SlaveToMaster{Payload: &controlv1.SlaveToMaster_Snapshot{Snapshot: snapshot}}); err != nil {
		return err
	}
	return stream.Send(&controlv1.SlaveToMaster{Payload: &controlv1.SlaveToMaster_ResyncComplete{ResyncComplete: &controlv1.ResyncComplete{Revision: snapshot.GetRevision(), SnapshotId: snapshot.GetSnapshotId()}}})
}

var _ controlv1.ControlServiceServer = (*Server)(nil)
