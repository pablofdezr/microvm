# A throwaway toolbox that turns an exported container filesystem into an ext4
# image. It exists so the build needs no privileges on the host: the container
# is already root inside its own namespace, which is all mke2fs -d requires.
#
# This image is never booted; only the tools matter, so it is always built for
# the host's native architecture regardless of the rootfs being packed.
FROM debian:trixie-slim

# e2fsprogs builds the ext4 image; cryptsetup-bin provides veritysetup, used to
# generate the dm-verity hash tree when MICROVM_VERITY=1.
RUN apt-get update \
 && apt-get install -y --no-install-recommends e2fsprogs cryptsetup-bin \
 && rm -rf /var/lib/apt/lists/*

COPY images/packer/pack.sh /usr/local/bin/pack.sh
RUN chmod +x /usr/local/bin/pack.sh

ENTRYPOINT ["/usr/local/bin/pack.sh"]
