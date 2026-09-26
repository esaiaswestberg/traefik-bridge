# Security

## Master Docker socket

`bridge-master` requires a direct read-write mount of the Docker socket on Host 1 to create and reconcile generated proxy containers. **This is root-equivalent access to Host 1.** Any process able to access that socket can create privileged containers, mount host paths, read host data, and effectively take over the host.

Limit Docker socket access to trusted administrators and the master container. Pin and review the master image, protect the Compose definition and its environment, and never publish the Docker API or socket to an untrusted network. A read-only socket does not make Docker API access safe enough to treat as ordinary application access.

## Credentials and transport

Keep the bridge CA private key, provisioned slave private keys, and route-signing keys outside version control. Use a secret manager or protected deployment environment, rotate credentials when access changes, and use a private network with firewall rules for master-slave control and data traffic. Traefik should mount only the CA and master client certificate material it needs, read-only. The static `ServersTransport` template keeps TLS verification enabled.

## Development-only PAKE enrollment

An opt-in development enrollment path uses `github.com/bytemare/opaque` v0.18.0 to implement RFC 9807 OPAQUE. This dependency is explicitly **unaudited** and this feature must not be used in production. Prefer the offline CSR signer for production provisioning.

Set `BRIDGE_ENROLLMENT_ADDRESS` and `BRIDGE_DEVELOPMENT_ENROLLMENT_SECRET_FILE` on the master, and set `BRIDGE_ENROLLMENT_ADDRESS`, `BRIDGE_ENROLLMENT_CA_FILE`, `BRIDGE_ENROLLMENT_CSR_FILE`, and `BRIDGE_DEVELOPMENT_ENROLLMENT_SECRET_FILE` on a credential-less slave. The secret file must contain at least 32 bytes of high-entropy random data and be readable only by the deployment owner. The bootstrap CA authenticates the master TLS certificate; OPAQUE then mutually authenticates both parties before the master signs a CSR. The secret is never sent on the wire or stored in master state.

The slave automatically enrolls only when all three configured credential files are absent. Enrollment refuses to overwrite any existing credential and writes the issued certificate, CA, and private key with owner-only permissions.
