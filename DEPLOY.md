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
- build one from source, or use the reproducible builder this repo ships in
  [`kernel/`](kernel/): it starts from Firecracker's config and adds the options
  microvm needs, so its output boots verified images out of the box.

> For **verified boot** (§5) a kernel needs `CONFIG_DM_VERITY=y` and
> `CONFIG_DM_INIT=y`; the `kernel/` builder enables both.

> For **snapshots** (`-snapshot-dir`) on an **arm64 host with a GICv2** — a
> Raspberry Pi 5's GIC-400, and anything that is not a GICv3 — the guest kernel
> needs `CONFIG_DEVMEM=y` (`/dev/mem`), which is on by default and is what the
> guest agent uses to put its own interrupt-controller state back after a restore.
> Firecracker does not save that state on GICv2, and without it a restored guest
> comes back with no working timer and answers nothing; the agent refuses to arm a
> snapshot it cannot carry, so the warm pool cold-boots instead of producing
> unrestorable snapshots. x86 hosts and arm64 hosts with a GICv3 need none of
> this. See [`internal/agent/gic_linux.go`](internal/agent/gic_linux.go).
>
> Snapshots also need the guest kernel's `RNDRESEEDCRNG` ioctl on `/dev/urandom`
> (`CONFIG_*` — it is unconditional in the random driver, so any mainline kernel
> has it). It is how a restored guest's CSPRNG is actually rotated; a guest that
> cannot rotate it has its restore refused rather than being handed out sharing
> its keys with every other restore of the same template. See
> [`internal/agent/reseed.go`](internal/agent/reseed.go).

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

### If you enable snapshots (`-snapshot-dir`)

```bash
# Scratch space, not storage. One directory per daemon: a snapshot's only handle
# is in the daemon's memory, so anything left here by a previous run is orphaned
# and is swept at startup -- two daemons sharing one would sweep each other's
# live templates.
mkdir -p /var/lib/microvm/snapshots
chmod 700 /var/lib/microvm/snapshots
```

Budget one **full copy of the guest's RAM per warm-pool shape**, so
`-warm python:2:512:2,node:2:512:2 -snapshot-dir …` wants ~1 GiB free on top of
the images. Templates are discarded when the daemon shuts down and swept if it
was killed, so the steady state is one per shape — but put it on a filesystem
where filling it does not take the log store and the image directory with it.

Snapshots need guest images built from this repo at or after the agent's snapshot
routes exist. An older image is not restored unsafely: the arm call warns and the
reseed call refuses, so the warm pool cold-boots that shape instead.

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

Storage enables tenant quotas; setting a tenant's policy needs an admin token
(§9), and on a fleet the tenant store must be shared, which happens
automatically when `-redis` is set.

---

## 9. Generate API tokens

Auth is bearer tokens. **Never on the command line** — a secret there is a
secret in `ps`, in shell history, and in the unit file. Three sources, in order
of preference:

| Source | Tokens | Admin tokens |
|---|---|---|
| Environment | `MICROVM_TOKENS` | `MICROVM_ADMIN_TOKENS` |
| File, one per line | `-tokens-file` | `-admin-tokens-file` |
| Flag (**deprecated**, still honoured) | `-tokens` | `-admin-tokens` |

They **add up**, which is what makes moving off the flags a rotation rather than
a cutover: add the file, restart, drop the flag, restart, and no client is
refused in between. A duplicate across sources is one token. A token file may
use `#` comments and one token per line, or commas; keep it root-owned and
`0600` (the daemon warns if it is readable by anyone else). A named file that is
missing or holds no tokens is fatal — a daemon that shrugged and started with no
auth would be serving VM creation to whoever found the port.

No tokens at all disables auth entirely — never do that on an exposed host.
Admin tokens are a superset that may also configure tenant storage policies.

```bash
openssl rand -hex 32   # generate one per client; put it in the env file below
```

Clients send the token as `Authorization: Bearer <token>`, which every SDK does
for you via `new Client(url, { token })` and equivalents.

### Per-tenant limits

Two ceilings bound **one token**, where `-slots` and `-ceiling-*` bound the
whole host. Without them any single valid token can occupy every slot on the
node and call `POST /sandboxes` as fast as VMs boot, and the other tenants meet
a host that is permanently full.

| Flag | Default | What it caps |
|---|---|---|
| `-tenant-max-sandboxes` | `0` = unlimited | Sandboxes one token may have running at once |
| `-tenant-max-rps` | `0` = unlimited | API requests per second per token, bursting one second's worth |

Both are charged to a **tenant** rather than to an address, because an address is
not an identity: one tenant behind a NAT would be charged for its neighbours, and
one with a pool of egress addresses for almost none of its own. `microvmd` derives
a tenant from each token, so on this daemon a tenant *is* a token and **two keys
are two allowances**. Rotating a key therefore doubles the ceiling for as long as
both are listed, and a team that wants one allowance across several keys needs
those keys to share a tenant — a `Principals` configuration the flags do not
expose yet. Plan a rotation as remove-then-add, or halve the ceiling while both
are live.

Over either limit the caller gets the API's existing `capacity_error` 429 — the
same type a full node answers with, so no SDK needed a new branch. Only the rate
limit sets `Retry-After`: a token bucket knows to the second when it will have
another request, whereas when one of a caller's own sandboxes ends is up to them,
and a number invented here would be a guess dressed as a fact. Both default to
off, so a node that sets neither behaves exactly as before.

**`-tenant-max-sandboxes` does not cover `POST /v1/tasks`.** The count is one
node's, and a task is scheduled across the fleet: a task enqueued on this node may
be leased by any other, where this counter says nothing about its submitter. So a
token that is refused a sandbox for being at its cap can still put work on the
node through the queue. That is a real gap in what the flag bounds, not a
disguised feature — bound task-driven load with `-slots`, `-cpu` and `-mem`, which
bound the box whoever the work belongs to, and with task `priority` for fairness
between callers.

### Sandboxes are scoped to their tenant

A sandbox belongs to the tenant whose token created it. Every route that names one
resolves it against the caller, so another tenant's sandbox answers `404` — not
`403`, which would confirm which of a guessed range of IDs exist — and
`GET /v1/sandboxes` returns only the caller's. An admin key keeps the node-wide
view, because it is the operator's own key and something has to be able to answer
"what is running on this box".

This matters most for files: a sandbox's `/mnt/storage` is its tenant's whole
object-store namespace, so a route that handed another tenant's sandbox over
handed over their stored data with it.

---

## 10. Optional: seeding sandboxes from a source

A caller may ask for a sandbox that arrives with a project already in it — a
tarball, or a git checkout — instead of uploading it one file at a time. **The
daemon does the fetching**, expands the result host-side, and writes the tree in
over the same file endpoints an upload uses. That is why it works for a sandbox
with no network at all, and why a private repository's token never enters a VM.

**It is off, and two flags are needed to turn it on:**

```bash
-source-fetch \
-source-allow-host codeload.github.com \
-source-allow-host .githubusercontent.com
```

Either one missing refuses every `source`. `-source-fetch` on its own names no
host, so an upgrade changes nothing about a deployment that does not set both — the
daemon logs a warning if you set the flag and leave the list empty, because that
combination looks enabled and is not.

### The allowlist is the security boundary

Everything else here is a limit. This is the control.

The daemon runs **outside the firewall it installs for its guests**. `internal/netpool`
blocks RFC1918, link-local and `169.254.169.254` for a sandbox with nftables; none of
that applies to the daemon's own sockets. So "fetch this URL for me" is an outbound
request made by a process that can reach your LAN, the daemon's own API on loopback,
and the cloud metadata service holding this host's identity — and the caller reads
the result out of their own sandbox afterwards. Unallowlisted, that is a complete
SSRF read primitive with the host's credentials at the end of it.

**A wildcard defeats the whole thing.** An entry is either an exact host
(`codeload.github.com`) or a leading-dot suffix (`.githubusercontent.com`, which
matches subdomains and not the bare domain). A suffix broad enough to match anything
— `.com`, `.net`, `.internal` — is the same as having no allowlist: any host an
attacker controls now answers a redirect, or a DNS record, of their choosing. Name
the hosts your callers actually fetch from. Two or three is normal.

What the allowlist does *not* do is decide addresses. Where a name resolves is
checked separately, at connect time, against the same blocked ranges a guest gets, on
every redirect hop and again on the syscall path — so allowlisting a host does not
allowlist an address, and a name that rebinds to the metadata address between the
check and the connect does not get a socket. `https` only, and no proxy is honoured,
not even the environment's.

### The caps

Each bounds something a hostile source would otherwise spend for free. Every one
defaults to a conservative number rather than to unlimited, so a flag nobody wired
up refuses a bad archive instead of admitting it.

| Flag | Default | What it caps |
|---|---|---|
| `-source-max-bytes` | 64 MiB | The compressed body downloaded |
| `-source-max-expanded-bytes` | 256 MiB | What is written into the guest — this is what stops a decompression bomb |
| `-source-max-file-bytes` | 64 MiB | One file inside a source. Raise it with the total; the guest itself takes 256 MiB per file |
| `-source-max-files` | 20000 | Members in one source, because a 10 KB tar of a million empty files passes both byte caps. It also bounds what the member names may total, at 256 bytes per member |
| `-source-timeout` | 60s | The fetch or clone, which is everything a third party controls |

The sandbox's own writable layer is smaller than these, and a tree that passes here
and will not fit the guest is refused per sandbox with both numbers named — before
the VM boots, rather than as an `ENOSPC` from inside a file transfer.

**A git clone is bounded by the same two byte caps**, which takes explaining because
git has no option for it: there is no `--max-bytes`, and `--depth 1` says nothing
about how large the one commit it fetches is. So the clone's temporary directory is
measured *while git is writing it*, and the clone is killed at
`-source-max-bytes` + `-source-max-expanded-bytes` — the packfile bounded like a
compressed body, the tree it expands to like an expanded archive. Measured during
rather than after, because "the caps were satisfied" is worth nothing once the disk
is full.

That directory is `-source-temp-dir`, or the system temporary directory when it is
unset. **Point it at a real disk if `/tmp` is a tmpfs here** (it is on Fedora, Arch
and anything with `PrivateTmp`), or what a seed stages competes for memory with the
VMs. Whatever is left there by a daemon killed mid-seed is swept at the next
startup.

Concurrency is bounded too, node-wide: this daemon fetches at most 8 sources at
once and answers a ninth with the same `429 node_at_capacity` a full node gives.
`-tenant-max-sandboxes` is not that bound — it defaults to unlimited, and a fetch
happens before the node's own admission has been asked whether there is room for a
VM at all.

### Credentials for a private repository

```bash
-source-credential github-ci@https://github.com/acme-corp/=/etc/microvm/github-ci.token
```

Three parts: the **name** a caller selects with `credential_ref`, the **URL prefix**
the credential may be sent to, and the **path** to the file holding it.

The flag takes a **path, never the secret** — the same reason as `-tokens-file`: a
secret on a command line is a secret in `ps`, in shell history and in the unit file.
Keep the file root-owned and `0600`; the daemon warns if anyone else can read it, and
refuses to start if the file is missing or empty, so a broken credential fails where
you are watching rather than on some caller's create hours later.

**The prefix is required, and it is a security boundary, not a convenience.** Every
caller shares this map, so a credential resolved by name alone is a confused deputy
twice over. It spends your token on any repository a caller names — git authenticates
the clone, and the working tree lands in a sandbox the caller reads — and it hands the
token *itself* to whatever server answers, because git sends its credential to any
host that challenges for one and an allowlist entry may be a suffix a caller can
choose a name under. With a prefix, `credential_ref: github-ci` on
`https://github.com/someone-else/private` is refused before a socket is opened, in
the same words as a credential that does not exist — so the names and the
repositories behind them cannot be probed either.

An `https` URL naming a host and as much path as you want to pin. Matching is on the
host and then the path at a component boundary, so `.../acme` does not cover
`.../acmecorp`. A prefix with no path (`https://git.example.com`) covers the whole
host, which is what to write for a self-hosted forge you own outright.

The file holds a token, or `username:token` for a forge that wants a real username.
Callers never see the value: the clone happens on this host, only the working tree is
copied into the guest, and `.git` is not written at all. The secret does not enter the
URL or argv either (`/proc/<pid>/cmdline` is world-readable), so it reaches git
through an askpass helper reading the environment.

A `git` source also needs `git` on `PATH`, **version 2.31 or newer**. The address
policy pins the clone to addresses it has vetted with `http.curloptResolve`, which
landed in 2.31 — and an unrecognised `http.*` setting is accepted and silently
ignored, so on an older git (Debian 11 ships 2.30.2) the pins would do nothing and
git would do its own DNS, which is the rebinding window the tarball path exists to
close. There is no safe degraded mode, so a host whose git is too old serves tarballs
only and says so at startup, exactly like a host with no git.

---

## 11. Run it under systemd

`microvmd` handles `SIGINT`/`SIGTERM` with a graceful shutdown in two 30-second
phases: it drains in-flight requests, then stops every sandbox and tears the
firewall down, in that order. A systemd unit is the natural fit. `Delegate=yes` hands
it a cgroup subtree to manage.

`TimeoutStopSec` has to sit above both phases, which is why the unit below says 120
rather than 45. Draining waits for in-flight requests rather than cancelling them,
and with `-source-fetch` on, the longest of those is no longer a boot: it is a create
holding a `-source-timeout` fetch plus the write of the tree that came out of it.
Cut short by `SIGKILL`, what leaks is the *other* sandboxes' jail directories and
cgroups, because the phase doing that teardown is the one that gets killed.

`/etc/systemd/system/microvmd.service`:

```ini
[Unit]
Description=microvm daemon
After=network-online.target redis.service
Wants=network-online.target

[Service]
# Tokens and AWS creds come from an env file, not the command line, so they
# stay out of `ps` and the unit itself. chmod 600 it, root-owned.
# MICROVM_TOKENS and MICROVM_ADMIN_TOKENS are read from the environment
# directly: expanding them into -tokens here would put the secret back on the
# command line this env file exists to keep it off.
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
  -tenant-max-sandboxes 20 \
  -tenant-max-rps 20
# Must be root: it manages TAP, nftables and cgroups. The jailer drops the VMM
# to -uid/-gid; the daemon itself stays root.
User=root
Delegate=yes
Restart=on-failure
TimeoutStopSec=120

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

## 12. Put TLS in front — this is not optional

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

## 13. Verify the deployment

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

## 14. Scaling to a fleet

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

## 15. Operating it

- **Adding an image:** drop a new `*.ext4` into `-image-dir` and restart the node.
- **Upgrades / rolling restarts:** on a fleet, a worker that stops pulling has its
  leases expire and its in-flight work returned to the queue, so you can restart
  workers one at a time with no lost tasks. Drain a node by removing its public
  `-addr` from the load balancer first if it also serves the API.
- **Graceful shutdown:** `systemctl stop` sends `SIGTERM`; the daemon drains
  in-flight requests for up to 30s, then stops every sandbox and removes its
  firewall rules within another 30s. Keep `TimeoutStopSec` above both — and above
  `-source-timeout` too if `-source-fetch` is on, since a create mid-seed is what
  the drain is waiting for. The unit in §11 uses 120.
- **Log retention:** exec output is kept `-log-retention` (default 1h) after a run
  finishes, then swept. Raise it if clients collect output late.
- **Sandbox retention:** a stopped sandbox stays listed and retrievable so a caller
  learns *why* it stopped and what it cost. `-sandbox-retention` is when the daemon
  forgets it; the default `0` never does, which is a slow leak on a node that never
  restarts, so set it. Set below `-log-retention` it is raised to it: the stopped
  record carries the final metering and every exec record is reached through its
  sandbox, so the shorter window would strand output still on the host. Past the
  window the ID answers the ordinary `sandbox_not_found` 404 — anything you need to
  keep comes from the `DELETE` reply.
- **Monitoring:** scrape `GET /v1/queue` for depth and `oldest_pending_ms`, and
  alert on `microvmd` restarts and on `/v1/health` failing.

---

## 16. Security checklist

- [ ] `-addr` on loopback; a TLS terminator on the public edge.
- [ ] Tokens set (never none on an exposed host); admin tokens only where an
      operator needs the tenant API.
- [ ] Tokens and AWS credentials in a `0600` env file, a `0600` token file, or
      an instance role — never on the command line. `-tokens`/`-admin-tokens`
      are deprecated for exactly this reason.
- [ ] `-tenant-max-sandboxes` and `-tenant-max-rps` set wherever more than one
      tenant shares a node: without them one token can hold every slot and
      hammer the API as fast as VMs boot.
- [ ] `-source-fetch` left **off** unless sandboxes need seeding from a URL, and
      `-source-allow-host` naming every host that may be fetched from when it is
      on. This is the one request that makes the daemon reach the network on a
      caller's behalf, and the daemon sits outside the firewall it installs for
      guests — the allowlist is what stands between a caller's URL and the host's
      LAN, its own API on loopback, and the cloud metadata service. Where a name
      resolves is checked again at connect time either way, so allowlisting a host
      does not allowlist an address. No wildcard-shaped entry (`.com`, `.internal`)
      — a suffix that matches anything is the same as no allowlist. See §10.
- [ ] `-source-credential` values in `0600` root-owned files, each bound to the
      **narrowest URL prefix that works** — the organisation, or the one repository.
      The flag takes a path, never the secret, and callers select one by name and
      never see it; the prefix is what stops any caller spending it on any repository
      the token can reach, and handing it to any allowlisted host that asks.
- [ ] `-source-temp-dir` on a real disk if `/tmp` is a tmpfs on this host and
      `-source-fetch` is on.
- [ ] `-uid`/`-gid` are a real non-root user.
- [ ] `-ceiling-cores`/`-ceiling-mem-mb` set on any shared host.
- [ ] Per-sandbox network cap left on (`-default-network-bps`, ~100 Mbit default):
      a sandbox on a fraction of a core can still saturate the uplink.
- [ ] Redis reachable only from the fleet, not the public internet.
- [ ] Firewall on the host allows the public API port only via the terminator.
