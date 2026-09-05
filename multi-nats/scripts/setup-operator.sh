#!/bin/sh
# One-time setup: creates the Operator, SYS account, and DEVICES account,
# then generates nats-config/resolver.conf (mem-resolver preload) for nats-remote.
#
# This is the ONLY step that ever touches nats-remote's config. After this,
# new devices are provisioned with scripts/provision-device.sh and nats-remote
# is never restarted again.
set -e

cd "$(dirname "$0")/.."

docker build -q -t multi-nats-provisioning ./provisioning >/dev/null

mkdir -p nsc-data nats-config

docker run --rm \
  --user "$(id -u):$(id -g)" \
  -v "$(pwd)/nsc-data:/work/nsc-data" \
  -v "$(pwd)/nats-config:/work/nats-config" \
  multi-nats-provisioning -c '
    set -e
    D=--all-dirs=/work/nsc-data/store
    nsc add operator $D --generate-signing-key --sys multinats
    nsc add account $D DEVICES
    nsc edit account $D DEVICES --sk generate
    nsc generate config $D --mem-resolver --sys-account SYS --config-file /work/nats-config/resolver.conf --force
    echo "--- operator/account setup complete ---"
    nsc list accounts $D
  '

echo
echo "resolver.conf written to nats-config/resolver.conf"
echo "Next: include it from nats-config/remote.conf, then start/restart nats-remote ONE more time to pick it up."
