package docker

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	controlv1 "github.com/traefik/traefik-bridge/api/gen/go/control/v1"
	"github.com/traefik/traefik-bridge/internal/proxy"
	"github.com/traefik/traefik-bridge/internal/state"
)

func TestReconcileCreatesReplacesAndCleansStaleContainers(t *testing.T) {
	client := &fakeReconcileClient{}
	reconciler := newReconciler(t, client, 21000, 21010)
	if rejected, err := reconciler.Apply(context.Background(), "slave-a", endpoint(), snapshot(appSnapshot("app", "web", "api"))); err != nil || len(rejected) != 0 {
		t.Fatalf("apply = %v, %v", rejected, err)
	}
	if len(client.containers) != 1 {
		t.Fatalf("containers = %+v", client.containers)
	}
	created := client.specs[0]
	if created.Labels[ownerLabel] != "true" || created.Labels[masterLabel] != "master" || created.Labels["traefik.http.services.api.loadbalancer.server.port"] == "" {
		t.Fatalf("labels = %#v", created.Labels)
	}
	if !contains(created.Env, "BRIDGE_UPSTREAM=https://slave.example:8444") || !containsPrefix(created.Env, "BRIDGE_ROUTE_TOKENS=api=") || !contains(created.Env, "BRIDGE_CA_FILE=/certs/ca.crt") || len(created.Mounts) != 1 || created.Mounts[0] != (Mount{Source: "certs", Target: "/certs"}) {
		t.Fatalf("proxy spec = %#v", created)
	}
	if _, err := reconciler.Apply(context.Background(), "slave-a", endpoint(), snapshot(appSnapshot("app", "web", "api"))); err != nil {
		t.Fatal(err)
	}
	if client.creates != 1 {
		t.Fatalf("creates = %d, want no replacement", client.creates)
	}
	if _, err := reconciler.Apply(context.Background(), "slave-a", endpoint(), snapshot()); err != nil {
		t.Fatal(err)
	}
	if len(client.containers) != 0 || client.removes != 1 {
		t.Fatalf("containers=%+v removes=%d", client.containers, client.removes)
	}
}

func TestReconcileRejectsConflictWithoutWithdrawingOtherApplications(t *testing.T) {
	client := &fakeReconcileClient{}
	reconciler := newReconciler(t, client, 21000, 21010)
	if _, err := reconciler.Apply(context.Background(), "slave-a", endpoint(), snapshot(appSnapshot("a", "shared", "api"))); err != nil {
		t.Fatal(err)
	}
	rejected, err := reconciler.Apply(context.Background(), "slave-b", endpoint(), snapshot(appSnapshot("b", "shared", "api"), appSnapshot("valid", "other", "other-api")))
	if err != nil || len(rejected) != 1 || len(client.containers) != 2 {
		t.Fatalf("rejected=%+v err=%v containers=%+v", rejected, err, client.containers)
	}
}

func TestReconcileKeepsUnresyncedSlaveContainers(t *testing.T) {
	client := &fakeReconcileClient{containers: []ManagedContainer{{ID: "old", Name: "old", Labels: map[string]string{ownerLabel: "true", masterLabel: "master", slaveLabel: "unresynced"}}}}
	reconciler := newReconciler(t, client, 21000, 21010)
	if _, err := reconciler.Apply(context.Background(), "slave-a", endpoint(), snapshot(appSnapshot("app", "web", "api"))); err != nil {
		t.Fatal(err)
	}
	if len(client.containers) != 2 {
		t.Fatalf("containers = %+v", client.containers)
	}
}

func TestPortAllocationIsStableAndResolvesCollisions(t *testing.T) {
	client := &fakeReconcileClient{}
	reconciler := newReconciler(t, client, 22000, 22001)
	_, err := reconciler.Apply(context.Background(), "slave", endpoint(), snapshot(appSnapshot("a", "a", "one"), appSnapshot("b", "b", "two")))
	if err != nil {
		t.Fatal(err)
	}
	ports := map[string]string{}
	for _, container := range client.containers {
		for key, value := range container.Labels {
			if len(key) > 0 && value != "" {
				if key == "traefik.http.services.one.loadbalancer.server.port" || key == "traefik.http.services.two.loadbalancer.server.port" {
					ports[key] = value
				}
			}
		}
	}
	if len(ports) != 2 || ports["traefik.http.services.one.loadbalancer.server.port"] == ports["traefik.http.services.two.loadbalancer.server.port"] {
		t.Fatalf("ports = %#v", ports)
	}
}

func TestRefreshReplacesOnlyDueProxies(t *testing.T) {
	client := &fakeReconcileClient{}
	reconciler := newReconciler(t, client, 21000, 21010)
	if _, err := reconciler.Apply(t.Context(), "slave-a", endpoint(), snapshot(appSnapshot("due", "due", "api"), appSnapshot("later", "later", "metrics"))); err != nil {
		t.Fatal(err)
	}
	reconciler.expiries[identity("slave-a", "due")] = time.Now().Add(10 * time.Second)
	if err := reconciler.refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if client.creates != 3 || client.removes != 1 {
		t.Fatalf("refresh creates=%d removes=%d, want one replacement", client.creates, client.removes)
	}
}

func TestRunRefreshesSnapshotAppliedAfterEmptyStart(t *testing.T) {
	client := &fakeReconcileClient{listed: make(chan struct{}, 1)}
	reconciler := newReconcilerWithTiming(t, client, 21000, 21010, 100*time.Millisecond, 80*time.Millisecond)
	ctx, cancel := context.WithCancel(t.Context())
	run := make(chan error, 1)
	go func() { run <- reconciler.Run(ctx) }()

	select {
	case <-client.listed:
	case <-time.After(time.Second):
		t.Fatal("Run did not reconcile empty state")
	}
	if _, err := reconciler.Apply(ctx, "slave-a", endpoint(), snapshot(appSnapshot("app", "web", "api"))); err != nil {
		t.Fatal(err)
	}

	deadline := time.After(time.Second)
	for client.createCount() < 2 {
		select {
		case <-deadline:
			t.Fatalf("creates = %d, want refresh without waiting for the idle timer", client.createCount())
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	if err := <-run; err != nil {
		t.Fatalf("Run() error = %v", err)
	}
}

func TestRestoreBeforeAndAfterRefreshDeadline(t *testing.T) {
	store, client := &fakeStateStore{}, &fakeReconcileClient{}
	first := newPersistentReconciler(t, client, store)
	if _, err := first.Apply(t.Context(), "slave-a", endpoint(), snapshot(appSnapshot("app", "web", "api"))); err != nil {
		t.Fatal(err)
	}
	persisted, err := json.Marshal(store.value)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range client.specs[0].Env {
		if token, found := strings.CutPrefix(value, "BRIDGE_ROUTE_TOKENS="); found && strings.Contains(string(persisted), token) {
			t.Fatal("persisted reconciler state contains a route token")
		}
	}
	before := newPersistentReconciler(t, client, store)
	if err := before.Restore(); err != nil {
		t.Fatal(err)
	}
	if _, err := before.Apply(t.Context(), "slave-a", endpoint(), snapshot(appSnapshot("app", "web", "api"))); err != nil {
		t.Fatal(err)
	}
	if client.creates != 1 || client.removes != 0 {
		t.Fatalf("restore before deadline replaced proxy: creates=%d removes=%d", client.creates, client.removes)
	}
	store.value.Slaves["slave-a"] = state.ReconcilerSlave{EndpointHost: "slave.example", EndpointPort: 8444, Snapshot: store.value.Slaves["slave-a"].Snapshot, Expiries: map[string]time.Time{identity("slave-a", "app"): time.Now().Add(-time.Second)}}
	after := newPersistentReconciler(t, client, store)
	if err := after.Restore(); err != nil {
		t.Fatal(err)
	}
	if err := after.refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if client.creates != 2 || client.removes != 1 {
		t.Fatalf("restore after deadline did not refresh proxy: creates=%d removes=%d", client.creates, client.removes)
	}
}

func TestPersistenceFailurePreventsDockerMutation(t *testing.T) {
	client := &fakeReconcileClient{}
	reconciler := newPersistentReconciler(t, client, &fakeStateStore{err: errors.New("disk full")})
	if _, err := reconciler.Apply(t.Context(), "slave-a", endpoint(), snapshot(appSnapshot("app", "web", "api"))); err == nil {
		t.Fatal("Apply succeeded despite persistence failure")
	}
	if client.creates != 0 || client.removes != 0 || client.lists != 0 {
		t.Fatalf("persistence failure reached Docker: %+v", client)
	}
}

func newReconciler(t *testing.T, client *fakeReconcileClient, start, end uint32) *Reconciler {
	return newReconcilerWithTiming(t, client, start, end, 0, 0)
}

func newReconcilerWithTiming(t *testing.T, client *fakeReconcileClient, start, end uint32, lifetime, refreshBefore time.Duration) *Reconciler {
	t.Helper()
	signer, err := proxy.NewSigner([]byte("test-key"))
	if err != nil {
		t.Fatal(err)
	}
	result, err := NewReconciler(client, ReconcileConfig{MasterID: "master", ProxyImage: "proxy:test", PortStart: start, PortEnd: end, Signer: signer, CertificateMount: Mount{Source: "certs", Target: "/certs"}, RouteTokenLifetime: lifetime, RouteTokenRefreshBefore: refreshBefore})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func newPersistentReconciler(t *testing.T, client *fakeReconcileClient, store ReconcilerStateStore) *Reconciler {
	t.Helper()
	signer, err := proxy.NewSigner([]byte("test-key"))
	if err != nil {
		t.Fatal(err)
	}
	result, err := NewReconciler(client, ReconcileConfig{MasterID: "master", ProxyImage: "proxy:test", PortStart: 21000, PortEnd: 21010, Signer: signer, CertificateMount: Mount{Source: "certs", Target: "/certs"}, StateStore: store})
	if err != nil {
		t.Fatal(err)
	}
	return result
}
func endpoint() *controlv1.SlaveEndpoint {
	return &controlv1.SlaveEndpoint{Host: "slave.example", Port: 8444}
}
func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
func containsPrefix(values []string, prefix string) bool {
	for _, value := range values {
		if len(value) >= len(prefix) && value[:len(prefix)] == prefix {
			return true
		}
	}
	return false
}
func snapshot(applications ...*controlv1.ApplicationSnapshot) *controlv1.FullSnapshot {
	return &controlv1.FullSnapshot{Applications: applications}
}
func appSnapshot(id, router, service string) *controlv1.ApplicationSnapshot {
	return &controlv1.ApplicationSnapshot{ApplicationId: id, Network: "edge", Labels: map[string]string{"traefik.http.routers." + router + ".service": service}, Services: []*controlv1.ExportedService{{ServiceId: service, TargetPort: 8080}}}
}

type fakeReconcileClient struct {
	mu                      sync.Mutex
	containers              []ManagedContainer
	specs                   []ContainerSpec
	creates, removes, lists int
	listed                  chan struct{}
}

func (f *fakeReconcileClient) ListManaged(context.Context) ([]ManagedContainer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lists++
	if f.listed != nil {
		select {
		case f.listed <- struct{}{}:
		default:
		}
	}
	return append([]ManagedContainer(nil), f.containers...), nil
}

func (f *fakeReconcileClient) createCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.creates
}

type fakeStateStore struct {
	value *state.ReconcilerState
	err   error
}

func (s *fakeStateStore) Load() (*state.ReconcilerState, error) {
	if s.err != nil {
		return nil, s.err
	}
	if s.value == nil {
		return nil, os.ErrNotExist
	}
	return s.value, nil
}
func (s *fakeStateStore) Save(value *state.ReconcilerState) error {
	if s.err != nil {
		return s.err
	}
	s.value = value
	return nil
}
func (f *fakeReconcileClient) CreateManaged(_ context.Context, spec ContainerSpec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.creates++
	f.specs = append(f.specs, spec)
	f.containers = append(f.containers, ManagedContainer{ID: spec.Name, Name: spec.Name, Labels: spec.Labels})
	return nil
}
func (f *fakeReconcileClient) RemoveManaged(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removes++
	for i, container := range f.containers {
		if container.ID == id {
			f.containers = append(f.containers[:i], f.containers[i+1:]...)
			break
		}
	}
	return nil
}
