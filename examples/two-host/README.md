# Two-host Compose example

Run `compose.master.yml` on the public host and `compose.slave.yml` on each remote Docker host.

## Prerequisites

- Docker Engine and the Compose plugin on both hosts.
- Private network reachability from each slave to the master's control endpoint and from Host 1 to each slave's data endpoint.
- Provisioned slave credentials: `ca.crt`, `slave.crt`, `slave.key`, and `route-signing.key` in a protected directory.
- The master creates `ca.crt`, `master-client.crt`, and `master-client.key` in `bridge-master-data` on first start. Traefik reads them from `/bridge`.
- An existing Docker network on Host 1 named `BRIDGE_DOCKER_NETWORK`. Traefik and generated proxies join this network.

## Host 1: Traefik and master

From the repository root, create the local proxy network and build the master and generated-proxy images:

```sh
export BRIDGE_DOCKER_NETWORK='bridge-proxy'
docker network create "$BRIDGE_DOCKER_NETWORK"
umask 077
mkdir -p examples/two-host/secrets
openssl rand -base64 32 > examples/two-host/secrets/route.key
docker compose -f examples/two-host/compose.master.yml build
docker compose -f examples/two-host/compose.master.yml up -d
```

The `bridge-proxy-image` service has the `image-build` profile, so it is built but not started. Master will use `traefik-bridge-proxy:local` when it begins reconciling generated proxies. Distribute `secrets/route.key` to each slave as `route-signing.key` through an authenticated secret-delivery channel. This ignored secret must remain available on Host 1 for master restarts.

## Host 2: slave

Set the provisioned slave identity, credential directory, Docker network, and private addresses, then start the slave:

```sh
export BRIDGE_SLAVE_ID='remote-host-1'
export BRIDGE_SLAVE_CREDENTIALS_DIR='/secure/bridge-slave-credentials'
export BRIDGE_DOCKER_NETWORK='apps'
export BRIDGE_MASTER_ADDRESS='master.internal.example:9443'
export BRIDGE_MASTER_SERVER_NAME='bridge-master'
export BRIDGE_DATA_ADDRESS='slave.internal.example:9444'
docker compose -f examples/two-host/compose.slave.yml up -d --build
```

Validate either Compose file without storing a secret by supplying temporary values only to the command:

```sh
BRIDGE_SLAVE_ID=temporary BRIDGE_SLAVE_CREDENTIALS_DIR=/tmp/bridge BRIDGE_DOCKER_NETWORK=bridge-proxy BRIDGE_MASTER_ADDRESS=master.internal:9443 BRIDGE_DATA_ADDRESS=slave.internal:9444 docker compose -f examples/two-host/compose.slave.yml config
BRIDGE_DOCKER_NETWORK=bridge-proxy docker compose -f examples/two-host/compose.master.yml config
```

## Security

`bridge-master` mounts `/var/run/docker.sock` read-write. Direct access to the master Docker socket is root-equivalent access to Host 1: a process that can use it can start privileged containers, mount host filesystems, and take control of the host. Restrict who can modify the master Compose file, its image, its environment, and Docker access on Host 1. Do not expose the Docker socket over TCP.

The slave socket is read-only to support container discovery. Treat it as sensitive as well: Docker metadata may disclose service configuration. Use a private network for bridge traffic, protect provisioned credentials, and mount credential volumes read-only. Use the offline CSR process in the installation guide to issue credentials; it requires an authenticated operator and does not expose enrollment over the network.
