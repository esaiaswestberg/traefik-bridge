// Package config loads and validates bridge runtime configuration from the environment.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

const defaultDockerHost = "unix:///var/run/docker.sock"

// Master configures the bridge master runtime.
type Master struct {
	DataDir               string
	ProxyImage            string
	DockerHost            string
	ControlAddress        string
	ObservabilityAddress  string
	EndpointCIDRs         []string
	EndpointDNSNames      []string
	ProxyPortStart        uint32
	ProxyPortEnd          uint32
	RouteSigningKeyFile   string
	ProxyCertificateMount string
	RouteTokenLifetime    time.Duration
	EnrollmentAddress     string
	EnrollmentSecretFile  string
}

// Slave configures the bridge slave runtime.
type Slave struct {
	MasterAddress        string
	EnrollmentAddress    string
	MasterServerName     string
	DataAddress          string
	DataListenAddress    string
	ObservabilityAddress string
	DockerHost           string
	DefaultNetwork       string
	SlaveID              string
	CAFile               string
	CertificateFile      string
	PrivateKeyFile       string
	RouteSigningKeyFile  string
	ConstraintLabels     map[string]string
	EnrollmentCAFile     string
	EnrollmentSecretFile string
	EnrollmentCSRFile    string
}

// Proxy configures one generated bridge proxy.
type Proxy struct {
	ListenerPorts   map[string]uint16
	Upstream        string
	RouteTokens     map[string]string
	RouteIDs        map[string]string
	RouteExpiries   map[string]string
	CAFile          string
	CertificateFile string
	PrivateKeyFile  string
	ServerName      string
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
		DataDir:               value(getenv, "BRIDGE_DATA_DIR", "/bridge"),
		ProxyImage:            strings.TrimSpace(getenv("BRIDGE_PROXY_IMAGE")),
		DockerHost:            value(getenv, "BRIDGE_DOCKER_HOST", defaultDockerHost),
		ControlAddress:        value(getenv, "BRIDGE_CONTROL_ADDRESS", ":8443"),
		ObservabilityAddress:  value(getenv, "BRIDGE_OBSERVABILITY_ADDRESS", ":8080"),
		EndpointCIDRs:         list(getenv("BRIDGE_ENDPOINT_CIDRS")),
		EndpointDNSNames:      list(getenv("BRIDGE_ENDPOINT_DNS_NAMES")),
		ProxyPortStart:        20000,
		ProxyPortEnd:          29999,
		RouteSigningKeyFile:   strings.TrimSpace(getenv("BRIDGE_ROUTE_SIGNING_KEY_FILE")),
		ProxyCertificateMount: strings.TrimSpace(getenv("BRIDGE_PROXY_CERTIFICATE_MOUNT")),
		RouteTokenLifetime:    5 * time.Minute,
		EnrollmentAddress:     strings.TrimSpace(getenv("BRIDGE_ENROLLMENT_ADDRESS")),
		EnrollmentSecretFile:  strings.TrimSpace(getenv("BRIDGE_DEVELOPMENT_ENROLLMENT_SECRET_FILE")),
	}
	if config.ProxyImage == "" {
		return Master{}, errors.New("BRIDGE_PROXY_IMAGE is required")
	}
	if config.RouteSigningKeyFile == "" {
		return Master{}, errors.New("BRIDGE_ROUTE_SIGNING_KEY_FILE is required")
	}
	if config.ProxyCertificateMount == "" || !strings.Contains(config.ProxyCertificateMount, ":") {
		return Master{}, errors.New("BRIDGE_PROXY_CERTIFICATE_MOUNT must be a source:target mount")
	}
	var err error
	if config.RouteTokenLifetime, err = duration(getenv, "BRIDGE_ROUTE_TOKEN_LIFETIME", config.RouteTokenLifetime); err != nil {
		return Master{}, err
	}
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
		EnrollmentAddress:    value(getenv, "BRIDGE_ENROLLMENT_ADDRESS", getenv("BRIDGE_MASTER_ADDRESS")),
		MasterServerName:     value(getenv, "BRIDGE_MASTER_SERVER_NAME", "bridge-master"),
		DataAddress:          strings.TrimSpace(getenv("BRIDGE_DATA_ADDRESS")),
		DataListenAddress:    value(getenv, "BRIDGE_DATA_LISTEN_ADDRESS", getenv("BRIDGE_DATA_ADDRESS")),
		ObservabilityAddress: value(getenv, "BRIDGE_OBSERVABILITY_ADDRESS", ":8080"),
		DockerHost:           value(getenv, "BRIDGE_DOCKER_HOST", defaultDockerHost),
		DefaultNetwork:       strings.TrimSpace(getenv("BRIDGE_DOCKER_NETWORK")),
		SlaveID:              strings.TrimSpace(getenv("BRIDGE_SLAVE_ID")),
		CAFile:               strings.TrimSpace(getenv("BRIDGE_CA_FILE")),
		CertificateFile:      strings.TrimSpace(getenv("BRIDGE_CERTIFICATE_FILE")),
		PrivateKeyFile:       strings.TrimSpace(getenv("BRIDGE_PRIVATE_KEY_FILE")),
		RouteSigningKeyFile:  strings.TrimSpace(getenv("BRIDGE_ROUTE_SIGNING_KEY_FILE")),
		EnrollmentCAFile:     strings.TrimSpace(getenv("BRIDGE_ENROLLMENT_CA_FILE")),
		EnrollmentSecretFile: strings.TrimSpace(getenv("BRIDGE_DEVELOPMENT_ENROLLMENT_SECRET_FILE")),
		EnrollmentCSRFile:    strings.TrimSpace(getenv("BRIDGE_ENROLLMENT_CSR_FILE")),
	}
	if config.MasterAddress == "" {
		return Slave{}, errors.New("BRIDGE_MASTER_ADDRESS is required")
	}
	if config.MasterServerName != "bridge-master" {
		return Slave{}, errors.New("BRIDGE_MASTER_SERVER_NAME must be bridge-master for the current master certificate")
	}
	if config.DataAddress == "" {
		return Slave{}, errors.New("BRIDGE_DATA_ADDRESS is required")
	}
	if config.DataListenAddress == "" {
		return Slave{}, errors.New("BRIDGE_DATA_LISTEN_ADDRESS is required")
	}
	if config.DefaultNetwork == "" {
		return Slave{}, errors.New("BRIDGE_DOCKER_NETWORK is required")
	}
	for name, path := range map[string]string{
		"BRIDGE_SLAVE_ID":               config.SlaveID,
		"BRIDGE_ROUTE_SIGNING_KEY_FILE": config.RouteSigningKeyFile,
	} {
		if path == "" {
			return Slave{}, fmt.Errorf("%s is required", name)
		}
	}
	credentialPaths := []string{config.CAFile, config.CertificateFile, config.PrivateKeyFile}
	for _, path := range credentialPaths {
		if path == "" {
			return Slave{}, errors.New("BRIDGE_CA_FILE, BRIDGE_CERTIFICATE_FILE, and BRIDGE_PRIVATE_KEY_FILE are required")
		}
	}
	if config.EnrollmentSecretFile != "" && config.EnrollmentCAFile == "" {
		return Slave{}, errors.New("BRIDGE_ENROLLMENT_CA_FILE is required with development enrollment")
	}
	if config.EnrollmentSecretFile != "" && config.EnrollmentCSRFile == "" {
		return Slave{}, errors.New("BRIDGE_ENROLLMENT_CSR_FILE is required with development enrollment")
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
	tokens, err := stringMap(getenv("BRIDGE_ROUTE_TOKENS"), "BRIDGE_ROUTE_TOKENS")
	if err != nil {
		return Proxy{}, err
	}
	routeIDs, err := stringMap(getenv("BRIDGE_ROUTE_IDS"), "BRIDGE_ROUTE_IDS")
	if err != nil {
		return Proxy{}, err
	}
	routeExpiries, err := stringMap(getenv("BRIDGE_ROUTE_EXPIRIES"), "BRIDGE_ROUTE_EXPIRIES")
	if err != nil {
		return Proxy{}, err
	}
	config := Proxy{ListenerPorts: ports, Upstream: strings.TrimSpace(getenv("BRIDGE_UPSTREAM")), RouteTokens: tokens, RouteIDs: routeIDs, RouteExpiries: routeExpiries, CAFile: strings.TrimSpace(getenv("BRIDGE_CA_FILE")), CertificateFile: strings.TrimSpace(getenv("BRIDGE_CERTIFICATE_FILE")), PrivateKeyFile: strings.TrimSpace(getenv("BRIDGE_PRIVATE_KEY_FILE")), ServerName: strings.TrimSpace(getenv("BRIDGE_UPSTREAM_SERVER_NAME"))}
	for name, value := range map[string]string{"BRIDGE_UPSTREAM": config.Upstream, "BRIDGE_CA_FILE": config.CAFile, "BRIDGE_CERTIFICATE_FILE": config.CertificateFile, "BRIDGE_PRIVATE_KEY_FILE": config.PrivateKeyFile} {
		if value == "" {
			return Proxy{}, fmt.Errorf("%s is required", name)
		}
	}
	for service := range config.ListenerPorts {
		if config.RouteTokens[service] == "" || config.RouteIDs[service] == "" || config.RouteExpiries[service] == "" {
			return Proxy{}, fmt.Errorf("route configuration is missing service %q", service)
		}
		if _, err := time.Parse(time.RFC3339, config.RouteExpiries[service]); err != nil {
			return Proxy{}, fmt.Errorf("route expiry for service %q is invalid", service)
		}
	}
	return config, nil
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

func duration(getenv func(string) string, name string, fallback time.Duration) (time.Duration, error) {
	value := strings.TrimSpace(getenv(name))
	if value == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration", name)
	}
	return parsed, nil
}

func labels(value string) (map[string]string, error) {
	return stringMap(value, "BRIDGE_CONSTRAINT_LABELS")
}

func stringMap(value, name string) (map[string]string, error) {
	result := make(map[string]string)
	if strings.TrimSpace(value) == "" {
		return result, nil
	}
	for _, pair := range strings.Split(value, ",") {
		key, expected, ok := strings.Cut(pair, "=")
		key, expected = strings.TrimSpace(key), strings.TrimSpace(expected)
		if !ok || key == "" || expected == "" {
			return nil, fmt.Errorf("%s must contain comma-separated key=value pairs", name)
		}
		if _, exists := result[key]; exists {
			return nil, fmt.Errorf("%s contains duplicate key %q", name, key)
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
