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
	"sync"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	controlv1 "github.com/traefik/traefik-bridge/api/gen/go/control/v1"
	"github.com/traefik/traefik-bridge/internal/labels"
	"github.com/traefik/traefik-bridge/internal/proxy"
	"github.com/traefik/traefik-bridge/internal/state"
)

const (
	ownerLabel        = "traefik.bridge.owner"
	masterLabel       = "traefik.bridge.master"
	slaveLabel        = "traefik.bridge.slave"
	applicationLabel  = "traefik.bridge.application"
	resourceLabel     = "traefik.bridge.resource"
	routeExpiryLabel  = "traefik.bridge.route-expires-at"
	specLabel         = "traefik.bridge.spec"
	reconcilerVersion = 1
)

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
type Mount struct{ Source, Target string }

func ParseMount(value string) (Mount, error) {
	source, target, ok := strings.Cut(value, ":")
	if !ok || strings.TrimSpace(source) == "" || strings.TrimSpace(target) == "" || !strings.HasPrefix(strings.TrimSpace(target), "/") {
		return Mount{}, errors.New("certificate mount must be source:target")
	}
	return Mount{Source: strings.TrimSpace(source), Target: strings.TrimRight(strings.TrimSpace(target), "/")}, nil
}

type ReconcileClient interface {
	ListManaged(context.Context) ([]ManagedContainer, error)
	CreateManaged(context.Context, ContainerSpec) error
	RemoveManaged(context.Context, string) error
}

// ReconcilerStateStore persists desired configuration and expiry timestamps, never tokens.
type ReconcilerStateStore interface {
	Load() (*state.ReconcilerState, error)
	Save(*state.ReconcilerState) error
}

type ReconcileConfig struct {
	MasterID, ProxyImage                        string
	PortStart, PortEnd                          uint32
	Signer                                      *proxy.Signer
	RouteTokenLifetime, RouteTokenRefreshBefore time.Duration
	CertificateMount                            Mount
	StateStore                                  ReconcilerStateStore
}

type Reconciler struct {
	client          ReconcileClient
	config          ReconcileConfig
	mu              sync.Mutex
	snapshots       map[string][]labels.Application
	endpoints       map[string]*controlv1.SlaveEndpoint
	versions        map[string]uint64
	expiries        map[string]time.Time
	restored        bool
	scheduleChanged chan struct{}
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
	if config.RouteTokenLifetime <= 0 {
		return nil, errors.New("route token lifetime must be positive")
	}
	if config.RouteTokenRefreshBefore == 0 {
		config.RouteTokenRefreshBefore = time.Minute
	}
	if config.RouteTokenRefreshBefore <= 0 || config.RouteTokenRefreshBefore >= config.RouteTokenLifetime {
		return nil, errors.New("route token refresh before must be positive and less than the route token lifetime")
	}
	return &Reconciler{client: client, config: config, snapshots: map[string][]labels.Application{}, endpoints: map[string]*controlv1.SlaveEndpoint{}, versions: map[string]uint64{}, expiries: map[string]time.Time{}, scheduleChanged: make(chan struct{}, 1)}, nil
}

// Restore loads accepted snapshots and expiry timestamps before control traffic starts.
func (r *Reconciler) Restore() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.restored {
		return nil
	}
	if r.config.StateStore == nil {
		r.restored = true
		return nil
	}
	persisted, err := r.config.StateStore.Load()
	if state.IsNotExist(err) {
		r.restored = true
		return nil
	}
	if err != nil {
		return fmt.Errorf("load reconciler state: %w", err)
	}
	if persisted.Version != reconcilerVersion {
		return fmt.Errorf("unsupported reconciler state version %d", persisted.Version)
	}
	for slaveID, saved := range persisted.Slaves {
		if slaveID == "" || saved.EndpointHost == "" || saved.EndpointPort == 0 {
			return errors.New("invalid reconciler state")
		}
		var snapshot controlv1.FullSnapshot
		if err := protojson.Unmarshal(saved.Snapshot, &snapshot); err != nil {
			return fmt.Errorf("decode reconciler snapshot for %q: %w", slaveID, err)
		}
		apps, rejected := labels.Validate(slaveID, &snapshot, nil)
		if len(rejected) != 0 {
			return fmt.Errorf("invalid reconciler snapshot for %q", slaveID)
		}
		r.snapshots[slaveID], r.endpoints[slaveID], r.versions[slaveID] = apps, &controlv1.SlaveEndpoint{Host: saved.EndpointHost, Port: saved.EndpointPort}, saved.Revision
		for app := range apps {
			key := identity(slaveID, apps[app].Snapshot.GetApplicationId())
			expires := saved.Expiries[key]
			if expires.IsZero() {
				return fmt.Errorf("missing expiry for proxy %q", key)
			}
			r.expiries[key] = expires
		}
	}
	r.restored = true
	return nil
}

// Run restores state if necessary and refreshes only proxies whose expiry enters the refresh window.
func (r *Reconciler) Run(ctx context.Context) error {
	if err := r.Restore(); err != nil {
		return err
	}
	r.mu.Lock()
	err := r.reconcileLocked(ctx)
	r.mu.Unlock()
	if err != nil {
		return err
	}
	timer := time.NewTimer(r.nextRefreshDelay())
	defer stopTimer(timer)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-r.scheduleChanged:
			resetTimer(timer, r.nextRefreshDelay())
		case <-timer.C:
			if err := r.refresh(ctx); err != nil {
				return err
			}
			resetTimer(timer, r.nextRefreshDelay())
		}
	}
}

func resetTimer(timer *time.Timer, delay time.Duration) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(delay)
}

func stopTimer(timer *time.Timer) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
}

func (r *Reconciler) nextRefreshDelay() time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	next := time.Time{}
	for _, expires := range r.expiries {
		due := expires.Add(-r.config.RouteTokenRefreshBefore)
		if next.IsZero() || due.Before(next) {
			next = due
		}
	}
	if next.IsZero() {
		return time.Hour
	}
	if delay := time.Until(next); delay > 0 {
		return delay
	}
	return 0
}

func (r *Reconciler) refresh(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now().UTC()
	changed := false
	candidate := cloneExpiries(r.expiries)
	for key, expires := range candidate {
		if !expires.After(now.Add(r.config.RouteTokenRefreshBefore)) {
			candidate[key], changed = now.Add(r.config.RouteTokenLifetime), true
		}
	}
	if !changed {
		return nil
	}
	if err := r.persistLocked(r.snapshots, r.endpoints, r.versions, candidate); err != nil {
		return err
	}
	r.expiries = candidate
	return r.reconcileLocked(ctx)
}

// Apply validates a snapshot and persists its desired state before Docker mutation.
func (r *Reconciler) Apply(ctx context.Context, slaveID string, endpoint *controlv1.SlaveEndpoint, snapshot *controlv1.FullSnapshot) ([]*controlv1.ResourceRejection, error) {
	if slaveID == "" {
		return nil, errors.New("slave ID is required")
	}
	if endpoint == nil || endpoint.GetHost() == "" || endpoint.GetPort() == 0 {
		return nil, errors.New("slave endpoint is required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	occupied := map[string]string{}
	for owner, applications := range r.snapshots {
		if owner != slaveID {
			for _, app := range applications {
				for _, resource := range app.Resources {
					occupied[resource.Kind+"/"+resource.Name] = owner
				}
			}
		}
	}
	applications, rejected := labels.Validate(slaveID, snapshot, occupied)
	candidateSnapshots := cloneSnapshots(r.snapshots)
	candidateEndpoints := cloneEndpoints(r.endpoints)
	candidateVersions := cloneVersions(r.versions)
	candidateExpiries := cloneExpiries(r.expiries)
	old := candidateSnapshots[slaveID]
	candidateSnapshots[slaveID], candidateEndpoints[slaveID], candidateVersions[slaveID] = applications, &controlv1.SlaveEndpoint{Host: endpoint.GetHost(), Port: endpoint.GetPort()}, snapshot.GetRevision()
	for _, app := range applications {
		key := identity(slaveID, app.Snapshot.GetApplicationId())
		if candidateExpiries[key].IsZero() || applicationChanged(old, app) || !sameEndpoint(r.endpoints[slaveID], endpoint) {
			candidateExpiries[key] = time.Now().UTC().Add(r.config.RouteTokenLifetime)
		}
	}
	for _, app := range old {
		if !hasApplication(applications, app.Snapshot.GetApplicationId()) {
			delete(candidateExpiries, identity(slaveID, app.Snapshot.GetApplicationId()))
		}
	}
	if err := r.persistLocked(candidateSnapshots, candidateEndpoints, candidateVersions, candidateExpiries); err != nil {
		return rejected, err
	}
	r.snapshots, r.endpoints, r.versions, r.expiries = candidateSnapshots, candidateEndpoints, candidateVersions, candidateExpiries
	r.wakeScheduler()
	if err := r.reconcileLocked(ctx); err != nil {
		return rejected, err
	}
	return rejected, nil
}

func (r *Reconciler) wakeScheduler() {
	select {
	case r.scheduleChanged <- struct{}{}:
	default:
	}
}

func (r *Reconciler) persistLocked(snapshots map[string][]labels.Application, endpoints map[string]*controlv1.SlaveEndpoint, versions map[string]uint64, expiries map[string]time.Time) error {
	if r.config.StateStore == nil {
		return nil
	}
	persisted := &state.ReconcilerState{Version: reconcilerVersion, Slaves: map[string]state.ReconcilerSlave{}}
	for slave, apps := range snapshots {
		snapshot := &controlv1.FullSnapshot{Revision: versions[slave]}
		for _, app := range apps {
			snapshot.Applications = append(snapshot.Applications, app.Snapshot)
		}
		encoded, err := protojson.Marshal(snapshot)
		if err != nil {
			return fmt.Errorf("encode reconciler snapshot: %w", err)
		}
		savedExpiries := map[string]time.Time{}
		for _, app := range apps {
			key := identity(slave, app.Snapshot.GetApplicationId())
			savedExpiries[key] = expiries[key]
		}
		endpoint := endpoints[slave]
		persisted.Slaves[slave] = state.ReconcilerSlave{EndpointHost: endpoint.GetHost(), EndpointPort: endpoint.GetPort(), Revision: versions[slave], Snapshot: encoded, Expiries: savedExpiries}
	}
	if err := r.config.StateStore.Save(persisted); err != nil {
		return fmt.Errorf("persist reconciler state: %w", err)
	}
	return nil
}

func (r *Reconciler) reconcileLocked(ctx context.Context) error {
	type desired struct {
		slave    string
		app      labels.Application
		ports    map[string]uint32
		spec     ContainerSpec
		endpoint *controlv1.SlaveEndpoint
	}
	all := []desired{}
	for slave, apps := range r.snapshots {
		for _, app := range apps {
			all = append(all, desired{slave: slave, app: app, ports: map[string]uint32{}, endpoint: r.endpoints[slave]})
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
	used := map[uint32]struct{}{}
	for i := range all {
		for _, service := range all[i].app.Snapshot.GetServices() {
			key := identity(all[i].slave, all[i].app.Snapshot.GetApplicationId()) + "/" + service.GetServiceId()
			port := r.port(key, used)
			used[port] = struct{}{}
			all[i].ports[service.GetServiceId()] = port
		}
	}
	desiredByName := map[string]ContainerSpec{}
	for i := range all {
		app, key := all[i].app.Snapshot, identity(all[i].slave, all[i].app.Snapshot.GetApplicationId())
		expires := r.expiries[key]
		containerLabels := labels.RewriteBackendPorts(app, all[i].ports)
		containerLabels[ownerLabel], containerLabels[masterLabel], containerLabels[slaveLabel], containerLabels[applicationLabel], containerLabels[resourceLabel], containerLabels[routeExpiryLabel] = "true", r.config.MasterID, all[i].slave, app.GetApplicationId(), app.GetApplicationId(), expires.UTC().Format(time.RFC3339Nano)
		env, err := r.listenerEnvironment(all[i].slave, app.GetApplicationId(), all[i].ports, all[i].endpoint, expires)
		if err != nil {
			return err
		}
		containerLabels[specLabel] = staticSpecHash(r.config.ProxyImage, app.GetNetwork(), r.config.CertificateMount, env)
		all[i].spec = ContainerSpec{Name: containerName(all[i].slave, app.GetApplicationId()), Image: r.config.ProxyImage, Network: app.GetNetwork(), Labels: containerLabels, Env: env, Mounts: []Mount{r.config.CertificateMount}}
		desiredByName[all[i].spec.Name] = all[i].spec
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
		if err := r.client.CreateManaged(ctx, desiredByName[name]); err != nil {
			return fmt.Errorf("create bridge container %q: %w", name, err)
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
func sameEndpoint(a, b *controlv1.SlaveEndpoint) bool {
	return a != nil && b != nil && a.GetHost() == b.GetHost() && a.GetPort() == b.GetPort()
}
func hasApplication(apps []labels.Application, id string) bool {
	for _, app := range apps {
		if app.Snapshot.GetApplicationId() == id {
			return true
		}
	}
	return false
}
func applicationChanged(apps []labels.Application, want labels.Application) bool {
	for _, app := range apps {
		if app.Snapshot.GetApplicationId() == want.Snapshot.GetApplicationId() {
			return !proto.Equal(app.Snapshot, want.Snapshot)
		}
	}
	return true
}
func cloneSnapshots(values map[string][]labels.Application) map[string][]labels.Application {
	result := map[string][]labels.Application{}
	for slave, apps := range values {
		for _, app := range apps {
			result[slave] = append(result[slave], labels.Application{Snapshot: proto.Clone(app.Snapshot).(*controlv1.ApplicationSnapshot), Resources: append([]labels.Resource(nil), app.Resources...)})
		}
	}
	return result
}
func cloneEndpoints(values map[string]*controlv1.SlaveEndpoint) map[string]*controlv1.SlaveEndpoint {
	result := map[string]*controlv1.SlaveEndpoint{}
	for key, value := range values {
		result[key] = proto.Clone(value).(*controlv1.SlaveEndpoint)
	}
	return result
}
func cloneVersions(values map[string]uint64) map[string]uint64 {
	result := map[string]uint64{}
	for key, value := range values {
		result[key] = value
	}
	return result
}
func cloneExpiries(values map[string]time.Time) map[string]time.Time {
	result := map[string]time.Time{}
	for key, value := range values {
		result[key] = value
	}
	return result
}

func staticSpecHash(image, network string, mount Mount, env []string) string {
	values := make([]string, 0, len(env)+3)
	values = append(values, image, network, mount.Source+":"+mount.Target)
	for _, value := range env {
		if !strings.HasPrefix(value, "BRIDGE_ROUTE_TOKENS=") && !strings.HasPrefix(value, "BRIDGE_ROUTE_EXPIRIES=") {
			values = append(values, value)
		}
	}
	sort.Strings(values)
	sum := sha256.Sum256([]byte(strings.Join(values, "\x00")))
	return hex.EncodeToString(sum[:])
}

func (r *Reconciler) listenerEnvironment(slave, application string, ports map[string]uint32, endpoint *controlv1.SlaveEndpoint, expires time.Time) ([]string, error) {
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
	tokens, routeIDs, expiries := make([]string, 0, len(services)), make([]string, 0, len(services)), make([]string, 0, len(services))
	for _, service := range services {
		routeID := identity(slave, application) + "/" + service
		token, err := r.config.Signer.Sign(proxy.Route{RouteID: routeID, ServiceID: service, ExpiresAt: expires})
		if err != nil {
			return nil, fmt.Errorf("sign route %q: %w", routeID, err)
		}
		tokens, routeIDs, expiries = append(tokens, service+"="+token), append(routeIDs, service+"="+routeID), append(expiries, service+"="+expires.UTC().Format(time.RFC3339))
	}
	return append(env, "BRIDGE_ROUTE_TOKENS="+strings.Join(tokens, ","), "BRIDGE_ROUTE_IDS="+strings.Join(routeIDs, ","), "BRIDGE_ROUTE_EXPIRIES="+strings.Join(expiries, ",")), nil
}
