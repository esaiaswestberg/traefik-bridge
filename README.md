# Traefik Bridge

Traefik Bridge exposes Docker applications on remote hosts through one public Traefik instance while keeping Traefik's standard Docker provider.

```text
Internet -> Traefik (master) -> generated bridge-proxy -> mTLS -> bridge-slave -> application
                                   ^
bridge-slave ----------------- mTLS control connection ------------------> bridge-master
```

For complete deployment instructions, including Docker Compose, Docker CLI, and Portainer methods, see the [deployment guide](docs/setup.md).

## Requirements

- Docker Engine and the Compose plugin on both hosts.
- Direct LAN or VPN connectivity between master and slave. WireGuard, Tailscale, or a private VLAN are suitable.
- A public Traefik instance on the master host.
- DNS for application names pointing to Traefik on the master host.

Do not route bridge traffic through the public internet. Restrict these private-network flows with your firewall:

| Flow | Source | Destination | Port |
| --- | --- | --- | --- |
| Control and development enrollment | Slave | Master | TCP 9443, 9445 |
| Application traffic | Master | Slave | TCP 9444 |
| Public traffic | Internet | Master Traefik | TCP 80, 443 |

The master Docker socket is mounted read-write and is root-equivalent access to the master host. The slave socket is mounted read-only, but exposes container metadata.

## 1. Set Host Values

Set these values on both hosts. Use VPN/LAN addresses, not public addresses. All other values in this guide are static and used literally, including the image names and the `traefik-bridge` Docker network.

```sh
export MASTER_LAN=10.0.0.10
export SLAVE_LAN=10.0.0.20
export SLAVE_ID=remote-host-1
```

Create the named network on **both** hosts. Its name must be identical because the master creates generated proxies on the network selected by the slave.

```sh
docker network create "traefik-bridge" 2>/dev/null || true
```

Pull the published images. If GHCR packages are private, log in first with a GitHub token that has `read:packages`.

```sh
docker pull ghcr.io/esaiaswestberg/traefik-bridge-master:latest
docker pull ghcr.io/esaiaswestberg/traefik-bridge-proxy:latest
# Run this on the slave too.
docker pull ghcr.io/esaiaswestberg/traefik-bridge-slave:latest
```

## 2. Configure The Master

Run these commands on the **master**.

```sh
install -d -m 700 /srv/traefik-bridge/master /srv/traefik-bridge/secrets /srv/traefik-bridge/config
umask 077
openssl rand -base64 48 > /srv/traefik-bridge/secrets/route.key
openssl rand -base64 48 > /srv/traefik-bridge/secrets/enrollment.key
chmod 600 /srv/traefik-bridge/secrets/*.key

curl -fsSLo /srv/traefik-bridge/config/bridge-transport.yml \
  https://raw.githubusercontent.com/esaiaswestberg/traefik-bridge/main/deploy/traefik/bridge-transport.yml
```

Create `/srv/traefik-bridge/compose.master.yml`:

```yaml
services:
  bridge-master:
    image: ghcr.io/esaiaswestberg/traefik-bridge-master:latest
    container_name: bridge-master
    restart: unless-stopped
    ports:
      - "9443:8443"
      - "9445:8445"
    environment:
      BRIDGE_PROXY_IMAGE: ghcr.io/esaiaswestberg/traefik-bridge-proxy:latest
      BRIDGE_ROUTE_SIGNING_KEY_FILE: /run/secrets/route.key
      BRIDGE_PROXY_CERTIFICATE_MOUNT: /srv/traefik-bridge/master:/bridge
      BRIDGE_ENDPOINT_CIDRS: ${SLAVE_LAN}/32
      BRIDGE_ENROLLMENT_ADDRESS: :8445
      BRIDGE_DEVELOPMENT_ENROLLMENT_SECRET_FILE: /run/secrets/enrollment.key
      BRIDGE_ROUTE_TOKEN_LIFETIME: 5m
      BRIDGE_ROUTE_TOKEN_REFRESH_BEFORE: 1m
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      - /srv/traefik-bridge/master:/bridge
      - /srv/traefik-bridge/secrets/route.key:/run/secrets/route.key:ro
      - /srv/traefik-bridge/secrets/enrollment.key:/run/secrets/enrollment.key:ro
```

Create `/srv/traefik-bridge/.env`:

```dotenv
SLAVE_LAN=10.0.0.20
```

Start the master:

```sh
docker compose --env-file /srv/traefik-bridge/.env \
  -f /srv/traefik-bridge/compose.master.yml up -d
docker exec bridge-master wget -q -O - http://localhost:8080/readyz
```

The master initializes its CA and exports `ca.crt`, `master-client.crt`, and `master-client.key` under `/srv/traefik-bridge/master`.

## 3. Attach Traefik To The Bridge

Run this on the **master** after the master is healthy:

```sh
docker network connect "traefik-bridge" traefik 2>/dev/null || true
```

Add the following to the existing Traefik service in its Compose file, then recreate Traefik. The names below assume the service is called `traefik`.

```yaml
services:
  traefik:
    command:
      - --providers.file.filename=/etc/traefik/bridge-transport.yml
    volumes:
      - /srv/traefik-bridge/config/bridge-transport.yml:/etc/traefik/bridge-transport.yml:ro
      - /srv/traefik-bridge/master:/bridge:ro
    networks:
      - traefik-bridge

networks:
  traefik-bridge:
    external: true
    name: traefik-bridge
```

Preserve your existing Traefik command arguments, socket mount, public ports, ACME configuration, and networks. The file-provider transport is named `bridge-mtls@file`; generated services reference it automatically. Do not set `insecureSkipVerify`.

## 4. Prepare The Slave

The automatic enrollment flow is for development only because its OPAQUE implementation is not independently audited. For production, use the offline CSR workflow in [docs/installation.md](docs/installation.md) or an external PKI.

For development, transfer the enrollment CA and shared secrets from the **master** to the **slave** through your authenticated private connection. Replace `slave.example.internal` with the slave's VPN/LAN SSH address.

```sh
ssh root@slave.example.internal 'install -d -m 700 /srv/traefik-bridge/credentials'

cat /srv/traefik-bridge/secrets/route.key | \
  ssh root@slave.example.internal 'umask 077; cat > /srv/traefik-bridge/credentials/route-signing.key'
cat /srv/traefik-bridge/secrets/enrollment.key | \
  ssh root@slave.example.internal 'umask 077; cat > /srv/traefik-bridge/credentials/enrollment.key'
cat /srv/traefik-bridge/master/ca.crt | \
  ssh root@slave.example.internal 'umask 077; cat > /srv/traefik-bridge/credentials/enrollment-ca.crt'
ssh root@slave.example.internal 'chmod 600 /srv/traefik-bridge/credentials/*'
```

On the **slave**, create `/srv/traefik-bridge/compose.slave.yml`:

```yaml
services:
  bridge-slave:
    image: ghcr.io/esaiaswestberg/traefik-bridge-slave:latest
    container_name: bridge-slave
    restart: unless-stopped
    ports:
      - "9444:8444"
    environment:
      BRIDGE_SLAVE_ID: ${SLAVE_ID}
      BRIDGE_MASTER_ADDRESS: ${MASTER_LAN}:9443
      BRIDGE_ENROLLMENT_ADDRESS: ${MASTER_LAN}:9445
      BRIDGE_MASTER_SERVER_NAME: bridge-master
      BRIDGE_DATA_ADDRESS: ${SLAVE_LAN}:9444
      BRIDGE_DATA_LISTEN_ADDRESS: :8444
      BRIDGE_DOCKER_NETWORK: traefik-bridge
      BRIDGE_CA_FILE: /run/bridge/ca.crt
      BRIDGE_CERTIFICATE_FILE: /run/bridge/slave.crt
      BRIDGE_PRIVATE_KEY_FILE: /run/bridge/slave.key
      BRIDGE_ROUTE_SIGNING_KEY_FILE: /run/bridge/route-signing.key
      BRIDGE_ENROLLMENT_CA_FILE: /run/bridge/enrollment-ca.crt
      BRIDGE_DEVELOPMENT_ENROLLMENT_SECRET_FILE: /run/bridge/enrollment.key
      BRIDGE_ENROLLMENT_CSR_FILE: /run/bridge/slave.csr
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock:ro
      # Writable only for first enrollment; make this read-only afterward.
      - /srv/traefik-bridge/credentials:/run/bridge
    networks:
      - traefik-bridge

networks:
  traefik-bridge:
    external: true
    name: traefik-bridge
```

Create `/srv/traefik-bridge/.env` on the **slave**:

```dotenv
MASTER_LAN=10.0.0.10
SLAVE_LAN=10.0.0.20
SLAVE_ID=remote-host-1
```

Start the slave:

```sh
docker compose --env-file /srv/traefik-bridge/.env \
  -f /srv/traefik-bridge/compose.slave.yml up -d
docker logs bridge-slave
docker exec bridge-slave wget -q -O - http://localhost:8080/readyz
```

First enrollment creates `slave.key`, `slave.csr`, `slave.crt`, and `ca.crt`. Once the slave is healthy, change the credential volume to `- /srv/traefik-bridge/credentials:/run/bridge:ro` and run `docker compose ... up -d` again.

## 5. Publish A Remote Application

Create `/srv/traefik-bridge/compose.whoami.yml` on the **slave**. Replace `whoami.example.com` with a name that resolves to the master Traefik instance.

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

Start it on the slave:

```sh
docker compose -f /srv/traefik-bridge/compose.whoami.yml up -d
```

The master detects the labels, creates a local `traefik-bridge-*` proxy, and Traefik discovers its normal Docker labels.

## 6. Verify

From a public client:

```sh
curl --fail https://whoami.example.com
```

The response should identify the Whoami container on the slave. On the master, verify the generated proxy and readiness:

```sh
docker ps --filter label=traefik.bridge.owner=true
docker exec bridge-master wget -q -O - http://localhost:8080/readyz
```

## Updating

The Compose files use `latest` for convenience. For a stable deployment, replace `:latest` with a release tag in every image reference on both hosts. The master and generated proxy images must always use the same tag.

```sh
# Example: pin all images to v0.1.0 on both hosts.
sed -i 's/:latest/:v0.1.0/' /srv/traefik-bridge/compose.master.yml
docker compose --env-file /srv/traefik-bridge/.env \
  -f /srv/traefik-bridge/compose.master.yml pull
docker compose --env-file /srv/traefik-bridge/.env \
  -f /srv/traefik-bridge/compose.master.yml up -d
```

Apply the same commands to `compose.slave.yml` on the slave.

## Security

- Allow TCP 9443 and 9445 on the master only from slave LAN/VPN addresses.
- Allow TCP 9444 on every slave only from master LAN/VPN addresses.
- Keep `/srv/traefik-bridge/secrets`, `/srv/traefik-bridge/master`, and slave credentials private and backed up.
- Never expose Docker's API over TCP.
- Development OPAQUE enrollment is unaudited; use offline provisioning or an external PKI in production.

See [installation details](docs/installation.md), [security guidance](docs/security.md), and [the Compose example](examples/two-host/README.md) for additional options.
