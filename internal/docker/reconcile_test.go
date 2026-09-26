package docker

import (
	"context"
	"testing"

	controlv1 "github.com/traefik/traefik-bridge/api/gen/go/control/v1"
	"github.com/traefik/traefik-bridge/internal/proxy"
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

func newReconciler(t *testing.T, client *fakeReconcileClient, start, end uint32) *Reconciler {
	t.Helper()
	signer, err := proxy.NewSigner([]byte("test-key"))
	if err != nil {
		t.Fatal(err)
	}
	result, err := NewReconciler(client, ReconcileConfig{MasterID: "master", ProxyImage: "proxy:test", PortStart: start, PortEnd: end, Signer: signer, CertificateMount: Mount{Source: "certs", Target: "/certs"}})
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
	containers       []ManagedContainer
	specs            []ContainerSpec
	creates, removes int
}

func (f *fakeReconcileClient) ListManaged(context.Context) ([]ManagedContainer, error) {
	return append([]ManagedContainer(nil), f.containers...), nil
}
func (f *fakeReconcileClient) CreateManaged(_ context.Context, spec ContainerSpec) error {
	f.creates++
	f.specs = append(f.specs, spec)
	f.containers = append(f.containers, ManagedContainer{ID: spec.Name, Name: spec.Name, Labels: spec.Labels})
	return nil
}
func (f *fakeReconcileClient) RemoveManaged(_ context.Context, id string) error {
	f.removes++
	for i, container := range f.containers {
		if container.ID == id {
			f.containers = append(f.containers[:i], f.containers[i+1:]...)
			break
		}
	}
	return nil
}
