#!/bin/bash
# Builds a Firecracker guest vmlinux carrying microvm's required kernel options.
#
# It starts from Firecracker's own microVM config -- the known-good baseline for
# a kernel that boots under Firecracker at all -- and turns on only what microvm
# adds on top, so the result stays minimal. Runs inside kernel/Dockerfile and
# writes /out/vmlinux.
#
# Env:
#   ARCH            arm64 | amd64   (default: arm64)
#   KERNEL_VERSION  a 6.1.x release (default: 6.1.90)
set -euo pipefail

ARCH="${ARCH:-arm64}"
KVER="${KERNEL_VERSION:-6.1.90}"

case "$ARCH" in
  arm64) FCARCH=aarch64; KARCH=arm64 ;;
  amd64) FCARCH=x86_64;  KARCH=x86 ;;
  *) echo "unsupported ARCH: $ARCH (want arm64 or amd64)" >&2; exit 1 ;;
esac

# Firecracker ships one config per kernel series; 6.1 is the LTS we target, so
# KERNEL_VERSION must stay on 6.1.x for the config to apply cleanly.
FC_CONFIG="microvm-kernel-ci-${FCARCH}-6.1.config"
FC_URL="https://raw.githubusercontent.com/firecracker-microvm/firecracker/main/resources/guest_configs/${FC_CONFIG}"

echo "==> fetching linux ${KVER}"
cd /tmp
curl -fsSL "https://cdn.kernel.org/pub/linux/kernel/v6.x/linux-${KVER}.tar.xz" | tar -xJ
cd "linux-${KVER}"

echo "==> applying Firecracker microVM config (${FC_CONFIG})"
curl -fsSL "$FC_URL" -o .config

# microvm's additions on top of the Firecracker baseline. dm-verity + dm-init let
# the kernel assemble a verified root from a hash device passed on the command
# line (see internal/verity and DEPLOY.md); FUSE and overlayfs are what the guest
# agent mounts for object storage and the writable tmpfs root.
echo "==> enabling dm-verity, dm-init, fuse, overlayfs"
./scripts/config \
  --enable CONFIG_MD \
  --enable CONFIG_BLK_DEV_DM \
  --enable CONFIG_DM_VERITY \
  --enable CONFIG_DM_INIT \
  --enable CONFIG_FUSE_FS \
  --enable CONFIG_OVERLAY_FS

make ARCH="$KARCH" olddefconfig

echo "==> config check (these must read =y)"
grep -E 'CONFIG_DM_VERITY|CONFIG_DM_INIT|CONFIG_BLK_DEV_DM=|CONFIG_FUSE_FS=|CONFIG_OVERLAY_FS=' .config

# Firecracker loads a different kernel format per architecture: an ELF vmlinux on
# x86_64, but the PE-format arm64 Image on aarch64 (feeding it a vmlinux there
# fails with "invalid Image magic number"). Build the one Firecracker expects.
case "$ARCH" in
  arm64) target=Image; artifact=arch/arm64/boot/Image ;;
  amd64) target=vmlinux; artifact=vmlinux ;;
esac

echo "==> building $target with $(nproc) jobs"
make ARCH="$KARCH" "$target" -j"$(nproc)"

# Installed as "vmlinux" to match the daemon's default -kernel path, whatever the
# on-disk format actually is.
install -D -m 0644 "$artifact" /out/vmlinux
echo "==> wrote /out/vmlinux ($(du -h "$artifact" | cut -f1), $target format)"
