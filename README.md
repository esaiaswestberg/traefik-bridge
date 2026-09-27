# Traefik Bridge

Traefik Bridge exposes Docker applications on remote hosts through one public Traefik instance without replacing Traefik's Docker provider.

```text
Internet -> Traefik (master host) -> generated bridge-proxy -> mTLS -> bridge-slave -> application
                                      ^
bridge-slave -------------------- mTLS control connection -----------------> bridge-master
```

The master creates local proxy containers carrying the application's normal Traefik labels. Traefik therefore discovers ordinary Docker routers and services; bridge traffic is only used behind Traefik.

## Before You Start

This guide deploys one master and one slave using published GHCR images. It requires direct private-network access between the hosts: a LAN, VPN, WireGuard, Tailscale, or equivalent.

Do not expose bridge ports to the public internet.

| Flow | Source | Destination | Port |
| --- | --- | --- | --- |
| Control and enrollment | Slave | Master | TCP 9443 and 9445 |
| Application traffic | Master | Slave | TCP 9444 |
| Public traffic | Internet | Master Traefik | TCP 80 and 443 |

The examples use these values. Replace them before running commands.

```sh
export BRIDGE_TAG=nightly
export BRIDGE_REGISTRY=ghcr.io/esaiaswestberg/traefik-bridge
export MASTER_LAN=10.0.0.10
export SLAVE_LAN=10.0.0.20
export SLAVE_ID=remote-host-1
export BRIDGE_NETWORK=bridge-apps
```

`MASTER_LAN` and `SLAVE_LAN` must be addresses reachable directly through the LAN or VPN. Do not use public addresses when a private route is available.

Install Docker Engine and the Compose plugin on both hosts before continuing. The master Docker socket is mounted read-write and is root-equivalent access to the master host. The slave socket is mounted read-only, but still exposes container metadata.

## 1. Prepare Networks

Run these commands on **both** hosts. The Docker networks are local to each host, but the names must match because the master uses the slave's selected network name when creating a proxy.

```sh
docker network create "$BRIDGE_NETWORK" 2>/dev/null || true
```

On the **master**, attach the running Traefik container to this network. Substitute your Traefik container name if necessary.

```sh
docker network connect "$BRIDGE_NETWORK" traefik 2>/dev/null || true
```

## 2. Create Master Secrets

Run on the **master**. The route-signing key is shared with every slave. The enrollment secret is only for the development OPAQUE enrollment flow described below.

```sh
install -d -m 700 /srv/traefik-bridge/master /srv/traefik-bridge/secrets /srv/traefik-bridge/config
umask 077
openssl rand -base64 48 > /srv/traefik-bridge/secrets/route.key
openssl rand -base64 48 > /srv/traefik-bridge/secrets/enrollment.key
chmod 600 /srv/traefik-bridge/secrets/*.key
```

Copy the Traefik transport definition from this repository to the master:

```sh
curl -fsSLo /srv/traefik-bridge/config/bridge-transport.yml \
  https://raw.githubusercontent.com/esaiaswestberg/traefik-bridge/main/deploy/traefik/bridge-transport.yml
```

## 3. Start The Master

Run on the **master**. Pull both the master and proxy images because the master starts proxy containers itself.

```sh
docker pull "$BRIDGE_REGISTRY-master:$BRIDGE_TAG"
docker pull "$BRIDGE_REGISTRY-proxy:$BRIDGE_TAG"

docker run -d --name bridge-master --restart unless-stopped \
  -p 9443:8443 -p 9445:8445 \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v /srv/traefik-bridge/master:/bridge \
  -v /srv/traefik-bridge/secrets/route.key:/run/secrets/route.key:ro \
  -v /srv/traefik-bridge/secrets/enrollment.key:/run/secrets/enrollment.key:ro \
  -e BRIDGE_PROXY_IMAGE="$BRIDGE_REGISTRY-proxy:$BRIDGE_TAG" \
  -e BRIDGE_ROUTE_SIGNING_KEY_FILE=/run/secrets/route.key \
  -e BRIDGE_PROXY_CERTIFICATE_MOUNT=/srv/traefik-bridge/master:/bridge \
  -e BRIDGE_ENDPOINT_CIDRS="$SLAVE_LAN/32" \
  -e BRIDGE_ENROLLMENT_ADDRESS=:8445 \
  -e BRIDGE_DEVELOPMENT_ENROLLMENT_SECRET_FILE=/run/secrets/enrollment.key \
  -e BRIDGE_ROUTE_TOKEN_LIFETIME=5m \
  -e BRIDGE_ROUTE_TOKEN_REFRESH_BEFORE=1m \
  "$BRIDGE_REGISTRY-master:$BRIDGE_TAG"
```

The master creates these files in `/srv/traefik-bridge/master`:

- `ca.crt`
- `master-client.crt`
- `master-client.key`
- private master and reconciler state files

Check that it is ready:

```sh
docker exec bridge-master wget -q -O - http://localhost:8080/readyz
```

## 4. Configure Traefik

Traefik needs the static file-provider transport named `bridge-mtls@file`, plus read-only access to the certificate files generated in the previous step.

For a Docker-managed Traefik container, add these arguments and mounts, then recreate Traefik:

```text
--providers.file.filename=/etc/traefik/bridge-transport.yml

/srv/traefik-bridge/config/bridge-transport.yml:/etc/traefik/bridge-transport.yml:ro
/srv/traefik-bridge/master:/bridge:ro
```

Keep Traefik attached to `$BRIDGE_NETWORK`. Do not enable `insecureSkipVerify`; the provided transport validates the slave mTLS certificate and supplies the master client certificate.

## 5. Prepare Slave Enrollment Material

The development enrollment flow uses RFC 9807 OPAQUE. Its Go implementation is not independently audited, so use the offline CSR workflow in [docs/installation.md](docs/installation.md) instead for production.

For development, run on the **master** to transfer the enrollment CA and the two required secrets over your authenticated private connection. Replace `slave.example.internal` with the slave's VPN/LAN SSH name or address.

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

The slave creates `slave.key`, `slave.csr`, `slave.crt`, and `ca.crt` itself during first enrollment. It refuses to overwrite existing credentials.

## 6. Start The Slave

Run on the **slave**.

```sh
docker pull "$BRIDGE_REGISTRY-slave:$BRIDGE_TAG"

docker run -d --name bridge-slave --restart unless-stopped \
  --network "$BRIDGE_NETWORK" \
  -p 9444:8444 \
  -v /var/run/docker.sock:/var/run/docker.sock:ro \
  -v /srv/traefik-bridge/credentials:/run/bridge \
  -e BRIDGE_SLAVE_ID="$SLAVE_ID" \
  -e BRIDGE_MASTER_ADDRESS="$MASTER_LAN:9443" \
  -e BRIDGE_ENROLLMENT_ADDRESS="$MASTER_LAN:9445" \
  -e BRIDGE_MASTER_SERVER_NAME=bridge-master \
  -e BRIDGE_DATA_ADDRESS="$SLAVE_LAN:9444" \
  -e BRIDGE_DATA_LISTEN_ADDRESS=:8444 \
  -e BRIDGE_DOCKER_NETWORK="$BRIDGE_NETWORK" \
  -e BRIDGE_CA_FILE=/run/bridge/ca.crt \
  -e BRIDGE_CERTIFICATE_FILE=/run/bridge/slave.crt \
  -e BRIDGE_PRIVATE_KEY_FILE=/run/bridge/slave.key \
  -e BRIDGE_ROUTE_SIGNING_KEY_FILE=/run/bridge/route-signing.key \
  -e BRIDGE_ENROLLMENT_CA_FILE=/run/bridge/enrollment-ca.crt \
  -e BRIDGE_DEVELOPMENT_ENROLLMENT_SECRET_FILE=/run/bridge/enrollment.key \
  -e BRIDGE_ENROLLMENT_CSR_FILE=/run/bridge/slave.csr \
  "$BRIDGE_REGISTRY-slave:$BRIDGE_TAG"
```

Verify enrollment and control connectivity:

```sh
docker logs bridge-slave
docker exec bridge-slave wget -q -O - http://localhost:8080/readyz
ls -l /srv/traefik-bridge/credentials
```

After first enrollment, recreate `bridge-slave` with the credential mount changed to `-v /srv/traefik-bridge/credentials:/run/bridge:ro`. The initial writable mount is required only because enrollment creates the key, CSR, CA, and certificate files.

## 7. Run A Remote Application

Run this on the **slave** to expose Whoami through the master. Replace `whoami.example.com` with a DNS name pointing at the master Traefik instance. The labels are standard Traefik Docker labels.

```sh
docker run -d --name whoami --restart unless-stopped \
  --network "$BRIDGE_NETWORK" \
  --label traefik.enable=true \
  --label 'traefik.http.routers.whoami.rule=Host(`whoami.example.com`)' \
  --label traefik.http.routers.whoami.entrypoints=websecure \
  --label traefik.http.routers.whoami.tls.certresolver=letsencrypt \
  --label traefik.http.services.whoami.loadbalancer.server.port=80 \
  traefik/whoami:v1.11
```

The master creates a `traefik-bridge-*` proxy container. Traefik reads the copied labels and routes to its assigned local listener. The proxy then uses mTLS to reach the slave.

## 8. Verify End To End

From a client that can resolve the public name:

```sh
curl --fail https://whoami.example.com
```

The response should show the hostname and IP address of the **slave** Whoami container. On the master, inspect the generated proxy and master readiness:

```sh
docker ps --filter label=traefik.bridge.owner=true
docker exec bridge-master wget -q -O - http://localhost:8080/readyz
```

## Updating Images

Use `nightly` for the newest `main` build, or pin a release tag for a stable deployment. Pull the required image, then recreate the affected container. The master must always have the matching proxy image locally.

```sh
docker pull "$BRIDGE_REGISTRY-master:<release-tag>"
docker pull "$BRIDGE_REGISTRY-proxy:<release-tag>"
docker pull "$BRIDGE_REGISTRY-slave:<release-tag>"
```

## Security Notes

- Restrict TCP 9443 and 9445 on the master to slave VPN/LAN addresses only.
- Restrict TCP 9444 on each slave to master VPN/LAN addresses only.
- Keep `/srv/traefik-bridge/secrets`, `/srv/traefik-bridge/master`, and slave credential directories private and backed up appropriately.
- The master Docker socket mount is root-equivalent. Do not expose Docker's API over TCP.
- Development OPAQUE enrollment is unaudited. Use offline certificate provisioning or an external PKI for production.

See [installation details](docs/installation.md), [security guidance](docs/security.md), and [the Compose example](examples/two-host/README.md) for additional options.
