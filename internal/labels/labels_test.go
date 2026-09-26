package labels

import (
	"testing"

	controlv1 "github.com/traefik/traefik-bridge/api/gen/go/control/v1"
)

func TestValidateKeepsValidApplicationsAndRejectsInvalidReferences(t *testing.T) {
	snapshot := &controlv1.FullSnapshot{Applications: []*controlv1.ApplicationSnapshot{
		app("good", map[string]string{"traefik.http.routers.web.rule": "Host(`example.test`)", "traefik.http.routers.web.service": "api"}, "api"),
		app("bad", map[string]string{"traefik.http.routers.bad.service": "foreign"}, "local"),
	}}
	valid, rejected := Validate("slave-a", snapshot, nil)
	if len(valid) != 1 || valid[0].Snapshot.GetApplicationId() != "good" {
		t.Fatalf("valid = %+v", valid)
	}
	if len(rejected) != 1 || rejected[0].GetApplicationId() != "bad" {
		t.Fatalf("rejected = %+v", rejected)
	}
}

func TestValidateAllowsSameSlaveCrossApplicationReferences(t *testing.T) {
	snapshot := &controlv1.FullSnapshot{Applications: []*controlv1.ApplicationSnapshot{
		app("router", map[string]string{"traefik.http.routers.web.service": "api", "traefik.http.routers.web.middlewares": "auth@docker"}),
		app("service", map[string]string{"traefik.http.middlewares.auth.basicauth.users": "user:hash"}, "api"),
	}}
	valid, rejected := Validate("slave-a", snapshot, nil)
	if len(valid) != 2 || len(rejected) != 0 {
		t.Fatalf("valid=%d rejected=%+v", len(valid), rejected)
	}
}

func TestValidateRejectsOtherSlaveResourceConflict(t *testing.T) {
	valid, rejected := Validate("slave-b", &controlv1.FullSnapshot{Applications: []*controlv1.ApplicationSnapshot{app("app", map[string]string{"traefik.http.routers.web.rule": "Host(`x`)"})}}, map[string]string{"routers/web": "slave-a"})
	if len(valid) != 0 || len(rejected) != 1 || rejected[0].GetCode() != controlv1.RejectionCode_REJECTION_CODE_RESOURCE_CONFLICT {
		t.Fatalf("valid=%+v rejected=%+v", valid, rejected)
	}
}

func TestRewriteBackendPortsPreservesOtherLabels(t *testing.T) {
	application := app("app", map[string]string{"traefik.http.routers.web.rule": "Host(`x`)", "traefik.http.services.api.loadbalancer.passhostheader": "true"}, "api")
	got := RewriteBackendPorts(application, map[string]uint32{"api": 21000})
	if got["traefik.http.routers.web.rule"] != "Host(`x`)" || got["traefik.http.services.api.loadbalancer.passhostheader"] != "true" || got["traefik.http.services.api.loadbalancer.server.port"] != "21000" {
		t.Fatalf("labels = %#v", got)
	}
}

func app(id string, values map[string]string, services ...string) *controlv1.ApplicationSnapshot {
	application := &controlv1.ApplicationSnapshot{ApplicationId: id, Network: "edge", Labels: values}
	for _, service := range services {
		application.Services = append(application.Services, &controlv1.ExportedService{ServiceId: service, TargetPort: 8080})
	}
	return application
}
