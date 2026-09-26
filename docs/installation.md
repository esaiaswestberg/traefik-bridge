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

## Master proxy credentials

`bridge-master` requires `BRIDGE_ROUTE_SIGNING_KEY_FILE`, containing the same route-signing key provisioned to each slave. It signs short-lived per-service route tokens while reconciling; generated `bridge-proxy` containers receive the tokens, never that key.

Set `BRIDGE_PROXY_CERTIFICATE_MOUNT` to a Docker `source:target` mount, for example `bridge-master-data:/bridge` or `/srv/bridge-certs:/certs`. The source must contain `ca.crt`, `master-client.crt`, and `master-client.key`, which `bridge-master` exports in its data directory. Each generated proxy mounts it read-only and receives only these file paths through its environment. A named volume is preferred where practical; an absolute source is mounted as a read-only bind. Do not put certificates or private keys in generated container environment variables.
