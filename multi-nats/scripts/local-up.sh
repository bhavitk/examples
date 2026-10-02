#!/bin/sh
# Usage: scripts/local-up.sh <name> <ui-port>   e.g. scripts/local-up.sh L1 8080
# Starts (or updates) one local site as its own compose project.
set -e
NAME="${1:?name (e.g. L1)}"
PORT="${2:?ui port (e.g. 8080)}"
cd "$(dirname "$0")/.."
LOWER=$(echo "$NAME" | tr 'A-Z' 'a-z')
LOCAL_NAME="$NAME" UI_PORT="$PORT" LOCAL_DATA="./local-data/$LOWER" \
  docker compose -f docker-compose.local.yml -p "local-$LOWER" up -d --build
echo "UI: http://localhost:$PORT"
