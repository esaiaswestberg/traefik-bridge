# Installation

Traefik Bridge connects Docker workloads on remote hosts to one public Traefik instance. Host 1 runs Traefik and `bridge-master`; every remote Docker host runs `bridge-slave`. The master creates local `bridge-proxy` containers that Traefik discovers through its Docker provider.

Build the three images from the repository root:

```sh
make docker-build
```

For a two-host deployment, follow [the Compose example](../examples/two-host/README.md). Mount `deploy/traefik/bridge-transport.yml` in Traefik and provide the master-issued CA and client certificate files at the paths it names.

## Slave credentials

`bridge-slave` only starts with provisioned mTLS and route-signing material. Configure `BRIDGE_SLAVE_ID`, `BRIDGE_CA_FILE`, `BRIDGE_CERTIFICATE_FILE`, `BRIDGE_PRIVATE_KEY_FILE`, and `BRIDGE_ROUTE_SIGNING_KEY_FILE`, along with the master and data addresses and `BRIDGE_DOCKER_NETWORK`. The slave certificate must identify the configured slave ID and be issued by the same CA used by the master.

The certificate, private key, CA, and route-signing key must be delivered by an authenticated deployment workflow and mounted read-only. Enrollment is not implemented: in particular, this project does not accept a shared secret, token, or unauthenticated request to issue a slave certificate until an audited PAKE enrollment flow exists.
