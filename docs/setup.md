# Deployment Guide

Traefik Bridge supports three deployment methods:

1. [Docker Compose](#docker-compose) for a reproducible host configuration.
2. [Docker CLI](#docker-cli) for hosts managed with direct Docker commands.
3. [Portainer](#portainer) for hosts managed through Portainer stacks.

Every method requires a master host running Traefik and `bridge-master`, plus a slave host running `bridge-slave`. The master creates `bridge-proxy` containers on its local Docker daemon.

## Shared Requirements

- Docker Engine on both hosts.
- Direct LAN or VPN reachability between master and slave. Use a private VLAN, WireGuard, Tailscale, or equivalent. Do not send bridge traffic through the public internet.
- A public DNS record for each routed application, pointing to Traefik on the master host.
- Firewall rules allowing slave-to-master TCP `9443` and `9445`, and master-to-slave TCP `9444` only over the LAN/VPN.

Use these values in the examples. Only addresses and the slave identity are deployment-specific.

```text
Master LAN address: 10.0.0.10
Slave LAN address:  10.0.0.20
Slave ID:           remote-host-1
Bridge network:     traefik-bridge
```

All examples use these published images:

```text
ghcr.io/esaiaswestberg/traefik-bridge-master:latest
ghcr.io/esaiaswestberg/traefik-bridge-slave:latest
ghcr.io/esaiaswestberg/traefik-bridge-proxy:latest
```

Replace `latest` with the same release tag on all three images to pin a deployment.

## Common Master Files

Before starting Traefik or the master, create the master data directory. Run this command on the master host.

```sh
install -d -m 700 /srv/traefik-bridge/master
```

The master creates its CA and route-signing key privately and generates `bridge-transport.yml` in this directory after exporting its TLS material. The generated transport file is safe to mount read-only into Traefik.

The development enrollment flow uses an unaudited OPAQUE implementation. Use [offline certificate provisioning](installation.md#offline-certificate-provisioning) or an external PKI for production.

## Docker Compose

### Master: Traefik And Bridge Master

Create `/srv/traefik-bridge/compose.master.yml` on the master host. Replace the ACME email and ensure `whoami.example.com`-style DNS records point at this host before requesting certificates.

```yaml
services:
  bridge-master:
    image: ghcr.io/esaiaswestberg/traefik-bridge-master:latest
    restart: unless-stopped
    ports:
      - "9443:8443"
      - "9445:8445"
    environment:
      BRIDGE_PROXY_IMAGE: ghcr.io/esaiaswestberg/traefik-bridge-proxy:latest
      BRIDGE_PROXY_CERTIFICATE_MOUNT: /srv/traefik-bridge/master:/bridge
      BRIDGE_ENDPOINT_CIDRS: 10.0.0.20/32
      BRIDGE_ENROLLMENT_ADDRESS: :8445
      BRIDGE_ROUTE_TOKEN_LIFETIME: 5m
      BRIDGE_ROUTE_TOKEN_REFRESH_BEFORE: 1m
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      - /srv/traefik-bridge/master:/bridge
    healthcheck:
      test: ["CMD-SHELL", "wget -q -O - http://localhost:8080/readyz >/dev/null"]
      interval: 10s
      timeout: 3s
      retries: 3

  traefik:
    image: traefik:v3.6
    restart: unless-stopped
    depends_on:
      bridge-master:
        condition: service_healthy
    command:
      - --entrypoints.web.address=:80
      - --entrypoints.websecure.address=:443
      - --entrypoints.web.http.redirections.entrypoint.to=websecure
      - --entrypoints.web.http.redirections.entrypoint.scheme=https
      - --providers.docker=true
      - --providers.docker.exposedbydefault=false
      - --providers.file.filename=/bridge/bridge-transport.yml
      - --certificatesresolvers.letsencrypt.acme.email=admin@example.com
      - --certificatesresolvers.letsencrypt.acme.storage=/letsencrypt/acme.json
      - --certificatesresolvers.letsencrypt.acme.httpchallenge.entrypoint=web
    ports:
      - "80:80"
      - "443:443"
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock:ro
      - /srv/traefik-bridge/master:/bridge:ro
      - traefik-letsencrypt:/letsencrypt
    networks:
      - traefik-bridge

networks:
  traefik-bridge:
    name: traefik-bridge

volumes:
  traefik-letsencrypt:
```

Start it:

```sh
docker compose -f /srv/traefik-bridge/compose.master.yml up -d
docker compose -f /srv/traefik-bridge/compose.master.yml ps
```

If Traefik already exists, retain its current Compose service and add the file-provider argument, the master data-directory mount, and the `traefik-bridge` network shown above. `bridge-master` generates `/bridge/bridge-transport.yml` on every start.

### Slave: Bridge Slave

For development pairing, retrieve the one-time pairing code from the master logs. No bootstrap CA, route-signing key, or enrollment secret transfer is required. The code pins the master's TLS identity and is consumed after one successful enrollment; retrieve a new code before pairing each additional slave. On the slave, prepare a writable credential directory.

```sh
docker compose -f /srv/traefik-bridge/compose.master.yml logs bridge-master | grep pairing_code
# Run on the slave.
install -d -m 700 /srv/traefik-bridge/credentials
export BRIDGE_DEVELOPMENT_PAIRING_CODE='bridge-pair-v1.example'
```

Create `/srv/traefik-bridge/compose.slave.yml` on the slave:

```yaml
services:
  bridge-slave:
    image: ghcr.io/esaiaswestberg/traefik-bridge-slave:latest
    restart: unless-stopped
    ports:
      - "9444:8444"
    environment:
      BRIDGE_SLAVE_ID: remote-host-1
      BRIDGE_MASTER_ADDRESS: 10.0.0.10:9443
      BRIDGE_ENROLLMENT_ADDRESS: 10.0.0.10:9445
      BRIDGE_MASTER_SERVER_NAME: bridge-master
      BRIDGE_DATA_ADDRESS: 10.0.0.20:9444
      BRIDGE_DATA_LISTEN_ADDRESS: :8444
      BRIDGE_DOCKER_NETWORK: traefik-bridge
      BRIDGE_DEVELOPMENT_PAIRING_CODE: ${BRIDGE_DEVELOPMENT_PAIRING_CODE:-}
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock:ro
      - /srv/traefik-bridge/credentials:/run/bridge
    networks:
      - traefik-bridge

networks:
  traefik-bridge:
    name: traefik-bridge
```

Start the slave:

```sh
docker compose -f /srv/traefik-bridge/compose.slave.yml up -d
docker logs bridge-slave
```

First enrollment writes `slave.key`, `slave.csr`, `slave.crt`, `ca.crt`, and `route-signing.key`. After it succeeds, remove `BRIDGE_DEVELOPMENT_PAIRING_CODE`, change the credential mount to `- /srv/traefik-bridge/credentials:/run/bridge:ro`, and run Compose again.

### Remote Application

Create a slave-side Compose file for applications using ordinary Traefik labels:

```yaml
services:
  whoami:
    image: traefik/whoami:v1.11
    restart: unless-stopped
    labels:
      traefik.enable: "true"
      traefik.http.routers.whoami.rule: Host(`whoami.example.com`)
      traefik.http.routers.whoami.entrypoints: websecure
      traefik.http.routers.whoami.tls.certresolver: letsencrypt
      traefik.http.services.whoami.loadbalancer.server.port: "80"
    networks:
      - traefik-bridge

networks:
  traefik-bridge:
    external: true
    name: traefik-bridge
```

## Docker CLI

Use this method when Compose is unavailable. It creates the same components explicitly.

### Master: Traefik And Bridge Master

Run on the master after completing [Common Master Files](#common-master-files):

```sh
docker network create traefik-bridge 2>/dev/null || true

docker run -d --name bridge-master --restart unless-stopped \
  -p 9443:8443 -p 9445:8445 \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v /srv/traefik-bridge/master:/bridge \
  -e BRIDGE_PROXY_IMAGE=ghcr.io/esaiaswestberg/traefik-bridge-proxy:latest \
  -e BRIDGE_PROXY_CERTIFICATE_MOUNT=/srv/traefik-bridge/master:/bridge \
  -e BRIDGE_ENDPOINT_CIDRS=10.0.0.20/32 \
  -e BRIDGE_ENROLLMENT_ADDRESS=:8445 \
  ghcr.io/esaiaswestberg/traefik-bridge-master:latest

docker run -d --name traefik --restart unless-stopped \
  --network traefik-bridge -p 80:80 -p 443:443 \
  -v /var/run/docker.sock:/var/run/docker.sock:ro \
  -v /srv/traefik-bridge/master:/bridge:ro \
  -v traefik-letsencrypt:/letsencrypt \
  traefik:v3.6 \
  --entrypoints.web.address=:80 \
  --entrypoints.websecure.address=:443 \
  --providers.docker=true \
  --providers.docker.exposedbydefault=false \
  --providers.file.filename=/bridge/bridge-transport.yml \
  --certificatesresolvers.letsencrypt.acme.email=admin@example.com \
  --certificatesresolvers.letsencrypt.acme.storage=/letsencrypt/acme.json \
  --certificatesresolvers.letsencrypt.acme.httpchallenge.entrypoint=web
```

### Slave: Bridge Slave And Application

Set `BRIDGE_DEVELOPMENT_PAIRING_CODE` to the one-time code from the master logs, then run on the slave:

```sh
install -d -m 700 /srv/traefik-bridge/credentials
docker network create traefik-bridge 2>/dev/null || true

docker run -d --name bridge-slave --restart unless-stopped \
  --network traefik-bridge -p 9444:8444 \
  -v /var/run/docker.sock:/var/run/docker.sock:ro \
  -v /srv/traefik-bridge/credentials:/run/bridge \
  -e BRIDGE_SLAVE_ID=remote-host-1 \
  -e BRIDGE_MASTER_ADDRESS=10.0.0.10:9443 \
  -e BRIDGE_ENROLLMENT_ADDRESS=10.0.0.10:9445 \
  -e BRIDGE_MASTER_SERVER_NAME=bridge-master \
  -e BRIDGE_DATA_ADDRESS=10.0.0.20:9444 \
  -e BRIDGE_DATA_LISTEN_ADDRESS=:8444 \
  -e BRIDGE_DOCKER_NETWORK=traefik-bridge \
  -e BRIDGE_DEVELOPMENT_PAIRING_CODE="$BRIDGE_DEVELOPMENT_PAIRING_CODE" \
  ghcr.io/esaiaswestberg/traefik-bridge-slave:latest

docker run -d --name whoami --restart unless-stopped --network traefik-bridge \
  --label traefik.enable=true \
  --label 'traefik.http.routers.whoami.rule=Host(`whoami.example.com`)' \
  --label traefik.http.routers.whoami.entrypoints=websecure \
  --label traefik.http.routers.whoami.tls.certresolver=letsencrypt \
  --label traefik.http.services.whoami.loadbalancer.server.port=80 \
  traefik/whoami:v1.11
```

## Portainer

Portainer deploys the same Compose specifications as stacks.

### Master: Traefik And Bridge Master

1. On the master host, complete [Common Master Files](#common-master-files).
2. In Portainer, open **Stacks**, select **Add stack**, and name it `traefik-bridge-master`.
3. Paste the full master Compose specification from [Master: Traefik And Bridge Master](#master-traefik-and-bridge-master) above.
4. Set the slave LAN address directly in `BRIDGE_ENDPOINT_CIDRS`, replace the ACME email, then deploy the stack.
5. Confirm `bridge-master` is healthy before Traefik starts serving applications.

The master stack includes Traefik, its public ports, ACME storage, the Docker provider, the required file-provider transport, and the `traefik-bridge` network. If Portainer already manages Traefik in another stack, add the Traefik service configuration from the Compose method to that stack instead.

### Slave: Bridge Slave And Application

1. Retrieve the one-time pairing code from the master logs and set `BRIDGE_DEVELOPMENT_PAIRING_CODE` in the slave stack.
2. In Portainer on the slave host, create a stack named `traefik-bridge-slave` using the slave Compose specification above.
3. Replace the master address, slave address, and slave ID directly in the stack editor, then deploy it.
4. After enrollment, remove `BRIDGE_DEVELOPMENT_PAIRING_CODE`, update the credential bind mount to read-only, and redeploy the stack.
5. Create a separate application stack using the remote Whoami Compose specification, or use the same network and standard Traefik labels for an existing stack.

## Verify

After any method finishes:

```sh
curl --fail https://whoami.example.com
```

The response should identify the Whoami container running on the slave. On the master, a generated proxy should be visible:

```sh
docker ps --filter label=traefik.bridge.owner=true
docker exec bridge-master wget -q -O - http://localhost:8080/readyz
```

## Security

- Restrict TCP 9443 and 9445 on the master to slave LAN/VPN addresses.
- Restrict TCP 9444 on every slave to master LAN/VPN addresses.
- Keep `/srv/traefik-bridge/master` and slave credentials private and backed up. Limit access to master logs while a pairing code is active.
- Never expose Docker's API over TCP.
- Development OPAQUE enrollment is unaudited; use offline provisioning or an external PKI in production.
