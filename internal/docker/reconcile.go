package docker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	controlv1 "github.com/traefik/traefik-bridge/api/gen/go/control/v1"
	"github.com/traefik/traefik-bridge/internal/labels"
	"github.com/traefik/traefik-bridge/internal/proxy"
)

const (
	ownerLabel       = "traefik.bridge.owner"
	masterLabel      = "traefik.bridge.master"
	slaveLabel       = "traefik.bridge.slave"
	applicationLabel = "traefik.bridge.application"
	resourceLabel    = "traefik.bridge.resource"
	versionLabel     = "traefik.bridge.version"
)

// ManagedContainer is the narrow Docker representation reconciliation needs.
type ManagedContainer struct {
	ID, Name string
	Labels   map[string]string
}
type ContainerSpec struct {
	Name, Image, Network string
	Labels               map[string]string
	Env                  []string
	Mounts               []Mount
}

// Mount describes a read-only host bind or named-volume mount.
type Mount struct{ Source, Target string }

// ParseMount parses a Docker volume or absolute bind source and container target.
func ParseMount(value string) (Mount, error) {
	source, target, ok := strings.Cut(value, ":")
	if !ok || strings.TrimSpace(source) == "" || strings.TrimSpace(target) == "" || !strings.HasPrefix(strings.TrimSpace(target), "/") {
		return Mount{}, errors.New("certificate mount must be source:target")
	}
	return Mount{Source: strings.TrimSpace(source), Target: strings.TrimRight(strings.TrimSpace(target), "/")}, nil
}

// ReconcileClient intentionally excludes all Docker operations not needed here.
type ReconcileClient interface {
	ListManaged(context.Context) ([]ManagedContainer, error)
	CreateManaged(context.Context, ContainerSpec) error
	RemoveManaged(context.Context, string) error
}
type ReconcileConfig struct {
	MasterID, ProxyImage string
	PortStart, PortEnd   uint32
	Signer               *proxy.Signer
	RouteTokenLifetime   time.Duration
	CertificateMount     Mount
}
type Reconciler struct {
	client    ReconcileClient
	config    ReconcileConfig
	snapshots map[string][]labels.Application
	endpoints map[string]*controlv1.SlaveEndpoint
	versions  map[string]uint64
}

func NewReconciler(client ReconcileClient, config ReconcileConfig) (*Reconciler, error) {
	if client == nil || config.MasterID == "" || config.ProxyImage == "" || config.Signer == nil || config.CertificateMount.Source == "" || config.CertificateMount.Target == "" {
		return nil, errors.New("docker client, master ID, proxy image, signer, and certificate mount are required")
	}
	if config.PortStart == 0 {
		config.PortStart = 20000
	}
	if config.PortEnd == 0 {
		config.PortEnd = 29999
	}
	if config.PortEnd < config.PortStart {
		return nil, errors.New("invalid proxy port range")
	}
	if config.RouteTokenLifetime == 0 {
		config.RouteTokenLifetime = 5 * time.Minute
	}
	if config.RouteTokenLifetime < 0 {
		return nil, errors.New("route token lifetime must not be negative")
	}
	return &Reconciler{client: client, config: config, snapshots: make(map[string][]labels.Application), endpoints: make(map[string]*controlv1.SlaveEndpoint), versions: make(map[string]uint64)}, nil
}

// Apply validates one full slave snapshot, reconciles every valid application,
// and returns invalid resources without withdrawing other valid applications.
func (r *Reconciler) Apply(ctx context.Context, slaveID string, endpoint *controlv1.SlaveEndpoint, snapshot *controlv1.FullSnapshot) ([]*controlv1.ResourceRejection, error) {
	if slaveID == "" {
		return nil, errors.New("slave ID is required")
	}
	if endpoint == nil || endpoint.GetHost() == "" || endpoint.GetPort() == 0 {
		return nil, errors.New("slave endpoint is required")
	}
	occupied := make(map[string]string)
	for owner, applications := range r.snapshots {
		if owner != slaveID {
			for _, application := range applications {
				for _, resource := range application.Resources {
					occupied[resource.Kind+"/"+resource.Name] = owner
				}
			}
		}
	}
	applications, rejected := labels.Validate(slaveID, snapshot, occupied)
	r.snapshots[slaveID] = applications
	r.endpoints[slaveID] = &controlv1.SlaveEndpoint{Host: endpoint.GetHost(), Port: endpoint.GetPort()}
	r.versions[slaveID] = snapshot.GetRevision()
	if err := r.reconcile(ctx); err != nil {
		return rejected, err
	}
	return rejected, nil
}

func (r *Reconciler) reconcile(ctx context.Context) error {
	type desired struct {
		slave    string
		app      labels.Application
		ports    map[string]uint32
		spec     ContainerSpec
		endpoint *controlv1.SlaveEndpoint
	}
	all := make([]desired, 0)
	for slave, applications := range r.snapshots {
		for _, application := range applications {
			all = append(all, desired{slave: slave, app: application, ports: make(map[string]uint32), endpoint: r.endpoints[slave]})
		}
	}
	sort.Slice(all, func(i, j int) bool {
		return identity(all[i].slave, all[i].app.Snapshot.GetApplicationId()) < identity(all[j].slave, all[j].app.Snapshot.GetApplicationId())
	})
	count := uint32(0)
	for _, item := range all {
		count += uint32(len(item.app.Snapshot.GetServices()))
	}
	if count > r.config.PortEnd-r.config.PortStart+1 {
		return errors.New("proxy port range is exhausted")
	}
	used := make(map[uint32]struct{})
	for i := range all {
		for _, service := range all[i].app.Snapshot.GetServices() {
			key := identity(all[i].slave, all[i].app.Snapshot.GetApplicationId()) + "/" + service.GetServiceId()
			port := r.port(key, used)
			used[port] = struct{}{}
			all[i].ports[service.GetServiceId()] = port
		}
	}
	desiredByName := make(map[string]ContainerSpec)
	for i := range all {
		app := all[i].app.Snapshot
		name := containerName(all[i].slave, app.GetApplicationId())
		containerLabels := labels.RewriteBackendPorts(app, all[i].ports)
		containerLabels[ownerLabel], containerLabels[masterLabel], containerLabels[slaveLabel], containerLabels[applicationLabel], containerLabels[resourceLabel], containerLabels[versionLabel] = "true", r.config.MasterID, all[i].slave, app.GetApplicationId(), app.GetApplicationId(), strconv.FormatUint(r.versions[all[i].slave], 10)
		env, err := r.listenerEnvironment(all[i].slave, app.GetApplicationId(), all[i].ports, all[i].endpoint)
		if err != nil {
			return err
		}
		all[i].spec = ContainerSpec{Name: name, Image: r.config.ProxyImage, Network: app.GetNetwork(), Labels: containerLabels, Env: env, Mounts: []Mount{r.config.CertificateMount}}
		desiredByName[name] = all[i].spec
	}
	existing, err := r.client.ListManaged(ctx)
	if err != nil {
		return fmt.Errorf("list bridge containers: %w", err)
	}
	for _, container := range existing {
		if container.Labels[ownerLabel] != "true" || container.Labels[masterLabel] != r.config.MasterID {
			continue
		}
		spec, wanted := desiredByName[container.Name]
		if !wanted {
			if _, received := r.snapshots[container.Labels[slaveLabel]]; !received {
				continue
			}
			if err := r.client.RemoveManaged(ctx, container.ID); err != nil {
				return fmt.Errorf("remove stale bridge container %q: %w", container.Name, err)
			}
			continue
		}
		if !sameLabels(container.Labels, spec.Labels) {
			if err := r.client.RemoveManaged(ctx, container.ID); err != nil {
				return err
			}
			if err := r.client.CreateManaged(ctx, spec); err != nil {
				return fmt.Errorf("replace bridge container %q: %w", spec.Name, err)
			}
		}
		delete(desiredByName, container.Name)
	}
	names := make([]string, 0, len(desiredByName))
	for name := range desiredByName {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		spec := desiredByName[name]
		if err := r.client.CreateManaged(ctx, spec); err != nil {
			return fmt.Errorf("create bridge container %q: %w", spec.Name, err)
		}
	}
	return nil
}
func (r *Reconciler) port(key string, used map[uint32]struct{}) uint32 {
	sum := sha256.Sum256([]byte(key))
	span := r.config.PortEnd - r.config.PortStart + 1
	candidate := r.config.PortStart + (uint32(sum[0])<<24|uint32(sum[1])<<16|uint32(sum[2])<<8|uint32(sum[3]))%span
	for {
		if _, exists := used[candidate]; !exists {
			return candidate
		}
		candidate = r.config.PortStart + (candidate-r.config.PortStart+1)%span
	}
}
func identity(slave, app string) string { return slave + "/" + app }
func containerName(slave, app string) string {
	sum := sha256.Sum256([]byte(identity(slave, app)))
	return "traefik-bridge-" + hex.EncodeToString(sum[:8])
}
func sameLabels(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for key, value := range a {
		if b[key] != value {
			return false
		}
	}
	return true
}
func (r *Reconciler) listenerEnvironment(slave, application string, ports map[string]uint32, endpoint *controlv1.SlaveEndpoint) ([]string, error) {
	services := make([]string, 0, len(ports))
	for service := range ports {
		services = append(services, service)
	}
	sort.Strings(services)
	values := make([]string, 0, len(services))
	for _, service := range services {
		values = append(values, service+"="+strconv.FormatUint(uint64(ports[service]), 10))
	}
	upstream := "https://" + endpoint.GetHost() + ":" + strconv.FormatUint(uint64(endpoint.GetPort()), 10)
	env := []string{"BRIDGE_UPSTREAM=" + upstream, "BRIDGE_UPSTREAM_SERVER_NAME=" + endpoint.GetHost(), "BRIDGE_LISTENER_PORTS=" + strings.Join(values, ","), "BRIDGE_CA_FILE=" + r.config.CertificateMount.Target + "/ca.crt", "BRIDGE_CERTIFICATE_FILE=" + r.config.CertificateMount.Target + "/master-client.crt", "BRIDGE_PRIVATE_KEY_FILE=" + r.config.CertificateMount.Target + "/master-client.key"}
	tokens := make([]string, 0, len(services))
	routeIDs := make([]string, 0, len(services))
	expiries := make([]string, 0, len(services))
	for _, service := range services {
		routeID := identity(slave, application) + "/" + service
		expires := time.Now().Add(r.config.RouteTokenLifetime).UTC()
		token, err := r.config.Signer.Sign(proxy.Route{RouteID: routeID, ServiceID: service, ExpiresAt: expires})
		if err != nil {
			return nil, fmt.Errorf("sign route %q: %w", routeID, err)
		}
		tokens = append(tokens, service+"="+token)
		routeIDs = append(routeIDs, service+"="+routeID)
		expiries = append(expiries, service+"="+expires.Format(time.RFC3339))
	}
	env = append(env, "BRIDGE_ROUTE_TOKENS="+strings.Join(tokens, ","), "BRIDGE_ROUTE_IDS="+strings.Join(routeIDs, ","), "BRIDGE_ROUTE_EXPIRIES="+strings.Join(expiries, ","))
	return env, nil
}
