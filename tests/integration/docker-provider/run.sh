#!/bin/sh
set -eu

case "${COMPOSE:-}" in
"")
  if docker compose version >/dev/null 2>&1; then
    COMPOSE="docker compose"
  elif command -v docker-compose >/dev/null 2>&1; then
    COMPOSE="docker-compose"
  else
    printf '%s\n' 'SKIP: Docker Compose is not installed.'
    exit 0
  fi
  ;;
esac

if ! docker info >/dev/null 2>&1; then
  printf '%s\n' 'SKIP: Docker daemon is not available.'
  exit 0
fi

root=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
project="traefik-bridge-spike-$$"
cleanup() {
  $COMPOSE -p "$project" -f "$root/compose.yaml" down --volumes --remove-orphans >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM

$COMPOSE -p "$project" -f "$root/compose.yaml" up --build --wait --wait-timeout 90

attempt=0
while ! response=$(curl --silent --show-error --fail --max-time 2 -H 'Host: spike.local' http://127.0.0.1:18080/ 2>/dev/null); do
  attempt=$((attempt + 1))
  if [ "$attempt" -ge 30 ]; then
    printf '%s\n' 'FAIL: Traefik did not route to the mTLS backend.' >&2
    exit 1
  fi
  sleep 1
done

test "$response" = 'cross-container mTLS service reached'
service=$($COMPOSE -p "$project" -f "$root/compose.yaml" exec -T traefik wget -qO- http://127.0.0.1:8080/api/http/services/bridge-service@docker)
printf '%s' "$service" | grep -q '"path":"/health/bridge-preserved"'
printf '%s' "$service" | grep -q '"serverStatus".*"UP"'
printf '%s\n' 'PASS: cross-container router/service reference, file-provider mTLS transport, and health-check path validated.'
