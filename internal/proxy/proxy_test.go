package proxy

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"
)

func TestMasterToSlaveMTLSHTTP2PreservesRequestAndTrailers(t *testing.T) {
	t.Parallel()
	signer, err := NewSigner([]byte("test route signing key"))
	if err != nil {
		t.Fatal(err)
	}
	token, err := signer.Sign(Route{RouteID: "route", ServiceID: "service", ExpiresAt: time.Now().Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Error(err)
		}
		if got, want := request.Host, "original.example"; got != want {
			t.Errorf("host = %q, want %q", got, want)
		}
		if got, want := request.URL.RequestURI(), "/stream?query=value"; got != want {
			t.Errorf("request URI = %q, want %q", got, want)
		}
		if got, want := string(body), "streaming body"; got != want {
			t.Errorf("body = %q, want %q", got, want)
		}
		if got := request.Header.Get(RouteHeader); got != "" {
			t.Errorf("internal route header reached backend: %q", got)
		}
		if got := request.Header.Get("X-Forwarded-Host"); got != "original.example" {
			t.Errorf("X-Forwarded-Host = %q", got)
		}
		writer.Header().Add("Trailer", "X-Stream-Status")
		_, _ = writer.Write([]byte("response stream"))
		writer.Header().Set("X-Stream-Status", "complete")
	}))
	defer backend.Close()
	backendURL, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatal(err)
	}

	serverCertificate, clientCertificate, roots := certificates(t)
	slave, err := NewSlave(SlaveConfig{Signer: signer, Services: map[string][]*url.URL{"service": {backendURL}}})
	if err != nil {
		t.Fatal(err)
	}
	slaveServer := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if got, want := request.ProtoMajor, 2; got != want {
			t.Errorf("master-to-slave protocol = HTTP/%d, want HTTP/%d", got, want)
		}
		slave.ServeHTTP(writer, request)
	}))
	slaveServer.EnableHTTP2 = true
	slaveServer.TLS = ServerTLSConfig(serverCertificate, roots)
	slaveServer.StartTLS()
	defer slaveServer.Close()
	slaveURL, err := url.Parse(slaveServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	master, err := NewMaster(MasterConfig{Upstream: slaveURL, RouteToken: token, TLSConfig: &tls.Config{RootCAs: roots, Certificates: []tls.Certificate{clientCertificate}, ServerName: "slave.test", MinVersion: tls.VersionTLS13}})
	if err != nil {
		t.Fatal(err)
	}
	masterServer := httptest.NewServer(master)
	defer masterServer.Close()

	request, err := http.NewRequest(http.MethodPost, masterServer.URL+"/stream?query=value", bytes.NewBufferString("streaming body"))
	if err != nil {
		t.Fatal(err)
	}
	request.Host = "original.example"
	request.Header.Set(RouteHeader, "untrusted")
	response, err := masterServer.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if got, want := response.Header.Get("X-Stream-Status"), ""; got != want {
		t.Errorf("trailer exposed as header = %q", got)
	}
	if body, err := io.ReadAll(response.Body); err != nil || string(body) != "response stream" {
		t.Errorf("response body = %q, err = %v", body, err)
	}
	if got, want := response.Trailer.Get("X-Stream-Status"), "complete"; got != want {
		t.Errorf("trailer = %q, want %q", got, want)
	}
}

func TestExpiredRouteRejectedAfterGrace(t *testing.T) {
	t.Parallel()
	signer, err := NewSigner([]byte("test route signing key"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	signer.now = func() time.Time { return now }
	token, err := signer.Sign(Route{RouteID: "route", ServiceID: "service", ExpiresAt: now.Add(-time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	slave, err := NewSlave(SlaveConfig{Signer: signer, Services: map[string][]*url.URL{"service": {mustURL(t, "http://127.0.0.1:1")}}})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "http://bridge/", nil)
	request.Header.Set(RouteHeader, token)
	response := httptest.NewRecorder()
	slave.ServeHTTP(response, request)
	if got, want := response.Code, http.StatusForbidden; got != want {
		t.Errorf("status = %d, want %d", got, want)
	}
}

func TestSlaveHealthChecksUseNormalRoutingAndRecoverTargets(t *testing.T) {
	var firstHealthy atomic.Bool
	var healthPath atomic.Value
	first := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/health/ready" {
			if request.URL.RequestURI() != "/health/ready?full=true" {
				t.Errorf("health request URI = %q", request.URL.RequestURI())
			}
			healthPath.Store(request.URL.RequestURI())
		}
		if !firstHealthy.Load() {
			http.Error(writer, "unhealthy", http.StatusServiceUnavailable)
			return
		}
		_, _ = writer.Write([]byte("first"))
	}))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = writer.Write([]byte("second"))
	}))
	defer second.Close()

	signer, err := NewSigner([]byte("test route signing key"))
	if err != nil {
		t.Fatal(err)
	}
	slave, err := NewSlave(SlaveConfig{Signer: signer, Services: map[string][]*url.URL{"service": {mustURL(t, first.URL), mustURL(t, second.URL)}}})
	if err != nil {
		t.Fatal(err)
	}
	defer slave.Close()
	check := HealthCheck{Path: "/health/ready?full=true", Scheme: "http", Timeout: time.Second}
	slave.checkTarget(t.Context(), "service", 0, check)
	if got := healthPath.Load(); got != "/health/ready?full=true" {
		t.Fatalf("health path = %v", got)
	}
	if got := slave.Status()["service"][0].Healthy; got {
		t.Fatal("failed target remained healthy")
	}
	if got := serveSlave(t, slave, signer, "service"); got != "second" {
		t.Fatalf("response from unhealthy pool = %q, want second", got)
	}

	firstHealthy.Store(true)
	slave.checkTarget(t.Context(), "service", 0, check)
	if got := slave.Status()["service"][0].Healthy; !got {
		t.Fatal("recovered target remained unhealthy")
	}
	if got := serveSlave(t, slave, signer, "service"); got != "first" {
		t.Fatalf("response after recovery = %q, want first", got)
	}
}

func TestSlaveHealthCheckPreservesHTTPS(t *testing.T) {
	backend := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/secure-health" {
			t.Errorf("health path = %q", request.URL.Path)
		}
		_, _ = writer.Write([]byte("healthy"))
	}))
	defer backend.Close()
	signer, err := NewSigner([]byte("test route signing key"))
	if err != nil {
		t.Fatal(err)
	}
	slave, err := NewSlave(SlaveConfig{Signer: signer, Services: map[string][]*url.URL{"service": {mustURL(t, backend.URL)}}, Transport: backend.Client().Transport})
	if err != nil {
		t.Fatal(err)
	}
	defer slave.Close()
	slave.checkTarget(t.Context(), "service", 0, HealthCheck{Path: "/secure-health", Scheme: "https", Timeout: time.Second})
	if got := slave.Status()["service"][0]; !got.Healthy {
		t.Fatalf("HTTPS target status = %+v", got)
	}
}

func serveSlave(t *testing.T, slave *Slave, signer *Signer, service string) string {
	t.Helper()
	token, err := signer.Sign(Route{RouteID: "route", ServiceID: service, ExpiresAt: time.Now().Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "http://bridge/", nil)
	request.Header.Set(RouteHeader, token)
	response := httptest.NewRecorder()
	slave.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q", response.Code, response.Body.String())
	}
	return response.Body.String()
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	value, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func certificates(t *testing.T) (tls.Certificate, tls.Certificate, *x509.CertPool) {
	t.Helper()
	return issueCertificates(t)
}

func issueCertificates(t *testing.T) (tls.Certificate, tls.Certificate, *x509.CertPool) {
	t.Helper()
	// The standard library test helper is intentionally avoided so this remains self-contained.
	caDER, caKey, err := newCertificate(nil, nil, "bridge-ca", nil, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	parsedCAKey, err := x509.ParsePKCS8PrivateKey(caKey)
	if err != nil {
		t.Fatal(err)
	}
	caPrivateKey, ok := parsedCAKey.(*ecdsa.PrivateKey)
	if !ok {
		t.Fatal("CA key is not ECDSA")
	}
	serverDER, serverKey, err := newCertificate(caDER, caPrivateKey, "slave.test", []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, false, []string{"slave.test"})
	if err != nil {
		t.Fatal(err)
	}
	clientDER, clientKey, err := newCertificate(caDER, caPrivateKey, "master.test", []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	server, err := tls.X509KeyPair(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverDER}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: serverKey}))
	if err != nil {
		t.Fatal(err)
	}
	client, err := tls.X509KeyPair(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: clientDER}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: clientKey}))
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(mustCertificate(t, caDER))
	return server, client, roots
}

func newCertificate(parentDER []byte, parentKey *ecdsa.PrivateKey, commonName string, usages []x509.ExtKeyUsage, isCA bool, dnsNames []string) ([]byte, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, err
	}
	now := time.Now()
	template := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: commonName}, DNSNames: dnsNames, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour), BasicConstraintsValid: true, IsCA: isCA, ExtKeyUsage: usages, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment}
	if isCA {
		template.KeyUsage |= x509.KeyUsageCertSign
	}
	parent := template
	signer := key
	if parentDER != nil {
		parent, err = x509.ParseCertificate(parentDER)
		if err != nil {
			return nil, nil, err
		}
		signer = parentKey
	}
	der, err := x509.CreateCertificate(rand.Reader, template, parent, &key.PublicKey, signer)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return der, keyDER, nil
}

func mustCertificate(t *testing.T, der []byte) *x509.Certificate {
	t.Helper()
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return certificate
}
