# Traefik Docker Provider Spike

Run `./tests/integration/docker-provider/run.sh` to validate Phase 9 against
Traefik's Docker provider. The runner starts a router-only labeled container,
a separately labeled TLS backend that requires a client certificate, and a
file-provider `ServersTransport`. The backend accepts health checks only at
`/health/bridge-preserved`; Traefik reports the service `UP` only when that
path and the mTLS transport both work.

The router container includes an unused placeholder service port because the
Docker provider otherwise rejects all labels on a container with no port.

The script exits successfully with `SKIP:` when Docker or Docker Compose is
unavailable. It otherwise leaves no containers or volumes behind.
