package docker

import (
	"context"
	"errors"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/network"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestAdapterListsDiscoveryAndManagedContainers(t *testing.T) {
	client := &fakeAPIClient{containers: []container.Summary{{
		ID: "container-id", Names: []string{"/api"}, Labels: map[string]string{"test": "value", ownerLabel: "true"},
		Ports: []container.Port{{PrivatePort: 8080}}, NetworkSettings: &container.NetworkSettingsSummary{Networks: map[string]*network.EndpointSettings{"apps": {}}},
	}}}
	adapter, err := NewAdapterWithClient(client)
	if err != nil {
		t.Fatal(err)
	}
	discovered, err := adapter.ListContainers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(discovered) != 1 || discovered[0].Name != "/api" || discovered[0].ExposedPorts[0] != 8080 {
		t.Fatalf("containers = %#v", discovered)
	}
	discovered[0].Labels["test"] = "changed"
	if client.containers[0].Labels["test"] != "value" {
		t.Fatal("adapter returned Docker label map without copying it")
	}
	managed, err := adapter.ListManaged(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(managed) != 1 || managed[0].ID != "container-id" {
		t.Fatalf("managed = %#v", managed)
	}
}

func TestAdapterCreatesOnRequestedNetworkAndRemovesFailedStart(t *testing.T) {
	client := &fakeAPIClient{createResponse: container.CreateResponse{ID: "created"}, startErr: errors.New("start failed")}
	adapter, err := NewAdapterWithClient(client)
	if err != nil {
		t.Fatal(err)
	}
	err = adapter.CreateManaged(context.Background(), ContainerSpec{Name: "proxy", Image: "proxy:test", Network: "edge", Labels: map[string]string{"label": "value"}, Env: []string{"A=B"}})
	if err == nil {
		t.Fatal("CreateManaged succeeded after start failure")
	}
	if client.createdConfig == nil || client.createdConfig.Image != "proxy:test" || client.createdNetwork.EndpointsConfig["edge"] == nil {
		t.Fatalf("creation = config=%#v network=%#v", client.createdConfig, client.createdNetwork)
	}
	if client.removedID != "created" {
		t.Fatalf("removed ID = %q, want created", client.removedID)
	}
}

type fakeAPIClient struct {
	containers     []container.Summary
	createResponse container.CreateResponse
	startErr       error
	createdConfig  *container.Config
	createdNetwork *network.NetworkingConfig
	removedID      string
}

func (f *fakeAPIClient) ContainerList(context.Context, container.ListOptions) ([]container.Summary, error) {
	return f.containers, nil
}
func (f *fakeAPIClient) Events(context.Context, events.ListOptions) (<-chan events.Message, <-chan error) {
	return make(chan events.Message), make(chan error)
}
func (f *fakeAPIClient) ContainerCreate(_ context.Context, config *container.Config, _ *container.HostConfig, networking *network.NetworkingConfig, _ *ocispec.Platform, _ string) (container.CreateResponse, error) {
	f.createdConfig, f.createdNetwork = config, networking
	return f.createResponse, nil
}
func (f *fakeAPIClient) ContainerStart(context.Context, string, container.StartOptions) error {
	return f.startErr
}
func (f *fakeAPIClient) ContainerRemove(_ context.Context, id string, _ container.RemoveOptions) error {
	f.removedID = id
	return nil
}
