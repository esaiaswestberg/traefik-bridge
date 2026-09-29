# Installation

Traefik Bridge connects Docker workloads on remote hosts to one public Traefik instance. Host 1 runs Traefik and `bridge-master`; every remote Docker host runs `bridge-slave`. The master creates local `bridge-proxy` containers that Traefik discovers through its Docker provider.

Build the three images from the repository root:

```sh
make docker-build
```

For a two-host deployment, follow [the Compose example](../examples/two-host/README.md). Mount the persistent `bridge-master-data` Docker volume at `/bridge` in Traefik and set `--providers.file.filename=/bridge/bridge-transport.yml`. The master generates this transport file and the TLS material it references on every start. The master control listener defaults to `8443` in its container; the example publishes it as Host 1 port `9443`. The slave data listener is `8444` in its container and is published as Host 2 port `9444`.

## Slave credentials

`bridge-slave` normally starts with paired or provisioned mTLS and route-signing material. Configure `BRIDGE_SLAVE_ID` with the master and data addresses and `BRIDGE_DOCKER_NETWORK`. Credential paths default to `/run/bridge/ca.crt`, `/run/bridge/slave.crt`, `/run/bridge/slave.key`, `/run/bridge/route-signing.key`, and `/run/bridge/slave.csr`; configure explicit paths for an existing provisioning workflow. The slave certificate must identify the configured slave ID and be issued by the same CA used by the master.

For offline provisioning, deliver the certificate, private key, CA, and route-signing key through an authenticated deployment workflow and mount them read-only.

### Development-only PAKE enrollment

This is not a production provisioning method. It uses the unaudited `github.com/bytemare/opaque` v0.18.0 implementation of RFC 9807 OPAQUE.

On the master, set `BRIDGE_ENROLLMENT_ADDRESS`; it logs a one-time pairing code. On an unprovisioned slave, set `BRIDGE_ENROLLMENT_ADDRESS` and `BRIDGE_DEVELOPMENT_PAIRING_CODE` along with its identity, addresses, and network. The code carries an OPAQUE credential and a pin for the master's TLS public key, so no bootstrap CA, route-signing key, or enrollment secret transfer is required. The slave generates and persists its private key, CSR, certificate, CA, and route-signing key as owner-only files after mutually authenticated OPAQUE enrollment. It will not enroll if any configured credential file already exists. A successful enrollment rotates the code; retrieve the newly logged code before enrolling another slave. Remove the code from the slave configuration and make the credential mount read-only after enrollment. The legacy secret, bootstrap-CA, and explicit route-key-file flow remains supported for existing deployments.

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

`-data-host` must exactly match the host in `BRIDGE_DATA_ADDRESS`; it rejects CSRs without that DNS or IP subject alternative name. Copy `slave.key`, `ca.crt`, and `slave.crt` to the slave through an authenticated out-of-band channel. For this manual workflow, configure `BRIDGE_ROUTE_SIGNING_KEY_FILE` on the master and distribute that same key as `route-signing.key` to the slave through the same protected channel.

## Master proxy credentials

`bridge-master` generates and stores a random route-signing key in its private state on first initialization. It signs short-lived per-service route tokens while reconciling; generated `bridge-proxy` containers receive the tokens, never that key. `BRIDGE_ROUTE_SIGNING_KEY_FILE` is an optional override for legacy deployments and is delivered to newly paired slaves. `BRIDGE_ROUTE_TOKEN_LIFETIME` defaults to five minutes and `BRIDGE_ROUTE_TOKEN_REFRESH_BEFORE` defaults to one minute. The refresh window must be positive and shorter than the lifetime.

The master records accepted snapshots and proxy expiry timestamps in its private data directory before changing Docker. It restores that state after restart and refreshes only proxies approaching expiry. The state never contains route tokens.

Set `BRIDGE_PROXY_CERTIFICATE_MOUNT` to a Docker `source:target` mount, for example `bridge-master-data:/bridge` or `/srv/bridge-certs:/certs`. The source must contain `ca.crt`, `master-client.crt`, and `master-client.key`, which `bridge-master` exports in its data directory. Each generated proxy mounts it read-only and receives only these file paths through its environment. A named volume is preferred where practical; an absolute source is mounted as a read-only bind. Do not put certificates or private keys in generated container environment variables.
