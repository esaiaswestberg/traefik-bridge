package main

import (
	"crypto/tls"
	"crypto/x509"
	"log"
	"net/http"
	"os"
)

func main() {
	ca, err := os.ReadFile("/certs/ca.crt")
	if err != nil {
		log.Fatal(err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		log.Fatal("parse client CA")
	}
	server := &http.Server{
		Addr: ":8443",
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/health/bridge-preserved":
				w.WriteHeader(http.StatusNoContent)
			default:
				_, _ = w.Write([]byte("cross-container mTLS service reached\n"))
			}
		}),
		TLSConfig: &tls.Config{ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: pool, MinVersion: tls.VersionTLS12},
	}
	log.Fatal(server.ListenAndServeTLS("/certs/backend.crt", "/certs/backend.key"))
}
