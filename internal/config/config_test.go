package config

import "testing"

func TestLoadMaster(t *testing.T) {
	config, err := loadMaster(environment(masterEnvironment(map[string]string{"BRIDGE_PROXY_PORT_START": "21000", "BRIDGE_PROXY_PORT_END": "21010", "BRIDGE_ENDPOINT_CIDRS": "10.0.0.0/8, 192.168.0.0/16", "BRIDGE_ENDPOINT_DNS_NAMES": " slave.example.test ,slave-2.example.test"})))
	if err != nil {
		t.Fatal(err)
	}
	if config.DataDir != "/bridge" || config.DockerHost != defaultDockerHost || config.ObservabilityAddress != ":8080" || config.ProxyPortStart != 21000 || config.ProxyPortEnd != 21010 || config.RouteTokenRefreshBefore.String() != "1m0s" {
		t.Fatalf("config = %#v", config)
	}
	if len(config.EndpointCIDRs) != 2 || len(config.EndpointDNSNames) != 2 {
		t.Fatalf("endpoint allowlist = %#v, %#v", config.EndpointCIDRs, config.EndpointDNSNames)
	}
}

func TestLoadMasterRejectsInvalidConfiguration(t *testing.T) {
	if _, err := loadMaster(environment(map[string]string{})); err == nil {
		t.Fatal("LoadMaster succeeded without proxy image")
	}
	if _, err := loadMaster(environment(masterEnvironment(map[string]string{"BRIDGE_PROXY_PORT_START": "30000", "BRIDGE_PROXY_PORT_END": "20000"}))); err == nil {
		t.Fatal("LoadMaster succeeded with reversed port range")
	}
	if _, err := loadMaster(environment(masterEnvironment(map[string]string{"BRIDGE_ROUTE_TOKEN_LIFETIME": "1m", "BRIDGE_ROUTE_TOKEN_REFRESH_BEFORE": "1m"}))); err == nil {
		t.Fatal("LoadMaster succeeded with refresh window equal to lifetime")
	}
}

func TestLoadSlave(t *testing.T) {
	config, err := loadSlave(environment(slaveEnvironment(map[string]string{"BRIDGE_CONSTRAINT_LABELS": "bridge.zone=east,team=platform"})))
	if err != nil {
		t.Fatal(err)
	}
	if config.ConstraintLabels["bridge.zone"] != "east" || config.ConstraintLabels["team"] != "platform" {
		t.Fatalf("constraints = %#v", config.ConstraintLabels)
	}
}

func TestLoadSlaveRejectsInvalidConstraints(t *testing.T) {
	_, err := loadSlave(environment(slaveEnvironment(map[string]string{"BRIDGE_CONSTRAINT_LABELS": "invalid"})))
	if err == nil {
		t.Fatal("LoadSlave succeeded with malformed constraints")
	}
}

func TestLoadProxy(t *testing.T) {
	config, err := loadProxy(environment(proxyEnvironment(map[string]string{"BRIDGE_LISTENER_PORTS": "api=21000, metrics=21001", "BRIDGE_ROUTE_TOKENS": "api=token-a,metrics=token-b", "BRIDGE_ROUTE_IDS": "api=route-a,metrics=route-b", "BRIDGE_ROUTE_EXPIRIES": "api=2030-01-01T00:00:00Z,metrics=2030-01-01T00:00:00Z"})))
	if err != nil {
		t.Fatal(err)
	}
	if config.ListenerPorts["api"] != 21000 || config.ListenerPorts["metrics"] != 21001 {
		t.Fatalf("ports = %#v", config.ListenerPorts)
	}
}

func TestLoadProxyRejectsMissingRouteToken(t *testing.T) {
	if _, err := loadProxy(environment(proxyEnvironment(map[string]string{"BRIDGE_LISTENER_PORTS": "api=21000", "BRIDGE_ROUTE_TOKENS": ""}))); err == nil {
		t.Fatal("LoadProxy succeeded without a route token")
	}
}

func environment(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}

func slaveEnvironment(values map[string]string) map[string]string {
	base := map[string]string{
		"BRIDGE_MASTER_ADDRESS": "master:8443", "BRIDGE_DATA_ADDRESS": "slave:8444", "BRIDGE_DOCKER_NETWORK": "apps",
		"BRIDGE_SLAVE_ID": "slave-a", "BRIDGE_CA_FILE": "/certs/ca.crt", "BRIDGE_CERTIFICATE_FILE": "/certs/slave.crt",
		"BRIDGE_PRIVATE_KEY_FILE": "/certs/slave.key", "BRIDGE_ROUTE_SIGNING_KEY_FILE": "/certs/route.key",
	}
	for key, value := range values {
		base[key] = value
	}
	return base
}

func masterEnvironment(values map[string]string) map[string]string {
	base := map[string]string{"BRIDGE_PROXY_IMAGE": "proxy:latest", "BRIDGE_ROUTE_SIGNING_KEY_FILE": "/secrets/route.key", "BRIDGE_PROXY_CERTIFICATE_MOUNT": "bridge-certs:/certs"}
	for key, value := range values {
		base[key] = value
	}
	return base
}

func proxyEnvironment(values map[string]string) map[string]string {
	base := map[string]string{"BRIDGE_UPSTREAM": "https://slave.example:8444", "BRIDGE_CA_FILE": "/certs/ca.crt", "BRIDGE_CERTIFICATE_FILE": "/certs/master-client.crt", "BRIDGE_PRIVATE_KEY_FILE": "/certs/master-client.key", "BRIDGE_ROUTE_TOKENS": "api=token", "BRIDGE_ROUTE_IDS": "api=route", "BRIDGE_ROUTE_EXPIRIES": "api=2030-01-01T00:00:00Z"}
	for key, value := range values {
		base[key] = value
	}
	return base
}
