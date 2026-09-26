// Package config loads and validates bridge runtime configuration from the environment.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

const defaultDockerHost = "unix:///var/run/docker.sock"

// Master configures the bridge master runtime.
type Master struct {
	DataDir              string
	ProxyImage           string
	DockerHost           string
	ControlAddress       string
	ObservabilityAddress string
	EndpointCIDRs        []string
	EndpointDNSNames     []string
	ProxyPortStart       uint32
	ProxyPortEnd         uint32
}

// Slave configures the bridge slave runtime.
type Slave struct {
	MasterAddress        string
	MasterServerName     string
	DataAddress          string
	ObservabilityAddress string
	DockerHost           string
	DefaultNetwork       string
	SlaveID              string
	CAFile               string
	CertificateFile      string
	PrivateKeyFile       string
	RouteSigningKeyFile  string
	ConstraintLabels     map[string]string
}

// Proxy configures one generated bridge proxy.
type Proxy struct {
	ListenerPorts map[string]uint16
}

// LoadMaster loads a complete master configuration from BRIDGE_* variables.
func LoadMaster() (Master, error) {
	return loadMaster(os.Getenv)
}

// LoadSlave loads a complete slave configuration from BRIDGE_* variables.
func LoadSlave() (Slave, error) {
	return loadSlave(os.Getenv)
}

// LoadProxy loads a complete proxy configuration from BRIDGE_* variables.
func LoadProxy() (Proxy, error) {
	return loadProxy(os.Getenv)
}

func loadMaster(getenv func(string) string) (Master, error) {
	config := Master{
		DataDir:              value(getenv, "BRIDGE_DATA_DIR", "/bridge"),
		ProxyImage:           strings.TrimSpace(getenv("BRIDGE_PROXY_IMAGE")),
		DockerHost:           value(getenv, "BRIDGE_DOCKER_HOST", defaultDockerHost),
		ControlAddress:       value(getenv, "BRIDGE_CONTROL_ADDRESS", ":8443"),
		ObservabilityAddress: value(getenv, "BRIDGE_OBSERVABILITY_ADDRESS", ":8080"),
		EndpointCIDRs:        list(getenv("BRIDGE_ENDPOINT_CIDRS")),
		EndpointDNSNames:     list(getenv("BRIDGE_ENDPOINT_DNS_NAMES")),
		ProxyPortStart:       20000,
		ProxyPortEnd:         29999,
	}
	if config.ProxyImage == "" {
		return Master{}, errors.New("BRIDGE_PROXY_IMAGE is required")
	}
	var err error
	if config.ProxyPortStart, err = port(getenv, "BRIDGE_PROXY_PORT_START", config.ProxyPortStart); err != nil {
		return Master{}, err
	}
	if config.ProxyPortEnd, err = port(getenv, "BRIDGE_PROXY_PORT_END", config.ProxyPortEnd); err != nil {
		return Master{}, err
	}
	if config.ProxyPortEnd < config.ProxyPortStart {
		return Master{}, errors.New("BRIDGE_PROXY_PORT_END must not be less than BRIDGE_PROXY_PORT_START")
	}
	return config, nil
}

func loadSlave(getenv func(string) string) (Slave, error) {
	config := Slave{
		MasterAddress:        strings.TrimSpace(getenv("BRIDGE_MASTER_ADDRESS")),
		MasterServerName:     value(getenv, "BRIDGE_MASTER_SERVER_NAME", "bridge-master"),
		DataAddress:          strings.TrimSpace(getenv("BRIDGE_DATA_ADDRESS")),
		ObservabilityAddress: value(getenv, "BRIDGE_OBSERVABILITY_ADDRESS", ":8080"),
		DockerHost:           value(getenv, "BRIDGE_DOCKER_HOST", defaultDockerHost),
		DefaultNetwork:       strings.TrimSpace(getenv("BRIDGE_DOCKER_NETWORK")),
		SlaveID:              strings.TrimSpace(getenv("BRIDGE_SLAVE_ID")),
		CAFile:               strings.TrimSpace(getenv("BRIDGE_CA_FILE")),
		CertificateFile:      strings.TrimSpace(getenv("BRIDGE_CERTIFICATE_FILE")),
		PrivateKeyFile:       strings.TrimSpace(getenv("BRIDGE_PRIVATE_KEY_FILE")),
		RouteSigningKeyFile:  strings.TrimSpace(getenv("BRIDGE_ROUTE_SIGNING_KEY_FILE")),
	}
	if config.MasterAddress == "" {
		return Slave{}, errors.New("BRIDGE_MASTER_ADDRESS is required")
	}
	if config.DataAddress == "" {
		return Slave{}, errors.New("BRIDGE_DATA_ADDRESS is required")
	}
	if config.DefaultNetwork == "" {
		return Slave{}, errors.New("BRIDGE_DOCKER_NETWORK is required")
	}
	for name, path := range map[string]string{
		"BRIDGE_SLAVE_ID":               config.SlaveID,
		"BRIDGE_CA_FILE":                config.CAFile,
		"BRIDGE_CERTIFICATE_FILE":       config.CertificateFile,
		"BRIDGE_PRIVATE_KEY_FILE":       config.PrivateKeyFile,
		"BRIDGE_ROUTE_SIGNING_KEY_FILE": config.RouteSigningKeyFile,
	} {
		if path == "" {
			return Slave{}, fmt.Errorf("%s is required", name)
		}
	}
	constraints, err := labels(getenv("BRIDGE_CONSTRAINT_LABELS"))
	if err != nil {
		return Slave{}, err
	}
	config.ConstraintLabels = constraints
	return config, nil
}

func loadProxy(getenv func(string) string) (Proxy, error) {
	ports, err := listenerPorts(getenv("BRIDGE_LISTENER_PORTS"))
	if err != nil {
		return Proxy{}, err
	}
	return Proxy{ListenerPorts: ports}, nil
}

func value(getenv func(string) string, name, fallback string) string {
	if value := strings.TrimSpace(getenv(name)); value != "" {
		return value
	}
	return fallback
}

func list(value string) []string {
	var result []string
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			result = append(result, item)
		}
	}
	return result
}

func port(getenv func(string) string, name string, fallback uint32) (uint32, error) {
	value := strings.TrimSpace(getenv(name))
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseUint(value, 10, 16)
	if err != nil || parsed == 0 {
		return 0, fmt.Errorf("%s must be a port number", name)
	}
	return uint32(parsed), nil
}

func labels(value string) (map[string]string, error) {
	result := make(map[string]string)
	if strings.TrimSpace(value) == "" {
		return result, nil
	}
	for _, pair := range strings.Split(value, ",") {
		key, expected, ok := strings.Cut(pair, "=")
		key, expected = strings.TrimSpace(key), strings.TrimSpace(expected)
		if !ok || key == "" || expected == "" {
			return nil, errors.New("BRIDGE_CONSTRAINT_LABELS must contain comma-separated key=value pairs")
		}
		if _, exists := result[key]; exists {
			return nil, fmt.Errorf("BRIDGE_CONSTRAINT_LABELS contains duplicate key %q", key)
		}
		result[key] = expected
	}
	return result, nil
}

func listenerPorts(value string) (map[string]uint16, error) {
	result := make(map[string]uint16)
	if strings.TrimSpace(value) == "" {
		return nil, errors.New("BRIDGE_LISTENER_PORTS is required")
	}
	for _, pair := range strings.Split(value, ",") {
		service, rawPort, ok := strings.Cut(pair, "=")
		service, rawPort = strings.TrimSpace(service), strings.TrimSpace(rawPort)
		if !ok || service == "" {
			return nil, errors.New("BRIDGE_LISTENER_PORTS must contain comma-separated service=port pairs")
		}
		parsed, err := strconv.ParseUint(rawPort, 10, 16)
		if err != nil || parsed == 0 {
			return nil, fmt.Errorf("listener port for service %q must be a port number", service)
		}
		if _, exists := result[service]; exists {
			return nil, fmt.Errorf("BRIDGE_LISTENER_PORTS contains duplicate service %q", service)
		}
		result[service] = uint16(parsed)
	}
	return result, nil
}
