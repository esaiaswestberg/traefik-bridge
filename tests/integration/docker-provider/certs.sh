#!/bin/sh
set -eu

apk add --no-cache openssl >/dev/null
openssl req -x509 -newkey rsa:2048 -nodes -days 1 \
  -subj /CN=traefik-bridge-spike-ca \
  -keyout /certs/ca.key -out /certs/ca.crt >/dev/null 2>&1

openssl req -newkey rsa:2048 -nodes -subj /CN=backend \
  -keyout /certs/backend.key -out /certs/backend.csr >/dev/null 2>&1
printf 'subjectAltName=DNS:backend\nextendedKeyUsage=serverAuth\n' >/certs/backend.ext
openssl x509 -req -days 1 -CA /certs/ca.crt -CAkey /certs/ca.key -CAcreateserial \
  -in /certs/backend.csr -out /certs/backend.crt -extfile /certs/backend.ext >/dev/null 2>&1

openssl req -newkey rsa:2048 -nodes -subj /CN=traefik \
  -keyout /certs/client.key -out /certs/client.csr >/dev/null 2>&1
printf 'extendedKeyUsage=clientAuth\n' >/certs/client.ext
openssl x509 -req -days 1 -CA /certs/ca.crt -CAkey /certs/ca.key -CAcreateserial \
  -in /certs/client.csr -out /certs/client.crt -extfile /certs/client.ext >/dev/null 2>&1
