# Security

## Master Docker socket

`bridge-master` requires a direct read-write mount of the Docker socket on Host 1 to create and reconcile generated proxy containers. **This is root-equivalent access to Host 1.** Any process able to access that socket can create privileged containers, mount host paths, read host data, and effectively take over the host.

Limit Docker socket access to trusted administrators and the master container. Pin and review the master image, protect the Compose definition and its environment, and never publish the Docker API or socket to an untrusted network. A read-only socket does not make Docker API access safe enough to treat as ordinary application access.

## Credentials and transport

Keep the bridge CA private key, paired or provisioned slave private keys, and route-signing keys outside version control. Back up and restrict access to the `bridge-master-data` Docker volume, use a secret manager or protected deployment environment for manually provisioned credentials, rotate credentials when access changes, and use a private network with firewall rules for master-slave control and data traffic. Traefik should mount the volume read-only. The generated `ServersTransport` keeps TLS verification enabled.

## Development-only PAKE enrollment

An opt-in development enrollment path uses `github.com/bytemare/opaque` v0.18.0 to implement RFC 9807 OPAQUE. This dependency is explicitly **unaudited** and this feature must not be used in production. Prefer the offline CSR signer for production provisioning.

Set `BRIDGE_ENROLLMENT_ADDRESS` on the master. It logs a one-time `bridge-pair-v1...` code that contains the OPAQUE credential and a pin for the master's TLS public key. Set `BRIDGE_ENROLLMENT_ADDRESS` and `BRIDGE_DEVELOPMENT_PAIRING_CODE` on a credential-less slave. The pin authenticates the master TLS certificate; OPAQUE then mutually authenticates both parties before the master signs a CSR. Treat the pairing code as a secret: limit access to master logs, provide it only through an authenticated channel, and remove it from the slave configuration after enrollment. The code rotates after a successful enrollment.

The slave automatically enrolls only when its configured credential files are absent. Enrollment refuses to overwrite any existing credential and writes the issued certificate, CA, private key, CSR, and route-signing key with owner-only permissions. Make the credentials mount read-only after enrollment.
