# Two-host Compose example

Run `compose.master.yml` on the public host and `compose.slave.yml` on each remote Docker host. The examples are deployment scaffolding for the configuration implemented in later phases; the current binaries only log that they are not configured and exit.

## Prerequisites

- Docker Engine and the Compose plugin on both hosts.
- Private network reachability from each slave to the master's control endpoint and from Host 1 to each slave's data endpoint.
- A single high-entropy cluster secret supplied outside the repository.
- Certificates at `bridge-master-data/certs` before starting Traefik. The intended master paths are `ca.crt`, `master-client.crt`, and `master-client.key`.

## Host 1: Traefik and master

From the repository root, set the shared secret in the shell and build the master and generated-proxy images:

```sh
export BRIDGE_CLUSTER_SECRET="$(openssl rand -hex 32)"
docker compose -f examples/two-host/compose.master.yml build
docker compose -f examples/two-host/compose.master.yml up -d
```

The `bridge-proxy-image` service has the `image-build` profile, so it is built but not started. Master will use `traefik-bridge-proxy:local` when it begins reconciling generated proxies.

## Host 2: slave

Set the same secret and private addresses, then start the slave:

```sh
export BRIDGE_CLUSTER_SECRET='the-same-secret-from-host-1'
export BRIDGE_MASTER_ADDRESS='master.internal.example:9443'
export BRIDGE_DATA_ADDRESS='slave.internal.example:9444'
docker compose -f examples/two-host/compose.slave.yml up -d --build
```

Validate either Compose file without storing a secret by supplying temporary values only to the command:

```sh
BRIDGE_CLUSTER_SECRET=temporary BRIDGE_MASTER_ADDRESS=master.internal:9443 BRIDGE_DATA_ADDRESS=slave.internal:9444 docker compose -f examples/two-host/compose.slave.yml config
BRIDGE_CLUSTER_SECRET=temporary docker compose -f examples/two-host/compose.master.yml config
```

## Security

`bridge-master` mounts `/var/run/docker.sock` read-write. Direct access to the master Docker socket is root-equivalent access to Host 1: a process that can use it can start privileged containers, mount host filesystems, and take control of the host. Restrict who can modify the master Compose file, its image, its environment, and Docker access on Host 1. Do not expose the Docker socket over TCP.

The slave socket is read-only to support container discovery. Treat it as sensitive as well: Docker metadata may disclose service configuration. Use a private network for bridge traffic, protect the shared secret and issued certificates, and mount the Traefik certificate volume read-only.
