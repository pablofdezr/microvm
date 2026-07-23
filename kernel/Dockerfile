# Reproducible builder for a Firecracker-compatible guest kernel (an
# uncompressed vmlinux) with the options microvm needs on top of Firecracker's
# own microVM config: dm-verity verified boot (DM_VERITY + DM_INIT), FUSE for
# storage, and overlayfs for the writable root.
#
# This image is never booted; only the toolchain matters. The kernel is built
# for the ARCH the build script is told, so it runs on the host's native arch and
# picks the target through ARCH rather than through the base image.
FROM debian:trixie-slim

RUN apt-get update \
 && apt-get install -y --no-install-recommends \
    bc bison flex build-essential libelf-dev libssl-dev \
    curl ca-certificates xz-utils \
 && rm -rf /var/lib/apt/lists/*

COPY kernel/build.sh /usr/local/bin/build-kernel.sh
RUN chmod +x /usr/local/bin/build-kernel.sh

ENTRYPOINT ["/usr/local/bin/build-kernel.sh"]
