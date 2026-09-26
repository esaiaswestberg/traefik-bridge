package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"net"
	"testing"
)

func TestMatchesDataHost(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		DNSNames:    []string{"slave.internal.example"},
		IPAddresses: []net.IP{net.ParseIP("192.0.2.10")},
	}, key)
	if err != nil {
		t.Fatal(err)
	}
	request, err := x509.ParseCertificateRequest(der)
	if err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"slave.internal.example", "SLAVE.INTERNAL.EXAMPLE", "192.0.2.10"} {
		if !matchesDataHost(request, host) {
			t.Errorf("matchesDataHost(%q) = false, want true", host)
		}
	}
	if matchesDataHost(request, "other.internal.example") {
		t.Error("matchesDataHost accepted an unrequested DNS name")
	}
}
