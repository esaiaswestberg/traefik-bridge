# Security

## Master Docker socket

`bridge-master` requires a direct read-write mount of the Docker socket on Host 1 to create and reconcile generated proxy containers. **This is root-equivalent access to Host 1.** Any process able to access that socket can create privileged containers, mount host paths, read host data, and effectively take over the host.

Limit Docker socket access to trusted administrators and the master container. Pin and review the master image, protect the Compose definition and its environment, and never publish the Docker API or socket to an untrusted network. A read-only socket does not make Docker API access safe enough to treat as ordinary application access.

## Credentials and transport

Keep the bridge CA private key, provisioned slave private keys, and route-signing keys outside version control. Use a secret manager or protected deployment environment, rotate credentials when access changes, and use a private network with firewall rules for master-slave control and data traffic. Traefik should mount only the CA and master client certificate material it needs, read-only. The static `ServersTransport` template keeps TLS verification enabled.

Slave certificate enrollment is not available. The supported offline CSR signer is a local command that requires access to the private master state and is intended for authenticated operator use only. Do not add a shared-secret or token-based enrollment path; certificate issuance must remain behind an authenticated provisioning workflow until an audited PAKE implementation is available.
