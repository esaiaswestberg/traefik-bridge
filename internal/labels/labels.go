// Package labels validates and rewrites Traefik HTTP Docker labels.
package labels

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	controlv1 "github.com/traefik/traefik-bridge/api/gen/go/control/v1"
)

const httpPrefix = "traefik.http."

// Application is a valid source application with its labels unchanged except
// for service backend ports supplied by the master reconciler.
type Application struct {
	Snapshot  *controlv1.ApplicationSnapshot
	Resources []Resource
}

// Resource identifies a named Traefik resource.
type Resource struct{ Kind, Name string }

// Validate returns independently valid applications and per-application
// rejections. occupied maps resource keys to the slave which already owns it.
func Validate(slaveID string, snapshot *controlv1.FullSnapshot, occupied map[string]string) ([]Application, []*controlv1.ResourceRejection) {
	if snapshot == nil {
		return nil, []*controlv1.ResourceRejection{rejection("", "", controlv1.RejectionCode_REJECTION_CODE_INVALID_ARGUMENT, "snapshot is required")}
	}
	valid := make([]Application, 0, len(snapshot.GetApplications()))
	rejections := make([]*controlv1.ResourceRejection, 0)
	byResource := make(map[string]int)
	for _, app := range snapshot.GetApplications() {
		resources, err := validateApplication(app)
		if err != nil {
			rejections = append(rejections, rejection(app.GetApplicationId(), "", controlv1.RejectionCode_REJECTION_CODE_INVALID_LABEL, err.Error()))
			continue
		}
		application := Application{Snapshot: cloneApplication(app), Resources: resources}
		conflict := false
		for _, resource := range resources {
			key := resourceKey(resource)
			if owner := occupied[key]; owner != "" && owner != slaveID {
				rejections = append(rejections, rejection(app.GetApplicationId(), key, controlv1.RejectionCode_REJECTION_CODE_RESOURCE_CONFLICT, fmt.Sprintf("%s is already owned by slave %q", key, owner)))
				conflict = true
				break
			}
			if other, exists := byResource[key]; exists {
				rejections = append(rejections, rejection(app.GetApplicationId(), key, controlv1.RejectionCode_REJECTION_CODE_RESOURCE_CONFLICT, fmt.Sprintf("%s is also declared by application %q", key, valid[other].Snapshot.GetApplicationId())))
				conflict = true
				break
			}
		}
		if conflict {
			continue
		}
		valid = append(valid, application)
		for _, resource := range resources {
			byResource[resourceKey(resource)] = len(valid) - 1
		}
	}

	// References are checked after all declarations are known, allowing an
	// application to refer to a resource declared by another application.
	declared := make(map[string]struct{})
	for _, app := range valid {
		for _, resource := range app.Resources {
			declared[resourceKey(resource)] = struct{}{}
		}
	}
	kept := valid[:0]
	for _, app := range valid {
		if err := validateReferences(app.Snapshot.GetLabels(), declared); err != nil {
			rejections = append(rejections, rejection(app.Snapshot.GetApplicationId(), "", controlv1.RejectionCode_REJECTION_CODE_INVALID_LABEL, err.Error()))
			continue
		}
		kept = append(kept, app)
	}
	return kept, rejections
}

func validateApplication(app *controlv1.ApplicationSnapshot) ([]Resource, error) {
	if app == nil || strings.TrimSpace(app.GetApplicationId()) == "" {
		return nil, fmt.Errorf("application ID is required")
	}
	if strings.TrimSpace(app.GetNetwork()) == "" {
		return nil, fmt.Errorf("network is required")
	}
	services := make(map[string]struct{})
	for _, service := range app.GetServices() {
		if !validName(service.GetServiceId()) || service.GetTargetPort() == 0 || service.GetTargetPort() > 65535 {
			return nil, fmt.Errorf("invalid exported service")
		}
		if _, exists := services[service.GetServiceId()]; exists {
			return nil, fmt.Errorf("duplicate exported service %q", service.GetServiceId())
		}
		services[service.GetServiceId()] = struct{}{}
	}
	resources := make([]Resource, 0)
	seen := make(map[string]struct{})
	for key := range app.GetLabels() {
		resource, ok, err := labelResource(key)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		resourceKey := resourceKey(resource)
		if _, exists := seen[resourceKey]; !exists {
			resources, seen[resourceKey] = append(resources, resource), struct{}{}
		}
	}
	for service := range services {
		key := resourceKey(Resource{Kind: "services", Name: service})
		if _, exists := seen[key]; !exists {
			resources, seen[key] = append(resources, Resource{Kind: "services", Name: service}), struct{}{}
		}
	}
	sort.Slice(resources, func(i, j int) bool { return resourceKey(resources[i]) < resourceKey(resources[j]) })
	return resources, nil
}

func labelResource(key string) (Resource, bool, error) {
	if !strings.HasPrefix(key, httpPrefix) {
		return Resource{}, false, nil
	}
	rest := strings.TrimPrefix(key, httpPrefix)
	kind, rest, found := strings.Cut(rest, ".")
	if !found || (kind != "routers" && kind != "services" && kind != "middlewares") {
		return Resource{}, false, nil
	}
	name, _, found := strings.Cut(rest, ".")
	if !found || !validName(name) {
		return Resource{}, false, fmt.Errorf("invalid Traefik HTTP label %q", key)
	}
	return Resource{Kind: kind, Name: name}, true, nil
}

func validateReferences(values map[string]string, declared map[string]struct{}) error {
	for key, value := range values {
		resource, ok, _ := labelResource(key)
		if !ok {
			continue
		}
		if resource.Kind == "routers" && strings.HasSuffix(key, ".service") && value != "" && !has(declared, "services", value) {
			return fmt.Errorf("router %q references service %q outside this slave", resource.Name, value)
		}
		if resource.Kind == "routers" && strings.HasSuffix(key, ".middlewares") {
			if err := references(declared, "middlewares", value); err != nil {
				return err
			}
		}
		if resource.Kind == "middlewares" && strings.HasSuffix(key, ".chain.middlewares") {
			if err := references(declared, "middlewares", value); err != nil {
				return err
			}
		}
	}
	return nil
}

func references(declared map[string]struct{}, kind, values string) error {
	for _, value := range strings.Split(values, ",") {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		name, provider, _ := strings.Cut(value, "@")
		if provider != "" && provider != "docker" {
			continue
		}
		if !has(declared, kind, name) {
			return fmt.Errorf("%s %q is not exported by this slave", kind, value)
		}
	}
	return nil
}
func has(values map[string]struct{}, kind, name string) bool {
	_, ok := values[kind+"/"+name]
	return ok
}
func resourceKey(resource Resource) string { return resource.Kind + "/" + resource.Name }
func validName(value string) bool {
	return strings.TrimSpace(value) != "" && !strings.ContainsAny(value, ".@,=")
}
func rejection(app, resource string, code controlv1.RejectionCode, message string) *controlv1.ResourceRejection {
	return &controlv1.ResourceRejection{ApplicationId: app, ResourceId: resource, Code: code, Message: message}
}

func cloneApplication(application *controlv1.ApplicationSnapshot) *controlv1.ApplicationSnapshot {
	result := &controlv1.ApplicationSnapshot{ApplicationId: application.GetApplicationId(), ContainerName: application.GetContainerName(), Network: application.GetNetwork(), Labels: make(map[string]string, len(application.GetLabels()))}
	for key, value := range application.GetLabels() {
		result.Labels[key] = value
	}
	for _, service := range application.GetServices() {
		result.Services = append(result.Services, &controlv1.ExportedService{ServiceId: service.GetServiceId(), TargetPort: service.GetTargetPort()})
	}
	return result
}

// RewriteBackendPorts applies master-assigned listener ports without changing
// any other user-provided labels.
func RewriteBackendPorts(application *controlv1.ApplicationSnapshot, ports map[string]uint32) map[string]string {
	labels := make(map[string]string, len(application.GetLabels())+len(ports))
	for key, value := range application.GetLabels() {
		labels[key] = value
	}
	for service, port := range ports {
		labels["traefik.http.services."+service+".loadbalancer.server.port"] = strconv.FormatUint(uint64(port), 10)
	}
	return labels
}
