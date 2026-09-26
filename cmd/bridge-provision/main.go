// bridge-provision signs an offline slave CSR using an existing master authority.
package main

import (
	"crypto/x509"
	"encoding/pem"
	"flag"
	"fmt"
	"net"
	"os"
	"strings"

	bridgecrypto "github.com/traefik/traefik-bridge/internal/crypto"
	"github.com/traefik/traefik-bridge/internal/state"
)

func main() {
	dataDir := flag.String("data-dir", "/bridge", "existing bridge-master data directory")
	slaveID := flag.String("slave-id", "", "slave identity to provision")
	dataHost := flag.String("data-host", "", "advertised slave data DNS name or IP, required in the CSR SAN")
	csrFile := flag.String("csr", "", "PEM-encoded certificate signing request")
	outputDir := flag.String("output-dir", "", "directory for ca.crt and slave.crt")
	replace := flag.Bool("replace", false, "replace an existing certificate for this slave ID")
	flag.Parse()
	if err := run(*dataDir, *slaveID, *dataHost, *csrFile, *outputDir, *replace); err != nil {
		fmt.Fprintln(os.Stderr, "bridge-provision:", err)
		os.Exit(1)
	}
}

func run(dataDir, slaveID, dataHost, csrFile, outputDir string, replace bool) error {
	if strings.TrimSpace(slaveID) == "" || strings.TrimSpace(dataHost) == "" || strings.TrimSpace(csrFile) == "" || strings.TrimSpace(outputDir) == "" {
		return fmt.Errorf("-slave-id, -data-host, -csr, and -output-dir are required")
	}
	csr, err := os.ReadFile(csrFile)
	if err != nil {
		return fmt.Errorf("read CSR: %w", err)
	}
	authority, err := bridgecrypto.Load(state.NewStore(dataDir))
	if err != nil {
		return fmt.Errorf("load existing master authority: %w", err)
	}
	if authority.HasSlave(slaveID) && !replace {
		return fmt.Errorf("slave %q already has a certificate; use -replace to revoke it by issuing a replacement", slaveID)
	}
	block, _ := pem.Decode(csr)
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		return fmt.Errorf("CSR must be a PEM CERTIFICATE REQUEST")
	}
	request, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return fmt.Errorf("parse CSR: %w", err)
	}
	if err := request.CheckSignature(); err != nil {
		return fmt.Errorf("verify CSR: %w", err)
	}
	if !matchesDataHost(request, dataHost) {
		return fmt.Errorf("CSR SAN does not contain data host %q", dataHost)
	}
	issued, err := authority.IssueSlave(slaveID, block.Bytes)
	if err != nil {
		return fmt.Errorf("issue slave certificate: %w", err)
	}
	if err := authority.ExportSlavePEM(outputDir, issued.CertificatePEM); err != nil {
		return fmt.Errorf("export slave certificate: %w", err)
	}
	return nil
}

func matchesDataHost(request *x509.CertificateRequest, host string) bool {
	if ip := net.ParseIP(host); ip != nil {
		for _, candidate := range request.IPAddresses {
			if candidate.Equal(ip) {
				return true
			}
		}
		return false
	}
	for _, candidate := range request.DNSNames {
		if strings.EqualFold(candidate, host) {
			return true
		}
	}
	return false
}
