// Package state persists the master identity and certificates.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const fileName = "master-state.json"

// State is the complete persistent state owned by a bridge master.
type State struct {
	Version      int              `json:"version"`
	MasterID     string           `json:"master_id"`
	CA           KeyPair          `json:"ca"`
	MasterServer KeyPair          `json:"master_server"`
	MasterClient KeyPair          `json:"master_client"`
	Slaves       map[string]Slave `json:"slaves"`
	Enrollment   Enrollment       `json:"enrollment"`
}

// Enrollment holds opaque server material. None of these fields contain the
// shared enrollment secret.
type Enrollment struct {
	Configuration      []byte `json:"configuration"`
	ServerKeyMaterial  []byte `json:"server_key_material"`
	RegistrationRecord []byte `json:"registration_record"`
	CredentialID       []byte `json:"credential_id"`
	ClientIdentity     []byte `json:"client_identity"`
}

// KeyPair stores PEM-encoded certificate and private-key material.
type KeyPair struct {
	CertificatePEM []byte `json:"certificate_pem"`
	PrivateKeyPEM  []byte `json:"private_key_pem"`
}

// Slave records the currently issued certificate for a slave identity.
type Slave struct {
	CertificatePEM []byte    `json:"certificate_pem"`
	Serial         string    `json:"serial"`
	IssuedAt       time.Time `json:"issued_at"`
	NotAfter       time.Time `json:"not_after"`
}

// Store atomically reads and writes State in a private data directory.
type Store struct {
	dir string
	mu  sync.Mutex
}

// NewStore creates a store rooted at dir.
func NewStore(dir string) *Store {
	return &Store{dir: dir}
}

// Load reads the previously saved state.
func (s *Store) Load() (*State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := checkDirectory(s.dir); err != nil {
		return nil, err
	}
	path := filepath.Join(s.dir, fileName)
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("state file %q is not a regular file", path)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("state file %q must not be accessible by group or others", path)
	}

	contents, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var value State
	if err := json.Unmarshal(contents, &value); err != nil {
		return nil, fmt.Errorf("decode master state: %w", err)
	}
	if value.Slaves == nil {
		value.Slaves = make(map[string]Slave)
	}
	return &value, nil
}

// Save atomically replaces the state file, keeping it private to its owner.
func (s *Store) Save(value *State) error {
	if value == nil {
		return errors.New("master state is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	if err := os.Chmod(s.dir, 0o700); err != nil {
		return fmt.Errorf("secure state directory: %w", err)
	}

	contents, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode master state: %w", err)
	}
	contents = append(contents, '\n')

	temporary, err := os.CreateTemp(s.dir, ".master-state-*")
	if err != nil {
		return fmt.Errorf("create temporary state file: %w", err)
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("secure temporary state file: %w", err)
	}
	if _, err := temporary.Write(contents); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write master state: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync master state: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close master state: %w", err)
	}
	if err := os.Rename(temporaryPath, filepath.Join(s.dir, fileName)); err != nil {
		return fmt.Errorf("replace master state: %w", err)
	}

	directory, err := os.Open(s.dir)
	if err != nil {
		return fmt.Errorf("open state directory: %w", err)
	}
	defer func() { _ = directory.Close() }()
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("sync state directory: %w", err)
	}
	return nil
}

func checkDirectory(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("state directory %q is not a directory", path)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("state directory %q must not be accessible by group or others", path)
	}
	return nil
}

// IsNotExist reports whether an error means no state has yet been saved.
func IsNotExist(err error) bool {
	return errors.Is(err, fs.ErrNotExist)
}
