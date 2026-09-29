# Two-host Compose example

Run `compose.master.yml` on the public host and `compose.slave.yml` on each remote Docker host.

## Prerequisites

- Docker Engine and the Compose plugin on both hosts.
- Private network reachability from each slave to the master's control endpoint and from Host 1 to each slave's data endpoint.
- A writable, protected credential directory on each unpaired slave.
- The master creates `ca.crt`, `master-client.crt`, and `master-client.key` in `bridge-master-data` on first start. Traefik reads them from `/bridge`.
- An existing Docker network on Host 1 named `BRIDGE_DOCKER_NETWORK`. Traefik and generated proxies join this network.

## Host 1: Traefik and master

From the repository root, create the local proxy network and build the master and generated-proxy images:

```sh
export BRIDGE_DOCKER_NETWORK='bridge-proxy'
docker network create "$BRIDGE_DOCKER_NETWORK"
docker compose -f examples/two-host/compose.master.yml build
docker compose -f examples/two-host/compose.master.yml up -d
```

The `bridge-proxy-image` service has the `image-build` profile, so it is built but not started. Master will use `traefik-bridge-proxy:local` when it begins reconciling generated proxies. Retrieve the one-time pairing code from the master logs before enrolling a slave:

```sh
docker compose -f examples/two-host/compose.master.yml logs bridge-master | grep pairing_code
```

The code is consumed after a successful enrollment. Retrieve the next logged code before enrolling another slave.

## Host 2: slave

Set the slave identity, writable credential directory, one-time pairing code, Docker network, and private addresses, then start the slave:

```sh
export BRIDGE_SLAVE_ID='remote-host-1'
export BRIDGE_SLAVE_CREDENTIALS_DIR='/secure/bridge-slave-credentials'
export BRIDGE_DOCKER_NETWORK='apps'
export BRIDGE_MASTER_ADDRESS='master.internal.example:9443'
export BRIDGE_ENROLLMENT_ADDRESS='master.internal.example:9445'
export BRIDGE_MASTER_SERVER_NAME='bridge-master'
export BRIDGE_DATA_ADDRESS='slave.internal.example:9444'
export BRIDGE_DEVELOPMENT_PAIRING_CODE='bridge-pair-v1.example'
docker compose -f examples/two-host/compose.slave.yml up -d --build
```

After enrollment, remove `BRIDGE_DEVELOPMENT_PAIRING_CODE`, change the credentials bind mount to read-only, and recreate the slave. Validate either Compose file with temporary values:

```sh
BRIDGE_SLAVE_ID=temporary BRIDGE_SLAVE_CREDENTIALS_DIR=/tmp/bridge BRIDGE_DOCKER_NETWORK=bridge-proxy BRIDGE_MASTER_ADDRESS=master.internal:9443 BRIDGE_ENROLLMENT_ADDRESS=master.internal:9445 BRIDGE_DATA_ADDRESS=slave.internal:9444 BRIDGE_DEVELOPMENT_PAIRING_CODE=bridge-pair-v1.example docker compose -f examples/two-host/compose.slave.yml config
BRIDGE_DOCKER_NETWORK=bridge-proxy docker compose -f examples/two-host/compose.master.yml config
```

## Security

`bridge-master` mounts `/var/run/docker.sock` read-write. Direct access to the master Docker socket is root-equivalent access to Host 1: a process that can use it can start privileged containers, mount host filesystems, and take control of the host. Restrict who can modify the master Compose file, its image, its environment, and Docker access on Host 1. Do not expose the Docker socket over TCP.

The slave socket is read-only to support container discovery. Treat it as sensitive as well: Docker metadata may disclose service configuration. Use a private network for bridge traffic, protect paired credentials, and mount credential volumes read-only after enrollment. The pairing code is a secret and is development-only because its OPAQUE dependency is unaudited. Use the offline CSR process in the installation guide to issue production credentials.
