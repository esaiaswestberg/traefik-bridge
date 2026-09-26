# Traefik Bridge Implementation Plan

## Goal

Build a Go-based bridge that exposes Docker applications on remote hosts through a single public Traefik instance.

Host 1 runs Traefik and the bridge master. Host 2 and any additional remote hosts run a bridge slave alongside Docker applications that use normal Traefik HTTP labels.

The bridge uses Traefik's built-in Docker provider. The master creates and manages local, labeled bridge-proxy containers that Traefik treats as ordinary Docker backends. No custom Traefik provider plugin is required.

## Runtime Roles

- `bridge-master`: Runs on Host 1. Receives slave configuration over gRPC, manages enrollment and certificates, and reconciles generated Docker containers.
- `bridge-slave`: Runs on a remote Docker host. Watches the local Docker socket, publishes application snapshots to master, and proxies application traffic to local containers.
- `bridge-proxy`: A lightweight per-application proxy container started by master on Host 1. Traefik routes to it through the Docker provider.

## Architecture

```text
Internet
  |
Host 1
  |
Traefik
  |
generated bridge-proxy container
  |
mTLS HTTP/2
  |
Host 2 bridge-slave
  |
local Docker application container
```

Control traffic is separate from application traffic:

```text
bridge-slave -- gRPC/mTLS --> bridge-master
```

The bridge supports one master and many slaves. Host 1 reaches remote slaves through a private network.

## Configuration Flow

1. An administrator configures the same high-entropy cluster secret in master and slave environment variables.
2. Master persists its generated CA, signing key, master client certificate, enrollment records, and issued slave certificates in a mounted data directory.
3. Slave uses a PAKE exchange based on the shared secret to mutually authenticate during first enrollment.
4. Master issues the slave an mTLS certificate.
5. Slave opens a persistent gRPC/mTLS connection to master.
6. Slave watches Docker events and sends a complete configuration snapshot after each relevant change.
7. Master validates the snapshot and creates or updates bridge-owned Docker containers.
8. Traefik's Docker provider reads the labels on generated containers and creates normal routers, middlewares, services, and TLS routes.

## Docker Label Strategy

Slaves send standard, open-source Traefik HTTP Docker labels unchanged.

Master:

- Preserves router rules, TLS, entrypoints, priorities, middlewares, health checks, and service settings.
- Rewrites only backend-targeting settings needed to point a service to a local generated proxy.
- Rejects resource-name conflicts between slaves.
- Permits overlapping router rules and relies on normal Traefik priority behavior.
- Permits references only to resources exported from the same slave.
- Rejects invalid containers individually without withdrawing valid routes.

This delegates label interpretation to the exact Traefik Docker provider version running on Host 1 instead of duplicating Traefik's label parser.

## Generated Containers

For each exported application container, master creates bridge-owned containers on the configured Docker network shared with Traefik:

- A `bridge-proxy` container receives requests from Traefik.
- A dedicated config container holds routers and middleware labels when viable.
- If Traefik requires service metadata on the same container, master falls back to placing those labels on the application proxy container.
- Every generated resource has ownership labels for master ID, slave ID, source application ID, resource ID, and version.
- Master reconciles existing owned resources after restart and keeps them serving until fresh slave snapshots arrive.

The proxy receives its fixed configuration through environment variables:

- Slave data endpoint.
- Opaque signed route and service identifiers.
- Assigned local listener ports.
- Bridge CA location.
- Master identity expectations.

## Remote Service Routing

A generated proxy represents one remote application container.

- Each declared remote Traefik service is assigned a deterministic internal listener port from a configured range.
- The port assignment derives from slave ID, application ID, and service ID; collisions are detected and resolved deterministically.
- Master rewrites each service's Traefik server-port label to its corresponding local proxy listener port.
- The proxy forwards requests over mTLS HTTP/2 to the correct slave service identity.
- Slave owns balancing across matching local application targets.
- Slave resolves local targets through Docker network IPs, using a configured default network first and `traefik.docker.network` as an override.
- When no explicit service port label exists, slave selects the lowest exposed port.

## Traffic Behavior

The initial release guarantees:

- HTTP and HTTPS public routing with TLS terminating at Host 1.
- WebSockets.
- Streaming requests and responses.
- HTTP/2 between master proxy and slave.
- Request cancellation, trailers, and chunked bodies.
- Original host, path, query, and body preservation.
- Removal of bridge-internal routing details before the application receives the request.
- `X-Forwarded-*` propagation.
- HTTP and HTTPS service health checks forwarded through the bridge to the remote backend.

Slave data listeners require:

- Master Traefik client certificate validation.
- A valid signed opaque service identifier.
- Expiring route IDs with a configurable grace period after configuration changes.

## TLS and Enrollment

Master generates and persists:

- Bridge CA certificate and private key.
- Master server certificate for enrollment and control traffic.
- Master client certificate for Traefik-to-slave traffic.
- Slave client and server certificates.

First enrollment uses an audited PAKE implementation. The shared cluster secret is never sent directly, and the PAKE exchange provides mutual authentication before certificate issuance. Do not implement cryptography in-house.

A small static Traefik file-provider configuration is required for the shared `ServersTransport` used by generated Docker services. It supplies:

- Bridge CA root certificate.
- Master client certificate and private key.
- TLS verification settings.

Generated Docker labels reference that transport, for example `bridge-mtls@file`. Docker remains the provider for all routes, services, and middleware configuration.

## Repository Layout

```text
cmd/
  bridge-master/
  bridge-slave/
  bridge-proxy/
internal/
  config/         YAML and environment loading
  control/        gRPC server, client, enrollment, snapshots
  crypto/         PAKE integration, CA, cert issuance, token storage
  docker/         Discovery and generated-container reconciliation
  labels/         Validation and backend-target rewriting only
  observability/  Logs, metrics, health endpoints
  proxy/          HTTP/2 reverse proxy and service listener routing
  state/          Persistent master state and route-ID lifecycle
api/
  proto/          Bridge control-plane protobuf definitions
deploy/
  compose/
  traefik/
examples/
  two-host/
tests/
  fake-docker/
  integration/
```

## Implementation Phases

1. Bootstrap the Go module, formatting, linting, release builds, and Linux `amd64` and `arm64` targets.
2. Define protobuf control-plane messages for enrollment, certificate renewal, heartbeats, full snapshots, rejections, and resynchronization.
3. Implement master persistent state, CA initialization, issued-certificate tracking, and secret-based PAKE enrollment.
4. Implement mTLS gRPC control connections, heartbeats, slave endpoint allowlists, a two-minute disconnect grace period, and resynchronization after master restart.
5. Implement slave Docker watching with a read-only socket mount, Traefik enable and constraint filtering, network selection, port selection, and full snapshot publishing.
6. Implement master snapshot validation, conflict detection, ownership labels, deterministic names and ports, and Docker reconciliation.
7. Implement proxy data listeners, mTLS upstream forwarding, local slave service pools, WebSockets, streaming, HTTP/2, and forwarded headers.
8. Add health-check forwarding and service target health tracking.
9. Validate Traefik Docker-provider behavior for cross-container router and service references. Fall back to co-locating config labels on proxies if needed.
10. Add Prometheus metrics, structured JSON logs, health and readiness endpoints, and route/status inspection.
11. Build Docker images, Compose examples, installation documentation, and security guidance for master Docker-socket access.

## Technical Spikes

Complete these before committing to the rest of the implementation:

1. Confirm Traefik's Docker provider can resolve routers and middlewares from one generated container to services declared on another.
2. Confirm generated service labels can reference a shared file-provider `ServersTransport` with mTLS client certificates.
3. Confirm Traefik health checks preserve their configured path through the proxy listener.
4. Select an audited, maintained Go PAKE implementation.
5. Prove HTTP/2, WebSocket, trailer, cancellation, and streaming behavior across Traefik, generated proxy, and slave.

## Testing

Use a Dockerless integration suite built from:

- A fake Docker API server and event stream.
- Real in-process gRPC/mTLS master and slave instances.
- Real local TLS HTTP/2 backend servers.
- WebSocket, streaming upload/download, cancellation, and health-check fixtures.
- Fake Traefik-label snapshots covering open-source HTTP labels.
- Reconciliation tests for container creation, replacement, stale-resource cleanup, conflicts, disconnect grace, restart, and resynchronization.
- Security tests for PAKE enrollment, bad secrets, expired certificates, endpoint allowlist rejection, invalid route IDs, and certificate renewal.

Use Docker Compose only for deployable two-host demonstrations, not as the required integration-test runtime.

## Operational Defaults

- One cluster secret supplied through environment variables.
- Immediate enrollment after successful PAKE authentication.
- One master serving many slaves.
- Slave endpoint advertised through the control connection and constrained by CIDR or DNS allowlists.
- Two-minute stale-route grace period.
- Direct master Docker socket mount, explicitly documented as root-equivalent access.
- Static file plus environment configuration.
- Prometheus metrics, structured logs, health, and readiness endpoints.
