#!/usr/bin/env sh
set -eu

PORT="${PORT:-8080}"
NAME="${NAME:-local-share}"
IMAGE="__IMAGE__"

docker pull "$IMAGE"

if docker ps -a --filter "name=^/${NAME}$" --format '{{.Names}}' | grep -qx "$NAME"; then
  docker rm -f "$NAME" >/dev/null
fi

# Uploads are untrusted data: read-only root filesystem, uploads only in a
# noexec in-memory tmpfs, no capabilities, no privilege escalation.
docker run -d --name "$NAME" -p "${PORT}:8080" --restart unless-stopped \
  --user 65532:65532 \
  --read-only \
  --tmpfs /tmp:rw,noexec,nosuid,nodev,size=2200m,mode=1777 \
  --cap-drop ALL \
  --security-opt no-new-privileges:true \
  --pids-limit 256 \
  "$IMAGE"
printf 'Local Share is running at http://localhost:%s\n' "$PORT"
