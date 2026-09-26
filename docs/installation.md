# Installation

Traefik Bridge connects Docker workloads on remote hosts to one public Traefik instance. Host 1 runs Traefik and `bridge-master`; every remote Docker host runs `bridge-slave`. The master creates local `bridge-proxy` containers that Traefik discovers through its Docker provider.

Build the three images from the repository root:

```sh
make docker-build
```

For a two-host deployment, follow [the Compose example](../examples/two-host/README.md). Mount `deploy/traefik/bridge-transport.yml` in Traefik and provide the master-issued CA and client certificate files at the paths it names. The master control listener defaults to `8443` in its container; the example publishes it as Host 1 port `9443`. The slave data listener is `8444` in its container and is published as Host 2 port `9444`.

## Slave credentials

`bridge-slave` normally starts with provisioned mTLS and route-signing material. Configure `BRIDGE_SLAVE_ID`, `BRIDGE_CA_FILE`, `BRIDGE_CERTIFICATE_FILE`, `BRIDGE_PRIVATE_KEY_FILE`, and `BRIDGE_ROUTE_SIGNING_KEY_FILE`, along with the master and data addresses and `BRIDGE_DOCKER_NETWORK`. The slave certificate must identify the configured slave ID and be issued by the same CA used by the master.

The certificate, private key, CA, and route-signing key should be delivered by an authenticated deployment workflow and mounted read-only.

### Development-only PAKE enrollment

This is not a production provisioning method. It uses the unaudited `github.com/bytemare/opaque` v0.18.0 implementation of RFC 9807 OPAQUE.

On the master, set `BRIDGE_ENROLLMENT_ADDRESS` and `BRIDGE_DEVELOPMENT_ENROLLMENT_SECRET_FILE`. On an unprovisioned slave, set `BRIDGE_ENROLLMENT_ADDRESS`, `BRIDGE_ENROLLMENT_CA_FILE`, `BRIDGE_ENROLLMENT_CSR_FILE`, and the same `BRIDGE_DEVELOPMENT_ENROLLMENT_SECRET_FILE`. The enrollment CA must trust the master certificate. Use a unique random secret of at least 32 bytes, stored in an owner-only file. The slave generates and persists its private key and CSR locally, completes mutual OPAQUE authentication over server-authenticated TLS, then saves its certificate and CA as owner-only files. It will not enroll if any configured credential file already exists.

## Offline certificate provisioning

The repository supports a local, operator-controlled CSR workflow. It has no network listener and does not replace PAKE enrollment. Generate the private key and CSR on the slave host, keeping the key there:

```sh
umask 077
openssl ecparam -name prime256v1 -genkey -noout -out slave.key
openssl req -new -key slave.key -out slave.csr -subj '/CN=remote-host-1' -addext 'subjectAltName=DNS:slave.internal.example'
```

On Host 1, stop the master before accessing its state volume. Sign the CSR only after independently authenticating the provisioning request. The command refuses to overwrite an existing slave identity unless `-replace` is explicit; replacement immediately makes the old certificate unacceptable.

```sh
docker compose -f examples/two-host/compose.master.yml stop bridge-master
docker compose -f examples/two-host/compose.master.yml run --rm --no-deps --entrypoint bridge-provision -v /secure/requests:/requests:ro -v /secure/bridge-slave-credentials:/output bridge-master -slave-id remote-host-1 -data-host slave.internal.example -csr /requests/slave.csr -output-dir /output
docker compose -f examples/two-host/compose.master.yml up -d bridge-master
```

`-data-host` must exactly match the host in `BRIDGE_DATA_ADDRESS`; it rejects CSRs without that DNS or IP subject alternative name. Place `slave.key` and the same `route-signing.key` in `/secure/bridge-slave-credentials` on the slave host. Copy `ca.crt` and `slave.crt` from the provisioning output to that directory through an authenticated out-of-band channel.

## Master proxy credentials

`bridge-master` requires `BRIDGE_ROUTE_SIGNING_KEY_FILE`, containing the same route-signing key provisioned to each slave. It signs short-lived per-service route tokens while reconciling; generated `bridge-proxy` containers receive the tokens, never that key.

Set `BRIDGE_PROXY_CERTIFICATE_MOUNT` to a Docker `source:target` mount, for example `bridge-master-data:/bridge` or `/srv/bridge-certs:/certs`. The source must contain `ca.crt`, `master-client.crt`, and `master-client.key`, which `bridge-master` exports in its data directory. Each generated proxy mounts it read-only and receives only these file paths through its environment. A named volume is preferred where practical; an absolute source is mounted as a read-only bind. Do not put certificates or private keys in generated container environment variables.
