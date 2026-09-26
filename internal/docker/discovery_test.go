package docker

import (
	"context"
	"errors"
	"testing"
	"time"

	controlv1 "github.com/traefik/traefik-bridge/api/gen/go/control/v1"
)

func TestSnapshotSelectsEligibleContainersAndServices(t *testing.T) {
	client := &fakeClient{containers: []Container{
		{ID: "ignored", Name: "/ignored", Labels: map[string]string{traefikEnableLabel: "false"}, Networks: map[string]Network{"apps": {}}, ExposedPorts: []uint32{80}},
		{ID: "wrong-constraint", Name: "/wrong", Labels: map[string]string{traefikEnableLabel: "true", "bridge.zone": "west"}, Networks: map[string]Network{"apps": {}}, ExposedPorts: []uint32{80}},
		{ID: "app-b", Name: "/api", Labels: map[string]string{
			traefikEnableLabel: "TRUE", "bridge.zone": "east", traefikNetworkLabel: "edge",
			"traefik.http.services.api.loadbalancer.server.port":          "8080",
			"traefik.http.services.metrics.loadbalancer.healthcheck.path": "/health",
		}, Networks: map[string]Network{"apps": {}, "edge": {}}, ExposedPorts: []uint32{9000, 80}},
		{ID: "app-a", Name: "/web", Labels: map[string]string{
			traefikEnableLabel: "true", "bridge.zone": "east",
			"traefik.http.routers.web.service": "frontend",
		}, Networks: map[string]Network{"apps": {}}, ExposedPorts: []uint32{443, 80}},
	}}
	publisher := &fakePublisher{}
	discovery, err := NewDiscovery(client, publisher, Config{DefaultNetwork: "apps", ConstraintLabels: map[string]string{"bridge.zone": "east"}})
	if err != nil {
		t.Fatal(err)
	}

	snapshot, err := discovery.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.GetRevision() != 1 || len(snapshot.GetSnapshotId()) == 0 || len(snapshot.GetApplications()) != 2 {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	first, second := snapshot.GetApplications()[0], snapshot.GetApplications()[1]
	if first.GetApplicationId() != "app-a" || first.GetNetwork() != "apps" || first.GetServices()[0].GetServiceId() != "frontend" || first.GetServices()[0].GetTargetPort() != 80 {
		t.Fatalf("first application = %+v", first)
	}
	if second.GetApplicationId() != "app-b" || second.GetContainerName() != "api" || second.GetNetwork() != "edge" {
		t.Fatalf("second application = %+v", second)
	}
	if got := second.GetServices(); len(got) != 2 || got[0].GetServiceId() != "api" || got[0].GetTargetPort() != 8080 || got[1].GetServiceId() != "metrics" || got[1].GetTargetPort() != 80 {
		t.Fatalf("services = %+v", got)
	}

	second.GetLabels()[traefikEnableLabel] = "false"
	if client.containers[2].Labels[traefikEnableLabel] != "TRUE" {
		t.Fatal("snapshot labels alias Docker container labels")
	}
}

func TestSnapshotSkipsContainersWithoutSelectedNetworkOrPort(t *testing.T) {
	client := &fakeClient{containers: []Container{
		{ID: "missing-network", Labels: map[string]string{traefikEnableLabel: "true"}, Networks: map[string]Network{"other": {}}, ExposedPorts: []uint32{80}},
		{ID: "missing-port", Labels: map[string]string{traefikEnableLabel: "true"}, Networks: map[string]Network{"apps": {}}},
		{ID: "invalid-port", Labels: map[string]string{traefikEnableLabel: "true", "traefik.http.services.api.loadbalancer.server.port": "nope"}, Networks: map[string]Network{"apps": {}}, ExposedPorts: []uint32{80}},
	}}
	discovery, err := NewDiscovery(client, &fakePublisher{}, Config{DefaultNetwork: "apps"})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := discovery.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.GetApplications()) != 0 {
		t.Fatalf("applications = %+v, want none", snapshot.GetApplications())
	}
}

func TestSnapshotAcceptsExplicitPortWithoutExposedMetadata(t *testing.T) {
	client := &fakeClient{containers: []Container{{
		ID:       "app",
		Name:     "/app",
		Labels:   map[string]string{traefikEnableLabel: "true", "traefik.http.services.app.loadbalancer.server.port": "8080"},
		Networks: map[string]Network{"apps": {}},
	}}}
	discovery, err := NewDiscovery(client, &fakePublisher{}, Config{DefaultNetwork: "apps"})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := discovery.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := snapshot.GetApplications()[0].GetServices()[0].GetTargetPort(); got != 8080 {
		t.Fatalf("target port = %d, want 8080", got)
	}
}

func TestRunPublishesInitiallyAndAfterRelevantEvents(t *testing.T) {
	events := make(chan Event, 3)
	errs := make(chan error)
	client := &fakeClient{containers: []Container{{ID: "app", Name: "/app", Labels: map[string]string{traefikEnableLabel: "true"}, Networks: map[string]Network{"apps": {}}, ExposedPorts: []uint32{80}}}, events: events, errs: errs}
	publisher := &fakePublisher{published: make(chan *controlv1.FullSnapshot, 3)}
	discovery, err := NewDiscovery(client, publisher, Config{DefaultNetwork: "apps"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- discovery.Run(ctx) }()
	waitSnapshot(t, publisher.published, 1)
	events <- Event{Type: "network", Action: "connect"}
	events <- Event{Type: "container", Action: "update"}
	waitSnapshot(t, publisher.published, 2)
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestRunReturnsEventErrors(t *testing.T) {
	errs := make(chan error, 1)
	errs <- errors.New("socket disconnected")
	client := &fakeClient{containers: []Container{}, events: make(chan Event), errs: errs}
	discovery, err := NewDiscovery(client, &fakePublisher{}, Config{DefaultNetwork: "apps"})
	if err != nil {
		t.Fatal(err)
	}
	if err := discovery.Run(context.Background()); err == nil {
		t.Fatal("Run succeeded after event stream error")
	}
}

type fakeClient struct {
	containers []Container
	events     <-chan Event
	errs       <-chan error
}

func (f *fakeClient) ListContainers(context.Context) ([]Container, error) { return f.containers, nil }
func (f *fakeClient) Events(context.Context) (<-chan Event, <-chan error) { return f.events, f.errs }

type fakePublisher struct {
	published chan *controlv1.FullSnapshot
}

func (f *fakePublisher) PublishSnapshot(_ context.Context, snapshot *controlv1.FullSnapshot) error {
	if f.published != nil {
		f.published <- snapshot
	}
	return nil
}

func waitSnapshot(t *testing.T, snapshots <-chan *controlv1.FullSnapshot, revision uint64) {
	t.Helper()
	select {
	case snapshot := <-snapshots:
		if snapshot.GetRevision() != revision {
			t.Fatalf("revision = %d, want %d", snapshot.GetRevision(), revision)
		}
	case <-time.After(time.Second):
		t.Fatalf("did not receive revision %d", revision)
	}
}
