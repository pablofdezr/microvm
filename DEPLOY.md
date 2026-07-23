# Deploying microvm to production

This is the operator's guide: how to take the daemon from a checkout to a host
that serves the API and runs untrusted code safely. It assumes you have read the
architecture and security sections of the [README](README.md).

The short version: on each host you install `microvmd`, a guest kernel, and the
rootfs images; you run it as root behind a TLS terminator; and, for more than one
host, you point them all at a shared Redis. Everything below is the long version.

---

## 1. What actually gets deployed

Three binaries live in this repo, and they run in three different places:

| Binary | Runs where | Deployed how |
|---|---|---|
| `microvmd` | the host, as **root** | you install and run it (this guide) |
| `microvm` | operator/CI machines | the CLI; optional, for humans |
| `microvm-agent` | **inside** every guest as PID 1 | compiled *into* the rootfs image; never installed on the host |

You only deploy `microvmd` to production hosts. The agent ships baked into the
images (the base `Dockerfile` compiles it from source), so there is nothing to
install for it.

`microvmd` is **one daemon per host**. Running two on the same box would have each
install its own firewall and fight over the same TAP name space.

---

## 2. Requirements

### The production host (where `microvmd` runs)

- **Linux with KVM.** Firecracker needs hardware virtualization: `/dev/kvm` must
  exist and be usable. Most cheap/shared VPS (OpenVZ, or KVM guests without
  nested virtualization) do **not** expose it — you need bare metal or an
  instance type that offers nested virtualization (e.g. `*.metal`).
- **Firecracker and its jailer on `PATH`.** `microvmd` launches VMs through the
  jailer, not Firecracker directly.
- **Root.** The daemon manages TAP devices, nftables rules and cgroups. It
  refuses to start otherwise.
- **An unprivileged uid/gid** for the jailer to drop the VMM to (e.g. a `microvm`
  system user). Passed with `-uid`/`-gid`; both are required and must be non-root.
- **cgroups v2** (the modern default) and the kernel modules `kvm` +
  `kvm_intel`/`kvm_amd`, `tun` (TAP), and `nf_tables`.
- **CPU architecture** (`arm64` or `amd64`) must match the kernel and images you
  build below.

### A build machine (can be your laptop or CI)

- **Go 1.26+** to build the binaries.
- **Docker** to build the rootfs images (the build itself needs no root). Build
  on a machine that is *not* serving traffic — compiling the Rust image will
  saturate every core it can find.

---

## 3. Build the binaries

`microvmd` is Linux-only (the Firecracker runtime is guarded by `//go:build
linux`), so build it on the host, or cross-compile from anywhere:

```bash
# On the Linux host:
go build -o microvmd ./cmd/microvmd
go build -o microvm  ./cmd/microvm   # the CLI, if you want it here too

# Or cross-compile from any machine (match the host's arch):
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o microvmd ./cmd/microvmd
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o microvmd ./cmd/microvmd
```

Copy `microvmd` to the host, e.g. `/usr/local/bin/microvmd`.

---

## 4. Install Firecracker and a guest kernel

### Firecracker

Download the release for your host's architecture from the
[Firecracker releases](https://github.com/firecracker-microvm/firecracker/releases),
and put **both** `firecracker` and `jailer` on `PATH`:

```bash
install -m 0755 firecracker jailer /usr/local/bin/
firecracker --version   # sanity check
```

### Guest kernel

Firecracker boots an **uncompressed** Linux kernel image (`vmlinux`, not a
`bzImage`), built with a Firecracker-compatible config. This repo does not ship
one. Either:

- use a prebuilt kernel from Firecracker's own quickstart / CI artifacts (see the
  [Firecracker getting-started guide](https://github.com/firecracker-microvm/firecracker/blob/main/docs/getting-started.md)), or
- build one from source with their recommended microVM config.

> For **verified boot** (§5), the kernel additionally needs `CONFIG_DM_VERITY=y`
> and `CONFIG_DM_INIT=y`.

The `vmlinux` architecture must match the host. Place it where the daemon expects
it (default `/var/lib/microvm/vmlinux`, overridable with `-kernel`):

```bash
install -D -m 0644 vmlinux /var/lib/microvm/vmlinux
```

---

## 5. Build and install the rootfs images

On the build machine, build one ext4 image per language you want to offer. The
architecture **must** match the production host:

```bash
images/build.sh python arm64      # or amd64
images/build.sh node   arm64
images/build.sh go     arm64
images/build.sh rust   arm64
# → build/images/<lang>-<arch>.ext4
```

The image name the API exposes is exactly the filename without `.ext4`, so
`python-arm64.ext4` is the image `python-arm64`. If you run a single-architecture
fleet and want callers to say `image: "python"` (as the SDK examples do), install
it under the bare name:

```bash
# Keep the arch in the name...
install -D -m 0644 build/images/python-arm64.ext4 /var/lib/microvm/images/python-arm64.ext4
# ...or drop it so the API image is just "python":
install -D -m 0644 build/images/python-arm64.ext4 /var/lib/microvm/images/python.ext4
# ...repeat per image
```

The daemon reports the images it can run from what is on disk (`GET /v1/images`),
so adding an image is just dropping in a file and restarting.

### Verified boot with dm-verity (optional)

The rootfs images are read-only and shared by every sandbox on a host, so a
tampered image is a tampered userland for everyone who runs it. dm-verity makes
that fail closed: the kernel verifies every block of the image against a hash
tree at boot and **panics before init runs** if anything was altered.

Build the verity artifacts by setting `MICROVM_VERITY=1`:

```bash
MICROVM_VERITY=1 images/build.sh python arm64
# → build/images/python-arm64.ext4         the image
#   build/images/python-arm64.ext4.hash     the hash tree
#   build/images/python-arm64.ext4.verity   the root hash + geometry (JSON)
```

Install all three next to each other, keeping the `.hash` and `.verity` suffixes
on whatever you name the image:

```bash
install -D -m 0644 build/images/python-arm64.ext4        /var/lib/microvm/images/python.ext4
install -D -m 0644 build/images/python-arm64.ext4.hash   /var/lib/microvm/images/python.ext4.hash
install -D -m 0644 build/images/python-arm64.ext4.verity /var/lib/microvm/images/python.ext4.verity
```

The daemon detects the sidecar automatically: an image that has one boots
verified, an image that does not boots exactly as before — no flags, no
per-sandbox config. It needs a guest kernel with `CONFIG_DM_VERITY=y` and
`CONFIG_DM_INIT=y` (§4); that requirement is why verity is opt-in per image
rather than forced on every host.

---

## 6. Prepare the host

```bash
# 1. An unprivileged user for the VMM to drop to.
useradd --system --no-create-home --shell /usr/sbin/nologin microvm
id -u microvm   # note the uid/gid for -uid / -gid below

# 2. Directories. chroot-base MUST share a filesystem with image-dir, or every
#    sandbox copies its image instead of hardlinking it — put both under the
#    same mount (here, /var/lib/microvm and /srv/jailer both on /).
mkdir -p /var/lib/microvm/images /srv/jailer

# 3. Confirm the host can actually virtualize.
ls -l /dev/kvm            # must exist
lsmod | grep -E 'kvm|tun' # kvm + tun present
```

---

## 7. Optional: shared queue (Redis) for a fleet

A **single** host needs no Redis — the queue lives in-process. But an in-process
queue is not shared with other nodes and does not survive a restart, so any fleet
of two or more hosts needs a shared Redis that every node points at with `-redis`.
Nodes sharing a `-redis-prefix` share a queue (and, when storage is on, tenant
policies), so the prefix is what separates two fleets on one Redis.

Run Redis somewhere both reachable and private to the fleet. Nothing about it is
special; a managed Redis is fine.

---

## 8. Optional: sandbox file storage (S3)

If sandboxes need durable storage (`-s3-bucket`), the daemon connects to it at
startup so bad credentials fail where an operator is watching, not an hour later.

**Credentials are never a flag** — a secret on a command line is a secret in `ps`
and in shell history. The daemon uses the AWS SDK's own credential chain
(environment, shared config, or an instance role). Prefer an **instance role**,
which has no value to leak at all. For MinIO/R2 or another S3-compatible server,
set `-s3-endpoint` and `-s3-path-style`.

Storage enables tenant quotas; setting a tenant's policy needs an `-admin-tokens`
key, and on a fleet the tenant store must be shared, which happens automatically
when `-redis` is set.

---

## 9. Generate API tokens

Auth is bearer tokens passed with `-tokens` (comma-separated). An empty `-tokens`
disables auth entirely — never do that on an exposed host. `-admin-tokens` is a
superset that may also configure tenant storage policies.

```bash
openssl rand -hex 32   # generate one per client; keep them out of the unit file
```

Clients send the token as `Authorization: Bearer <token>`, which every SDK does
for you via `new Client(url, { token })` and equivalents.

---

## 10. Run it under systemd

`microvmd` handles `SIGINT`/`SIGTERM` with a 30-second graceful shutdown (it stops
every sandbox, then tears the firewall down, in that order). A systemd unit is the
natural fit. `Delegate=yes` hands it a cgroup subtree to manage.

`/etc/systemd/system/microvmd.service`:

```ini
[Unit]
Description=microvm daemon
After=network-online.target redis.service
Wants=network-online.target

[Service]
# Tokens and AWS creds come from an env file, not the command line, so they
# stay out of `ps` and the unit itself. chmod 600 it, root-owned.
EnvironmentFile=/etc/microvm/microvmd.env
ExecStart=/usr/local/bin/microvmd \
  -addr 127.0.0.1:8080 \
  -image-dir /var/lib/microvm/images \
  -kernel /var/lib/microvm/vmlinux \
  -chroot-base /srv/jailer \
  -uid 1000 -gid 1000 \
  -slots 10 \
  -cpu 8 -mem 16384 \
  -ceiling-cores 8 -ceiling-mem-mb 16384 \
  -redis 127.0.0.1:6379 \
  -tokens ${MICROVM_TOKENS} \
  -admin-tokens ${MICROVM_ADMIN_TOKENS}
# Must be root: it manages TAP, nftables and cgroups. The jailer drops the VMM
# to -uid/-gid; the daemon itself stays root.
User=root
Delegate=yes
Restart=on-failure
TimeoutStopSec=45

[Install]
WantedBy=multi-user.target
```

`/etc/microvm/microvmd.env` (mode `0600`, root-owned):

```
MICROVM_TOKENS=<token-a>,<token-b>
MICROVM_ADMIN_TOKENS=<admin-token>
# If using S3 without an instance role:
# AWS_ACCESS_KEY_ID=...
# AWS_SECRET_ACCESS_KEY=...
# AWS_REGION=...
```

```bash
systemctl daemon-reload
systemctl enable --now microvmd
journalctl -u microvmd -f
```

Set `-ceiling-cores`/`-ceiling-mem-mb` on any host running anything else besides
microvm; without them, sandboxes can consume the whole box. `-cpu`/`-mem` bound
what the *queue* packs onto the node and should reflect the host's real capacity;
`-slots` caps the VM count on top of that, for the fixed per-VM overhead.

---

## 11. Put TLS in front — this is not optional

`-addr` defaults to `127.0.0.1` on purpose: **this API creates VMs that run
arbitrary code, so an open one is an open shell.** Never bind it to a public
interface directly. Put a TLS terminator (nginx, Caddy, a cloud load balancer) on
the public edge and proxy to `127.0.0.1:8080`.

Minimal Caddy example:

```
api.example.com {
    reverse_proxy 127.0.0.1:8080
}
```

The SSE stream endpoint holds a response open for the life of a command, so
disable proxy response buffering and any short read/idle timeout on that path.

---

## 12. Verify the deployment

```bash
# Liveness needs no token:
curl -fsS https://api.example.com/v1/health

# End to end, with the CLI pointed at the host:
microvm run python -c "print('hello from a microVM')"
```

Or from TypeScript against the public URL:

```ts
const client = new Client("https://api.example.com", { token });
const done = await client.tasks.wait(
  (await client.tasks.create({
    image: "python", cmd: "python3", args: ["-c", "print(2+2)"],
  })).id,
);
console.log(done.stdout); // "4\n"
```

If `health` is green but a run fails, the usual causes are: `/dev/kvm` missing
(not a real virtualization host), `firecracker`/`jailer` not on `PATH`, an
image/kernel architecture mismatch, or `chroot-base` on a different filesystem
from `image-dir`.

---

## 13. Scaling to a fleet

The queue is the source of truth and nodes are dumb pullers, so the same binary is
shaped by its flags into three roles. Add capacity by starting more workers
against the same Redis; no node is told the others exist.

| Role | Flags |
|---|---|
| **Single box** | `-addr` + `-slots N` (+ optional `-redis`) |
| **API front end** (accepts work, runs none) | `-redis` + `-slots 0` + `-addr` |
| **Pure worker** (pulls work, no API exposed) | `-redis` + `-slots N`, no public `-addr` |

Set `-cpu`/`-mem` on every worker so the resource-aware packer can mix task sizes
without oversubscribing memory. Watch `GET /v1/queue`'s `oldest_pending_ms`: that
is the number that tells you whether the fleet is big enough.

---

## 14. Operating it

- **Adding an image:** drop a new `*.ext4` into `-image-dir` and restart the node.
- **Upgrades / rolling restarts:** on a fleet, a worker that stops pulling has its
  leases expire and its in-flight work returned to the queue, so you can restart
  workers one at a time with no lost tasks. Drain a node by removing its public
  `-addr` from the load balancer first if it also serves the API.
- **Graceful shutdown:** `systemctl stop` sends `SIGTERM`; the daemon stops every
  sandbox and removes its firewall rules within 30s. Keep `TimeoutStopSec` above
  that.
- **Log retention:** exec output is kept `-log-retention` (default 1h) after a run
  finishes, then swept. Raise it if clients collect output late.
- **Monitoring:** scrape `GET /v1/queue` for depth and `oldest_pending_ms`, and
  alert on `microvmd` restarts and on `/v1/health` failing.

---

## 15. Security checklist

- [ ] `-addr` on loopback; a TLS terminator on the public edge.
- [ ] `-tokens` set (never empty on an exposed host); `-admin-tokens` only where
      an operator needs the tenant API.
- [ ] Tokens and AWS credentials in a `0600` env file or an instance role, never
      on the command line.
- [ ] `-uid`/`-gid` are a real non-root user.
- [ ] `-ceiling-cores`/`-ceiling-mem-mb` set on any shared host.
- [ ] Per-sandbox network cap left on (`-default-network-bps`, ~100 Mbit default):
      a sandbox on a fraction of a core can still saturate the uplink.
- [ ] Redis reachable only from the fleet, not the public internet.
- [ ] Firewall on the host allows the public API port only via the terminator.
