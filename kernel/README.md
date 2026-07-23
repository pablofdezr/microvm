# Guest kernel

Firecracker boots an **uncompressed** `vmlinux`, and this repo does not ship one
(they are large and architecture-specific). This directory builds one
reproducibly, starting from Firecracker's own microVM config — the known-good
baseline for a kernel that boots under Firecracker — and adding only what microvm
needs on top:

- **`CONFIG_DM_VERITY` + `CONFIG_DM_INIT`** — verified boot. The daemon passes the
  hash device and root hash via `dm-mod.create=`, which `CONFIG_DM_INIT` parses;
  the kernel then refuses to mount a tampered rootfs. See [internal/verity](../internal/verity)
  and [DEPLOY.md](../DEPLOY.md) §5.
- **`CONFIG_FUSE_FS`** — the guest mounts object storage over FUSE.
- **`CONFIG_OVERLAY_FS`** — the writable tmpfs root the guest agent pivots into.

## Build

```sh
docker build -f kernel/Dockerfile -t microvm-kernel .
docker run --rm -v "$PWD/build:/out" -e ARCH=arm64 microvm-kernel
# → build/vmlinux
```

Then install it where the daemon expects it (`-kernel`, default
`/var/lib/microvm/vmlinux`):

```sh
install -D -m 0644 build/vmlinux /var/lib/microvm/vmlinux
```

`ARCH` (`arm64` | `amd64`) and `KERNEL_VERSION` (a `6.1.x` release) are
overridable via `-e`. The build needs no root on the host — it runs entirely
inside the container.
