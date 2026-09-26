package docker

import (
	"context"
	"fmt"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// APIClient is the subset of the Docker SDK used by Adapter.
type APIClient interface {
	ContainerList(context.Context, container.ListOptions) ([]container.Summary, error)
	Events(context.Context, events.ListOptions) (<-chan events.Message, <-chan error)
	ContainerCreate(context.Context, *container.Config, *container.HostConfig, *network.NetworkingConfig, *ocispec.Platform, string) (container.CreateResponse, error)
	ContainerStart(context.Context, string, container.StartOptions) error
	ContainerRemove(context.Context, string, container.RemoveOptions) error
}

// Adapter implements discovery and reconciliation clients against the Docker API.
// It lists all containers for discovery but only creates, starts, and removes
// containers explicitly selected by reconciliation.
type Adapter struct {
	client APIClient
}

var _ Client = (*Adapter)(nil)
var _ ReconcileClient = (*Adapter)(nil)

// NewAdapter connects to the Docker daemon configured by the standard Docker
// environment variables, defaulting to the local Unix socket.
func NewAdapter() (*Adapter, error) {
	sdk, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("create Docker client: %w", err)
	}
	return NewAdapterWithClient(sdk)
}

// NewAdapterWithClient constructs an adapter around a Docker SDK client.
func NewAdapterWithClient(client APIClient) (*Adapter, error) {
	if client == nil {
		return nil, fmt.Errorf("docker client is required")
	}
	return &Adapter{client: client}, nil
}

// ListContainers returns all local containers with the metadata discovery uses.
func (a *Adapter) ListContainers(ctx context.Context) ([]Container, error) {
	containers, err := a.client.ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		return nil, err
	}
	result := make([]Container, 0, len(containers))
	for _, summary := range containers {
		name := ""
		if len(summary.Names) > 0 {
			name = summary.Names[0]
		}
		networks := make(map[string]Network)
		if summary.NetworkSettings != nil {
			for networkName := range summary.NetworkSettings.Networks {
				networks[networkName] = Network{}
			}
		}
		ports := make([]uint32, 0, len(summary.Ports))
		for _, port := range summary.Ports {
			ports = append(ports, uint32(port.PrivatePort))
		}
		result = append(result, Container{ID: summary.ID, Name: name, Labels: cloneLabels(summary.Labels), Networks: networks, ExposedPorts: ports})
	}
	return result, nil
}

// Events watches container lifecycle and network attachment events.
func (a *Adapter) Events(ctx context.Context) (<-chan Event, <-chan error) {
	messages, errs := a.client.Events(ctx, events.ListOptions{Filters: filters.NewArgs(filters.Arg("type", "container"))})
	result := make(chan Event)
	go func() {
		defer close(result)
		for message := range messages {
			select {
			case result <- Event{Type: string(message.Type), Action: string(message.Action)}:
			case <-ctx.Done():
				return
			}
		}
	}()
	return result, errs
}

// ListManaged returns containers marked as bridge-owned. Reconciliation applies
// the master identity guard before any removal.
func (a *Adapter) ListManaged(ctx context.Context) ([]ManagedContainer, error) {
	containers, err := a.client.ContainerList(ctx, container.ListOptions{All: true, Filters: filters.NewArgs(filters.Arg("label", ownerLabel+"=true"))})
	if err != nil {
		return nil, err
	}
	result := make([]ManagedContainer, 0, len(containers))
	for _, summary := range containers {
		name := ""
		if len(summary.Names) > 0 {
			name = summary.Names[0]
		}
		result = append(result, ManagedContainer{ID: summary.ID, Name: name, Labels: cloneLabels(summary.Labels)})
	}
	return result, nil
}

// CreateManaged creates and starts one proxy attached solely to its requested network.
func (a *Adapter) CreateManaged(ctx context.Context, spec ContainerSpec) error {
	response, err := a.client.ContainerCreate(ctx, &container.Config{Image: spec.Image, Labels: cloneLabels(spec.Labels), Env: append([]string(nil), spec.Env...)}, &container.HostConfig{}, &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{spec.Network: {}}}, nil, spec.Name)
	if err != nil {
		return err
	}
	if err := a.client.ContainerStart(ctx, response.ID, container.StartOptions{}); err != nil {
		if removeErr := a.client.ContainerRemove(ctx, response.ID, container.RemoveOptions{Force: true}); removeErr != nil {
			return fmt.Errorf("start container: %w (remove failed: %v)", err, removeErr)
		}
		return fmt.Errorf("start container: %w", err)
	}
	return nil
}

// RemoveManaged force-removes a container selected by reconciliation.
func (a *Adapter) RemoveManaged(ctx context.Context, id string) error {
	return a.client.ContainerRemove(ctx, id, container.RemoveOptions{Force: true})
}
