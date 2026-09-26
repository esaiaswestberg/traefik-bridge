// Package docker discovers Traefik-enabled local containers for a slave.
package docker

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	controlv1 "github.com/traefik/traefik-bridge/api/gen/go/control/v1"
)

const (
	traefikEnableLabel  = "traefik.enable"
	traefikNetworkLabel = "traefik.docker.network"
	servicePrefix       = "traefik.http.services."
)

// Container is the Docker metadata needed to build an application snapshot.
// Networks maps Docker network names to their container-scoped details. Only
// the network name is currently sent to the master.
type Container struct {
	ID           string
	Name         string
	Labels       map[string]string
	Networks     map[string]Network
	ExposedPorts []uint32
}

// Network describes one network attached to a container.
type Network struct{}

// Event is a Docker event. Type normally is "container".
type Event struct {
	Type   string
	Action string
}

// Client is deliberately limited to the Docker operations discovery needs.
// It can be implemented by the Docker SDK or a test fake.
type Client interface {
	ListContainers(context.Context) ([]Container, error)
	Events(context.Context) (<-chan Event, <-chan error)
}

// SnapshotPublisher delivers snapshots to the slave's control connection.
// Keeping this interface narrow lets discovery remain independent of gRPC.
type SnapshotPublisher interface {
	PublishSnapshot(context.Context, *controlv1.FullSnapshot) error
}

// Config controls Docker application selection.
type Config struct {
	DefaultNetwork   string
	ConstraintLabels map[string]string
}

// Discovery builds snapshots and publishes them when Docker changes.
type Discovery struct {
	client    Client
	publisher SnapshotPublisher
	config    Config
	revision  uint64
}

// NewDiscovery creates Docker discovery. A default network is required so a
// target is never published without a deterministic reachable network.
func NewDiscovery(client Client, publisher SnapshotPublisher, config Config) (*Discovery, error) {
	if client == nil {
		return nil, errors.New("docker client is required")
	}
	if publisher == nil {
		return nil, errors.New("snapshot publisher is required")
	}
	if strings.TrimSpace(config.DefaultNetwork) == "" {
		return nil, errors.New("default Docker network is required")
	}
	return &Discovery{client: client, publisher: publisher, config: config}, nil
}

// Snapshot returns a complete, deterministic snapshot of eligible containers.
func (d *Discovery) Snapshot(ctx context.Context) (*controlv1.FullSnapshot, error) {
	containers, err := d.client.ListContainers(ctx)
	if err != nil {
		return nil, fmt.Errorf("list Docker containers: %w", err)
	}
	applications := make([]*controlv1.ApplicationSnapshot, 0, len(containers))
	for _, container := range containers {
		application, ok := d.application(container)
		if ok {
			applications = append(applications, application)
		}
	}
	sort.Slice(applications, func(i, j int) bool { return applications[i].GetApplicationId() < applications[j].GetApplicationId() })
	d.revision++
	snapshot := &controlv1.FullSnapshot{Revision: d.revision, Applications: applications}
	snapshot.SnapshotId = snapshotID(snapshot)
	return snapshot, nil
}

// Publish sends a newly-built full snapshot through the control abstraction.
func (d *Discovery) Publish(ctx context.Context) error {
	snapshot, err := d.Snapshot(ctx)
	if err != nil {
		return err
	}
	if err := d.publisher.PublishSnapshot(ctx, snapshot); err != nil {
		return fmt.Errorf("publish Docker snapshot: %w", err)
	}
	return nil
}

// Run publishes an initial snapshot, then publishes after events which can
// change container eligibility, network attachment, labels, or ports.
func (d *Discovery) Run(ctx context.Context) error {
	if err := d.Publish(ctx); err != nil {
		return err
	}
	events, errs := d.client.Events(ctx)
	for {
		select {
		case <-ctx.Done():
			return nil
		case err, ok := <-errs:
			if !ok {
				errs = nil
				continue
			}
			if err != nil {
				return fmt.Errorf("watch Docker events: %w", err)
			}
		case event, ok := <-events:
			if !ok {
				return errors.New("docker event stream closed")
			}
			if relevant(event) {
				if err := d.Publish(ctx); err != nil {
					return err
				}
			}
		}
	}
}

func (d *Discovery) application(container Container) (*controlv1.ApplicationSnapshot, bool) {
	if !enabled(container.Labels) || !matchesConstraints(container.Labels, d.config.ConstraintLabels) {
		return nil, false
	}
	network := d.config.DefaultNetwork
	if override := strings.TrimSpace(container.Labels[traefikNetworkLabel]); override != "" {
		network = override
	}
	if _, ok := container.Networks[network]; !ok {
		return nil, false
	}
	services, ok := services(container)
	if !ok {
		return nil, false
	}
	return &controlv1.ApplicationSnapshot{
		ApplicationId: container.ID,
		ContainerName: strings.TrimPrefix(container.Name, "/"),
		Labels:        cloneLabels(container.Labels),
		Network:       network,
		Services:      services,
	}, true
}

func enabled(labels map[string]string) bool {
	return strings.EqualFold(strings.TrimSpace(labels[traefikEnableLabel]), "true")
}

func matchesConstraints(labels, constraints map[string]string) bool {
	for key, expected := range constraints {
		if actual, ok := labels[key]; !ok || actual != expected {
			return false
		}
	}
	return true
}

func services(container Container) ([]*controlv1.ExportedService, bool) {
	ports := make(map[string]uint32)
	names := make(map[string]struct{})
	for key, value := range container.Labels {
		if !strings.HasPrefix(key, servicePrefix) {
			continue
		}
		rest := strings.TrimPrefix(key, servicePrefix)
		name, suffix, found := strings.Cut(rest, ".")
		if !found || name == "" {
			continue
		}
		names[name] = struct{}{}
		if suffix != "loadbalancer.server.port" {
			continue
		}
		port, err := strconv.ParseUint(strings.TrimSpace(value), 10, 16)
		if err != nil || port == 0 {
			return nil, false
		}
		ports[name] = uint32(port)
	}
	for key, value := range container.Labels {
		if !strings.HasPrefix(key, "traefik.http.routers.") || !strings.HasSuffix(key, ".service") || value == "" {
			continue
		}
		names[value] = struct{}{}
	}
	if len(names) == 0 {
		names[strings.TrimPrefix(container.Name, "/")] = struct{}{}
	}
	result := make([]*controlv1.ExportedService, 0, len(names))
	for name := range names {
		port := ports[name]
		if port == 0 {
			fallback, ok := lowestPort(container.ExposedPorts)
			if !ok {
				return nil, false
			}
			port = fallback
		}
		result = append(result, &controlv1.ExportedService{ServiceId: name, TargetPort: port})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].GetServiceId() < result[j].GetServiceId() })
	return result, true
}

func lowestPort(ports []uint32) (uint32, bool) {
	var lowest uint32
	for _, port := range ports {
		if port == 0 || port > 65535 {
			continue
		}
		if lowest == 0 || port < lowest {
			lowest = port
		}
	}
	return lowest, lowest != 0
}

func relevant(event Event) bool {
	if event.Type != "" && event.Type != "container" {
		return false
	}
	switch event.Action {
	case "create", "start", "die", "destroy", "update", "rename", "connect", "disconnect", "pause", "unpause":
		return true
	default:
		return false
	}
}

func cloneLabels(labels map[string]string) map[string]string {
	result := make(map[string]string, len(labels))
	for key, value := range labels {
		result[key] = value
	}
	return result
}

func snapshotID(snapshot *controlv1.FullSnapshot) []byte {
	hash := sha256.New()
	for _, application := range snapshot.GetApplications() {
		writeString(hash, application.GetApplicationId())
		writeString(hash, application.GetContainerName())
		writeString(hash, application.GetNetwork())
		keys := make([]string, 0, len(application.GetLabels()))
		for key := range application.GetLabels() {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			writeString(hash, key)
			writeString(hash, application.GetLabels()[key])
		}
		for _, service := range application.GetServices() {
			writeString(hash, service.GetServiceId())
			var port [4]byte
			binary.BigEndian.PutUint32(port[:], service.GetTargetPort())
			_, _ = hash.Write(port[:])
		}
	}
	return hash.Sum(nil)
}

func writeString(hash interface{ Write([]byte) (int, error) }, value string) {
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(value)))
	_, _ = hash.Write(length[:])
	_, _ = hash.Write([]byte(value))
}
