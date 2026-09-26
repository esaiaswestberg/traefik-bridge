# Installation

Traefik Bridge connects Docker workloads on remote hosts to one public Traefik instance. Host 1 runs Traefik and `bridge-master`; every remote Docker host runs `bridge-slave`. The master creates local `bridge-proxy` containers that Traefik discovers through its Docker provider.

Build the three images from the repository root:

```sh
make docker-build
```

For a two-host deployment, follow [the Compose example](../examples/two-host/README.md). Supply `BRIDGE_CLUSTER_SECRET` through a secret manager or the deployment environment, never a committed Compose or `.env` file. Mount `deploy/traefik/bridge-transport.yml` in Traefik and provide the master-issued CA and client certificate files at the paths it names.

The checked-in Compose files are valid configuration examples, but the current command binaries are placeholders and exit after logging that they are not configured. They do not yet open listeners, enroll peers, or reconcile Docker containers.
