#!/usr/bin/env bash
#
# udocker-sandbox-image.sh — build the GoClaw sandbox image with udocker
# (no Docker daemon, no root). Mirrors Dockerfile.sandbox.
#
# udocker cannot run Dockerfiles, so the image is produced by:
#   pull base image -> create container -> install packages -> export -> import.
#
# Usage:
#   scripts/udocker-sandbox-image.sh [image-tag]
#
# Env:
#   BASE_IMAGE   base image (default debian:bookworm-slim)
#   UDOCKER      udocker executable (default $GOCLAW_UDOCKER_BIN or "udocker")

set -euo pipefail

TAG="${1:-goclaw-sandbox:bookworm-slim}"
BASE_IMAGE="${BASE_IMAGE:-debian:bookworm-slim}"
UDOCKER="${UDOCKER:-${GOCLAW_UDOCKER_BIN:-udocker}}"
BUILD_NAME="goclaw-sandbox-build-$$"
TARBALL="$(mktemp -t goclaw-sandbox-XXXXXX.tar)"

UD=("$UDOCKER")
if [ "$(id -u)" = "0" ]; then
  UD+=(--allow-root)
fi

cleanup() {
  "${UD[@]}" rm -f "$BUILD_NAME" >/dev/null 2>&1 || true
  rm -f "$TARBALL"
}
trap cleanup EXIT

command -v "$UDOCKER" >/dev/null || { echo "udocker not found (pip install udocker && udocker install)" >&2; exit 1; }

echo "==> Pulling $BASE_IMAGE"
"${UD[@]}" pull "$BASE_IMAGE"

echo "==> Creating build container"
"${UD[@]}" create --name="$BUILD_NAME" "$BASE_IMAGE" >/dev/null

echo "==> Installing packages"
"${UD[@]}" run --nobanner --user=root --workdir=/ "$BUILD_NAME" /bin/sh -ec '
  export DEBIAN_FRONTEND=noninteractive
  APT_OPTS="-o APT::Sandbox::User=root"
  apt-get $APT_OPTS update
  apt-get $APT_OPTS install -y --no-install-recommends \
    bash ca-certificates curl git jq python3 python3-pip ripgrep
  rm -rf /var/lib/apt/lists/*
  id sandbox >/dev/null 2>&1 || useradd --create-home --shell /bin/bash sandbox
  mkdir -p /workspace
'

echo "==> Exporting container"
"${UD[@]}" export -o "$TARBALL" "$BUILD_NAME"

if "${UD[@]}" inspect "$TAG" >/dev/null 2>&1; then
  echo "==> Removing existing image $TAG"
  "${UD[@]}" rmi -f "$TAG" >/dev/null
fi

echo "==> Importing as $TAG"
"${UD[@]}" import "$TARBALL" "$TAG"

echo "Done. Image $TAG is available to udocker."
