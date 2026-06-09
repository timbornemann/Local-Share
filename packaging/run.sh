#!/usr/bin/env sh
set -eu

PORT="${PORT:-8080}"
NAME="${NAME:-local-share}"
IMAGE="__IMAGE__"

docker pull "$IMAGE"

if docker ps -a --filter "name=^/${NAME}$" --format '{{.Names}}' | grep -qx "$NAME"; then
  docker rm -f "$NAME" >/dev/null
fi

docker run -d --name "$NAME" -p "${PORT}:8080" --restart unless-stopped "$IMAGE"
printf 'Local Share is running at http://localhost:%s\n' "$PORT"
