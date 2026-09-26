# Traefik Bridge

Traefik Bridge exposes Docker applications on remote hosts through one public Traefik deployment. It keeps Traefik's standard Docker provider: the master creates local bridge-proxy containers that carry normal Traefik labels, while each slave discovers labeled applications on its own Docker host.

```text
Internet -> Traefik (Host 1) -> bridge-proxy -> mTLS -> bridge-slave (Host 2) -> application
                         ^
                         bridge-master <- mTLS control connection <- bridge-slave
```

`bridge-master` runs with Traefik on Host 1. `bridge-slave` runs on every remote Docker host. `bridge-proxy` is the image master uses for generated, Traefik-facing containers.

## Build and run

```sh
make check
make docker-build
export BRIDGE_CLUSTER_SECRET="$(openssl rand -hex 32)"
docker compose -f examples/two-host/compose.master.yml up -d
```

Start each remote slave using `examples/two-host/compose.slave.yml`; see its [two-host guide](examples/two-host/README.md) for required addresses, image builds, and validation commands.

The current command binaries are intentionally placeholders and log before exiting. The Dockerfiles and Compose files establish the deployment contract for the control, proxy, and Docker reconciliation wiring that follows.

Read [installation](docs/installation.md) and [security guidance](docs/security.md) before deploying. In particular, the master requires a direct read-write Docker socket mount, which is root-equivalent access to Host 1.
