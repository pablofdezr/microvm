#!/usr/bin/env bash
# Builds a language image into an ext4 rootfs that Firecracker can boot.
#
# Docker is used only to assemble a userland; the result is exported flat and
# packed into a filesystem image, and nothing about the container runtime
# survives into the VM. The packing runs inside a container too, so the build
# needs no root on the host.
#
# Usage: images/build.sh <image> [arch]
#   image  base | python | node | go | rust
#   arch   arm64 | amd64   (default: the host's architecture)
#
# Env:
#   MICROVM_VERITY=1  also emit a dm-verity hash tree (<out>.hash) and root-hash
#                     sidecar (<out>.verity) for verified boot. Needs a guest
#                     kernel with CONFIG_DM_VERITY + CONFIG_DM_INIT. See DEPLOY.md.
set -euo pipefail

readonly REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly IMAGE="${1:-base}"
readonly ARCH_IN="${2:-$(uname -m)}"
readonly OUT_DIR="${MICROVM_OUT_DIR:-$REPO_ROOT/build/images}"

# Slack on top of the measured contents: package managers need room to work, and
# a filesystem at 100% capacity fails in confusing ways.
readonly SLACK_MB="${MICROVM_SLACK_MB:-256}"

die() { echo "error: $*" >&2; exit 1; }
log() { echo "==> $*"; }

case "$ARCH_IN" in
  arm64|aarch64) readonly ARCH=arm64 ;;
  amd64|x86_64)  readonly ARCH=amd64 ;;
  *) die "unsupported architecture: $ARCH_IN" ;;
esac

readonly DOCKERFILE="$REPO_ROOT/images/$IMAGE/Dockerfile"
[ -f "$DOCKERFILE" ] || die "no Dockerfile for image '$IMAGE' at $DOCKERFILE"
command -v docker >/dev/null || die "docker is required"

readonly TAG="microvm-rootfs:${IMAGE}-${ARCH}"
readonly PACKER_TAG="microvm-packer:latest"
readonly OUT_NAME="${IMAGE}-${ARCH}.ext4"
mkdir -p "$OUT_DIR"

log "building $TAG for linux/$ARCH"
# The build context is the repo root: the base image compiles the agent from
# source rather than trusting a binary built elsewhere.
docker build \
  --platform "linux/$ARCH" \
  -f "$DOCKERFILE" \
  -t "$TAG" \
  "$REPO_ROOT"

log "building packer toolbox"
# Native architecture on purpose: it only runs mke2fs, and emulating it under
# qemu would slow the pack down for no benefit.
docker build -q -f "$REPO_ROOT/images/packer/Dockerfile" -t "$PACKER_TAG" "$REPO_ROOT" >/dev/null

CONTAINER=""
cleanup() { [ -n "$CONTAINER" ] && docker rm -f "$CONTAINER" >/dev/null 2>&1 || true; }
trap cleanup EXIT

# `docker export` flattens the filesystem and drops the image config, taking
# every ENV with it. Read them from the config now and hand them to the packer,
# which writes them into the rootfs where the guest's init can find them.
# Without this the Dockerfiles' ENV lines do nothing at all.
IMAGE_ENV="$(docker inspect --format '{{range .Config.Env}}{{println .}}{{end}}' "$TAG")"

log "exporting and packing to ext4"
CONTAINER="$(docker create --platform "linux/$ARCH" "$TAG" /bin/true)"

# The export streams straight into the packer's stdin, so the full rootfs never
# lands on the host disk as an intermediate copy.
docker export "$CONTAINER" \
  | docker run --rm -i \
      -v "$OUT_DIR:/out" \
      -e OUT_NAME="$OUT_NAME" \
      -e SLACK_MB="$SLACK_MB" \
      -e OWNER_UID="$(id -u)" \
      -e OWNER_GID="$(id -g)" \
      -e IMAGE_ENV="$IMAGE_ENV" \
      -e MICROVM_VERITY="${MICROVM_VERITY:-0}" \
      "$PACKER_TAG"

log "built $OUT_DIR/$OUT_NAME"
