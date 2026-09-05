#!/bin/sh
# Provision a new device NATS + client, entirely without touching nats-remote.
#
# Mints a User JWT under the (already-trusted) DEVICES account, restricted to
# device.<id>.outgoing.data / device.<id>.incoming.data, generates its .creds
# file, writes the device's nats-server config + a docker-compose override,
# and brings the two new containers up. nats-remote is never restarted.
#
# Usage: scripts/provision-device.sh <device-id>
# Example: scripts/provision-device.sh a4
set -e

DEVICE_ID="$1"
if [ -z "$DEVICE_ID" ]; then
  echo "usage: $0 <device-id>" >&2
  exit 1
fi

cd "$(dirname "$0")/.."

if [ ! -f nats-config/resolver.conf ]; then
  echo "nats-config/resolver.conf not found - run scripts/setup-operator.sh first" >&2
  exit 1
fi

docker build -q -t multi-nats-provisioning ./provisioning >/dev/null

mkdir -p "nsc-data/creds"

LOCAL_PASS=$(head -c 12 /dev/urandom | od -An -tx1 | tr -d ' \n')

docker run --rm \
  --user "$(id -u):$(id -g)" \
  -v "$(pwd)/nsc-data:/work/nsc-data" \
  -e DEVICE_ID="$DEVICE_ID" \
  multi-nats-provisioning -c '
    set -e
    D=--all-dirs=/work/nsc-data/store
    nsc add user -a DEVICES $D "$DEVICE_ID" \
      --allow-pub "device.$DEVICE_ID.outgoing.data" \
      --allow-sub "device.$DEVICE_ID.incoming.data"
    mkdir -p /work/nsc-data/creds
    nsc generate creds -a DEVICES $D -n "$DEVICE_ID" -o "/work/nsc-data/creds/$DEVICE_ID.creds"
  '

mkdir -p nats-config
cat > "nats-config/device-${DEVICE_ID}.conf" <<EOF
server_name: device-${DEVICE_ID}

port: 4222
http_port: 8222

leafnodes {
  remotes = [
    { url: "leaf://nats-remote:7422", credentials: "/etc/nats/leaf.creds" }
  ]
}

authorization {
  users = [
    {
      user: "${DEVICE_ID}"
      password: "${LOCAL_PASS}"
      permissions: {
        publish: { allow: ["device.${DEVICE_ID}.outgoing.data"] }
        subscribe: { allow: ["device.${DEVICE_ID}.incoming.data"] }
      }
    }
  ]
}
EOF

cat > "docker-compose.${DEVICE_ID}.yml" <<EOF
services:
  nats-${DEVICE_ID}:
    image: nats:2.10-alpine
    container_name: nats-${DEVICE_ID}
    command: ["-c", "/etc/nats/device.conf"]
    volumes:
      - ./nats-config/device-${DEVICE_ID}.conf:/etc/nats/device.conf:ro
      - ./nsc-data/creds/${DEVICE_ID}.creds:/etc/nats/leaf.creds:ro
    networks:
      default: {}

  device-${DEVICE_ID}-client:
    build: ./app
    container_name: device-${DEVICE_ID}-client
    environment:
      ROLE: device
      DEVICE_ID: ${DEVICE_ID}
      NATS_URL: nats://nats-${DEVICE_ID}:4222
      NATS_USER: ${DEVICE_ID}
      NATS_PASS: ${LOCAL_PASS}
    depends_on:
      - nats-${DEVICE_ID}
    networks:
      default: {}

networks:
  default:
    name: multi-nats_default
    external: true
EOF

echo "Bringing up nats-${DEVICE_ID} and device-${DEVICE_ID}-client (nats-remote is NOT touched)..."
docker compose -f "docker-compose.${DEVICE_ID}.yml" up -d --build

echo
echo "Device ${DEVICE_ID} provisioned and connected."
