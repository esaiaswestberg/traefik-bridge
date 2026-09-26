package config

import "testing"

func TestLoadMaster(t *testing.T) {
	config, err := loadMaster(environment(map[string]string{"BRIDGE_PROXY_IMAGE": "proxy:latest", "BRIDGE_PROXY_PORT_START": "21000", "BRIDGE_PROXY_PORT_END": "21010"}))
	if err != nil {
		t.Fatal(err)
	}
	if config.DataDir != "/bridge" || config.DockerHost != defaultDockerHost || config.ProxyPortStart != 21000 || config.ProxyPortEnd != 21010 {
		t.Fatalf("config = %#v", config)
	}
}

func TestLoadMasterRejectsInvalidConfiguration(t *testing.T) {
	if _, err := loadMaster(environment(map[string]string{})); err == nil {
		t.Fatal("LoadMaster succeeded without proxy image")
	}
	if _, err := loadMaster(environment(map[string]string{"BRIDGE_PROXY_IMAGE": "proxy", "BRIDGE_PROXY_PORT_START": "30000", "BRIDGE_PROXY_PORT_END": "20000"})); err == nil {
		t.Fatal("LoadMaster succeeded with reversed port range")
	}
}

func TestLoadSlave(t *testing.T) {
	config, err := loadSlave(environment(map[string]string{"BRIDGE_MASTER_ADDRESS": "master:8443", "BRIDGE_DATA_ADDRESS": "slave:8444", "BRIDGE_DOCKER_NETWORK": "apps", "BRIDGE_CONSTRAINT_LABELS": "bridge.zone=east,team=platform"}))
	if err != nil {
		t.Fatal(err)
	}
	if config.ConstraintLabels["bridge.zone"] != "east" || config.ConstraintLabels["team"] != "platform" {
		t.Fatalf("constraints = %#v", config.ConstraintLabels)
	}
}

func TestLoadSlaveRejectsInvalidConstraints(t *testing.T) {
	_, err := loadSlave(environment(map[string]string{"BRIDGE_MASTER_ADDRESS": "master", "BRIDGE_DATA_ADDRESS": "slave", "BRIDGE_DOCKER_NETWORK": "apps", "BRIDGE_CONSTRAINT_LABELS": "invalid"}))
	if err == nil {
		t.Fatal("LoadSlave succeeded with malformed constraints")
	}
}

func TestLoadProxy(t *testing.T) {
	config, err := loadProxy(environment(map[string]string{"BRIDGE_LISTENER_PORTS": "api=21000, metrics=21001"}))
	if err != nil {
		t.Fatal(err)
	}
	if config.ListenerPorts["api"] != 21000 || config.ListenerPorts["metrics"] != 21001 {
		t.Fatalf("ports = %#v", config.ListenerPorts)
	}
}

func environment(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}
