# Changelog

Notable changes to the microvm daemon and its SDKs. The SDKs are versioned
together at the version below; each is released to its own registry (pkg.go.dev,
npm, PyPI) by tagging this repository.

The format follows [Keep a Changelog](https://keepachangelog.com), and the
project aims for [Semantic Versioning](https://semver.org).

## [Unreleased]

### Added

- **Resume-after-stop (suspend/resume).** `POST /sandboxes/{id}/suspend`
  snapshots a *used* sandbox to disk and tears its VM down; `POST
  /sandboxes/{id}/resume` boots a fresh VM from that snapshot under the same id,
  with a fresh TTL window. A suspended sandbox costs no CPU or memory, only the
  snapshot, and keeps its tenant slot and name so a resume is guaranteed both and
  cannot be refused for either. It is kept until resumed or deleted, and is never
  swept like a stopped sandbox. A new `suspended` sandbox state and `suspended`
  stop reason. Available on the three SDKs (`Suspend`/`Resume`).

  Networked sandboxes can be resumed: the restore takes a fresh netpool slot,
  remaps its interface onto the new host TAP with Firecracker's
  `network_overrides`, and re-addresses the guest over vsock through a new guest
  agent route (`POST /v1/net/configure`) — so a restore comes back on its own
  address rather than the template's, lifting the old "no networking on restore"
  limitation. The host-side lifecycle is unit-tested against the runtime fake; the
  networked-restore path runs only on a KVM host and has not been exercised on one
  in this change. Still no surviving a daemon restart: a snapshot's only handle is
  an in-memory ref, so suspend/resume works within a daemon's life, not across a
  restart of it.
- **Named sandboxes and get-or-create.** A create may carry a `name` (unique among
  a tenant's running sandboxes, scoped to the API key). With `get_or_create: true`,
  the same name returns the sandbox you already have (a 200) rather than booting a
  second one (a 201) — a stable identity that outlives a single request without
  storing an id. A duplicate name without `get_or_create` is a 409. The name frees
  when the sandbox stops and survives a suspend/resume. On the three SDKs
  (`GetOrCreate`).
- **TTY executions.** An execution created with `tty: true` runs against a
  pseudo-terminal in the guest instead of pipes: the process sees an interactive
  terminal (line editing, colour, programs that refuse to run without one), and
  stdout and stderr are merged into one stream. Initial size via `rows`/`cols`,
  resized live with `POST /sandboxes/{id}/executions/{exe}/resize` (guest SIGWINCH).
  The pty is allocated with raw Linux ioctls — no new dependency. On the three SDKs
  (`resize`).

- **Cold-start optimizations** (three phases):
  - *Warm build caches baked into the language images.* Each image ships a
    prewarmed cache at a fixed `/opt` path in the read-only rootfs; the guest's
    tmpfs overlay reads the baked entries and copies a sandbox's own output up,
    so the cache costs nothing at boot. Go's `GOCACHE` is prewarmed with
    `go build std` — a cold first build recompiles the whole stdlib it touches
    (~37s on a Pi 5), which drops to ~0.5s warm. Node gets a warmed
    `NODE_COMPILE_CACHE`; Rust links with `mold`.
  - *Warm pool of pristine pre-booted VMs* (`-warm image:vcpus:mem:count`). Each
    pooled VM is a distinct, never-used microVM, so serving one preserves the
    one-sandbox-per-task invariant; the pool refills in the background.
  - *Firecracker snapshots* (`-snapshot-dir`). VMs boot with the control API
    socket so they can be paused and snapshotted; the warm pool fills by restoring
    from a per-shape template snapshot. Every restore rotates the guest's CSPRNG
    before the VM is reachable, and a restore that cannot is destroyed rather than
    handed out (a snapshot is a copy of RAM, so restored VMs would otherwise share
    keys — see `internal/vmgenid` and `internal/agent/reseed.go`). Validated
    end-to-end on a Pi 5 (arm64, GIC-400, Firecracker v1.16.1): **a restored guest
    answers in ~12 ms** (36 two-vCPU restores, 4–83 ms, on a host simultaneously
    running other tenants' work at load average 7–13), a warm pool configured with
    `-snapshot-dir` fills entirely from restores with no cold boots, and sandbox
    creation from it takes 41–140 ms.

    What restore is, precisely: a fresh VMM in a fresh jail, resumed from a
    template captured from a pristine VM the pool cold-booted itself, with its own
    vsock socket, its own host listener and its own CSPRNG. What it is **not**,
    and none of this is planned-and-nearly-there:

    For the warm pool the restore is unnetworked (a networked shape cold-boots
    instead, because one template restored many times would hand out one address).
    Resume-after-stop, added later in this changelog, does restore networked
    sandboxes — one snapshot, one restore, its own netpool slot — so the pool's
    "no networking" is a pool policy rather than a limit of the mechanism. What
    still holds for both:

    - **No surviving a restart.** A snapshot's only handle is an in-memory ref, so
      templates and suspended sandboxes' snapshots are captured per daemon run,
      reclaimed at shutdown, and swept at startup. Snapshots are scratch, not
      storage.
    - A restored guest keeps the template's clock, so its `/proc/uptime` is the
      snapshot's rather than wall-clock.

    Of the family people expect snapshots to unlock, **resume-after-stop, named
    sandboxes and get-or-create are now built** (see Added). Sandbox persistence
    across a daemon restart, fork, and sessions are not — each needs what this does
    not have: durable snapshot metadata, or an API identity spanning several VMs.
- **Verified boot (dm-verity)**: images built with `MICROVM_VERITY=1` ship a
  hash tree and a root-hash sidecar next to the `.ext4`, and the daemon boots
  them as a dm-verity device — the guest kernel verifies every block of the
  shared, read-only rootfs against the hash tree and panics before init if it
  was tampered with. Opt-in per image (auto-detected from the sidecar), backward
  compatible, and requires a guest kernel with `CONFIG_DM_VERITY`/`CONFIG_DM_INIT`.
- **Python SDK** (`sdk/python`, package `microvm`): standard-library-only client
  with the same shape as the Go and TypeScript SDKs — sandboxes, executions,
  files, tasks, queue, images, tenants, `run`, streaming, pagination, typed
  errors, retries, and an observability hook.
- **Request-level retries** in all three SDKs: transient failures (network
  errors and 429/500/502/503/504) are retried with exponential backoff, full
  jitter, and any `Retry-After` honoured. Only idempotent requests are retried —
  GET/PUT/DELETE always, POST only with an idempotency key. Configurable via
  `WithMaxRetries` / `maxRetries` / `max_retries` (default 2).
- **Observability hooks**: `WithObserver` (Go), `onResponse` (TS),
  `on_response` (Python), called once per HTTP attempt for logging, metrics or
  tracing. A `User-Agent` now carries each SDK's name and version.
- **Go ergonomics**: exported `microvm.Ptr[T]` for setting the generated
  optional (pointer) fields, and `Executions.All` pagination to match the
  TypeScript SDK.
- **All three SDKs cover the new routes**: `sandboxes.extend`, the `tags` filter on
  the sandbox list, `files.write_batch`/`createBatch`, and `files.mkdir`. They are
  also retried on a transient failure despite being POSTs that take no
  `Idempotency-Key`, because repeating them is a no-op by construction — extending
  never brings a deadline forward, a write is an overwrite, `mkdir -p` on an
  existing directory is success. Without that, the 429 a per-tenant rate limit
  answers with would reach calling code as a hard failure on exactly the routes
  used to upload a project.
- **Docs**: a README for each SDK, and runnable, compile-checked examples in the
  Go SDK (`example_test.go`).
- **CI guard**: `api/check-generated.sh` fails the build when the generated SDK
  types drift from `api/openapi.yaml`.

### Fixed

- **Security: a restored VM's CSPRNG was not actually being rotated.** The reseed
  wrote 16 unique bytes into the guest's `/dev/urandom` and reported success. On
  Linux ≥ 5.18 — every guest kernel this ships — that write reaches
  `mix_pool_bytes()` and stops: it credits no entropy and it does not re-derive
  `base_crng.key`, which is the key `getrandom(2)` answers from. A snapshot
  restores that key, its generation counter, its 60-second jiffies deadline *and*
  the jiffies identically into every restore, so **two sandboxes restored from one
  template produced byte-identical `secrets.token_hex`, TLS private keys and
  session tokens** for the same first minute of guest execution — longer than most
  sandboxes live. This is exactly the break `internal/vmgenid` documents, and it
  was open on the one platform the snapshot path was validated on: Firecracker's
  VMGenID device, which does force a reseed, is x86_64/ACPI-only, and arm64 uses a
  device tree. Reseeding is now a dedicated guest route (`POST
  /v1/snapshot/reseed`) that mixes the token in *and* forces the re-derivation
  (`ioctl(RNDRESEEDCRNG)` — the agent is PID 1 root, so `CAP_SYS_ADMIN` was never
  the obstacle the old comment claimed), and it fails if the forcing fails. A
  restore that cannot rotate is destroyed and the shape falls back to cold boots.
  Requires a rebuilt guest image: an image without the route is refused rather
  than restored unsafely.
- **Security: a template's memory image was writable by every VMM that restored
  from it.** Firecracker writes a snapshot as the unprivileged uid every jailed VMM
  on the host shares, and a restore hardlinked `mem` and `state` into the restoring
  sandbox's own chroot and then chowned the tree — handing each sandbox's VMM
  ownership, and so write access, of the shared template inode. A VMM that escaped
  its guest (the event the jailer and that uid exist for) could open `/mem` `O_RDWR`
  inside its own jail and choose the guest RAM — kernel text included — that every
  later restore of that shape boots with, and nothing would notice: no restore
  re-hashes what it stages, so the snapshot digest was never an integrity check
  anywhere. Snapshot files are now root-owned and mode `0444` from the moment they
  are collected, and re-sealed after the jail chown. Firecracker only reads them
  (the memory file is mapped private, which is why one template can serve many
  restores), so read-only costs it nothing.
- **A partially repaired restored guest was reported as ready.** Readiness is one
  HTTP reply, answered on whichever vCPU the guest scheduled it on; the
  interrupt-controller state a GICv2 snapshot loses is banked *per* vCPU. Arming
  leaves a pinned spinner per vCPU plus at least one unpinned runtime thread on the
  same vCPUs, so at least one spinner is off-CPU when the host pauses — and its
  vCPU came back with no timer and no reachable SGIs, so the sandbox passed its
  health check and wedged on its first cross-CPU call. Worse, the stand-down set
  the stop flag before waiting, so a spinner scheduled late read "stop" and exited
  without ever looking at its registers, turning that race from transient into
  permanent. Each spinner now checks and repairs its own vCPU before it honours a
  stop, the disarm reports how many vCPUs were accounted for, and a restore whose
  stand-down does not add up destroys the VM instead of shipping it.
- **A failed stand-down no longer drops the carry on the floor.** `disarm` cleared
  its session handle *before* waiting on it, so a wait that timed out left a retry
  being told "nothing to stand down" while spinners were still running, and a later
  arm recording the *armed* `GOMAXPROCS` and GC settings as the ones to restore
  afterwards. The session is now only forgotten once the wait has succeeded.
- **One slow restore no longer disables snapshots for a shape for the daemon's
  life.** A snapshot failure latched the shape to cold boots and nothing ever
  cleared it, so a single restore that faulted its memory image in from a cold page
  cache permanently disabled the fast path. Failures now cool down (30 s, doubling,
  capped at 10 minutes) and only become permanent after five consecutive ones,
  which is a host that cannot do this rather than one bad restore. A failure also
  discards the shape's template, since a stale template is a candidate cause.
- **Restore no longer leaks host resources on failure.** Eight bare error returns
  after the jail existed left behind, between them, the jail tree, its hardlinks to
  the shared template, and — once the inbound listener was open — a bound host
  socket in a directory whose VM never existed. It now rolls back through
  `inst.Stop`, exactly as the cold-boot path does.
- **Restore is bounded.** The wait for Firecracker's control socket polled the
  caller's context and ignored the VMM's own exit, and the reseed was issued on a
  client that deliberately sets no timeout. From the warm pool the caller's context
  has no deadline, so a jailer that died on exec — or a guest that answered health
  and then wedged — blocked the single refill goroutine for the daemon's life, with
  no log line and no fallback to cold boots for *any* shape. There is now one
  deadline over the whole restore plus a bound on every step, and the socket wait
  ends when the process does.
- **Snapshots are reclaimed.** Every capture created a directory under
  `-snapshot-dir` that nothing ever deleted: one full copy of a guest's RAM per
  shape per daemon run, orphaned on restart, until the disk it shares with the log
  store and the object cache filled. Templates are now discarded when the pool
  closes and when a shape's snapshot path fails, and any left by a previous run are
  swept at startup. (This does mean one `-snapshot-dir` per daemon.)
- **A restore no longer resumes onto an image that has been rebuilt underneath
  it.** A snapshot references its block devices rather than containing them, so a
  restore staged whatever stood at the image path *today* — and an operator
  rebuilding an image in place, the documented way to update one, left every later
  restore resuming a guest whose page cache and inode state described a different
  filesystem: EIO and silent corruption behind a health probe that passes. A
  template now records which file it was captured from and refuses a mismatch.
- **Pool residency is no longer billed to the tenant.** A pre-booted VM's meters
  started when the VM was minted, so a sandbox served from the pool after ten
  minutes reported ten minutes of billable wall and ten of idle against its first
  tenant — next to a `created_at` stamped at checkout, making `stats.wall` larger
  than the sandbox's whole apparent life. The meters restart as the VM leaves the
  pool.
- **Capture no longer hashes the guest's entire memory image.** The snapshot digest
  is consumed by exactly one thing (binding a restore token whose uniqueness comes
  from `crypto/rand` anyway), is never verified and never leaves the host, yet it
  was read over the whole `mem` file on the warm pool's single refill goroutine
  ignoring cancellation — four and a half minutes for an 8 GiB guest at the 30 MB/s
  floor `internal/fcapi` cites. It now covers the state file, respects its context,
  and says in its own doc comment that it is an identifier and not an integrity
  check.
- **Snapshot restore on arm64 GICv2 hosts** (a Raspberry Pi 5 and anything else
  with a GIC-400 rather than a GICv3). A restored guest used to answer nothing at
  all, and the warm pool fell back to a cold boot after paying 90 seconds per
  shape. It was never a vsock problem: Firecracker's GICv2 snapshot path does not
  save the guest's *banked*, per-vCPU interrupt-controller state — the guest came
  back with the ARM virtual timer PPI (INTID 27) disabled and pending forever on
  every vCPU, and with the whole GIC CPU interface switched off on secondary vCPUs
  (`GICC_CTLR=0`, `PMR=0`). An arm64 microVM has no other clockevent device, so
  the guest ran userspace at full speed with a correct clock while nothing that
  waited on time ever completed again: the guest kernel accepted the host's vsock
  connection in under a millisecond and the agent never got as far as writing a
  reply. Only the guest can write those registers, so it now carries them across
  the snapshot itself — the host arms the guest immediately before pausing it
  (`POST /v1/snapshot/arm`), each vCPU holds its state and reapplies it in the
  first instruction it executes on resume, and the host stands the carry down as
  soon as the restored guest answers (`POST /v1/snapshot/disarm`). On a host that
  restores the state correctly (x86, or arm64 with a GICv3) the guest reports that
  it has nothing to carry and nothing happens. Requires a rebuilt guest image;
  `internal/agent/gic_linux.go` documents the defect and the measurements.
- **Snapshot capture no longer times out on a slow disk.** `internal/fcapi` put a
  single 30-second timeout on every Firecracker API call, but a snapshot write is
  the guest's whole memory going to disk — from inside a cgroup capped near the
  guest's memory size, so its page cache is reclaimed as it goes. A 512 MiB guest
  measured over 30s on a busy SD card, which surfaced as "snapshot path failed"
  and permanently disabled snapshots for that shape; a larger guest could never
  have captured at all. Control calls keep a 30-second bound; the snapshot write
  gets its own.
- **A failed restore now says why.** The readiness error carries the guest console
  (as the cold-boot path already did) instead of being thrown away by the jail
  teardown, `waitReadyWithin` no longer discards the readiness poller's own error
  in a race with its deadline, and a restore that cannot open its host-side vsock
  listener logs it instead of silently having no storage.

### Scheduling (daemon)

- Task scheduling is now **resource-aware**: a node leases only tasks that fit
  its free CPU and memory (`-cpu`, `-mem`), so a fleet can mix task sizes without
  oversubscribing any box. `-slots` caps the concurrent VM count on top.
- **Task priority 0–10** (higher first, ties FIFO), bounded and validated at the
  API.
- **Reservation**: when the head task fits no node, one node (chosen through
  Redis) reserves it and drains while the rest of the fleet keeps working, so a
  large task is never starved by the small ones behind it.

### Admission control (daemon)

- **Per-tenant limits**, both off by default: `-tenant-max-sandboxes` caps how
  many sandboxes one token may have running at once, and `-tenant-max-rps` caps
  its request rate (token bucket, bursting one second's worth). Charged to a
  *tenant* rather than to a token or an address: an address is not an identity, so
  one tenant behind a NAT would be charged for its neighbours and one with a proxy
  pool for almost nothing. `microvmd` derives a tenant per token, so in this daemon
  the two coincide and **N keys are N allowances** — sharing a tenant across several
  keys is a `Principals` configuration the flags do not expose yet. Over either
  limit a caller gets the existing `capacity_error` 429 — no new error type, so
  every SDK's retry logic is unchanged. Only the rate limit adds `Retry-After`:
  when someone else's sandbox ends is not something this node knows.
  `POST /v1/tasks` is **not** covered by `-tenant-max-sandboxes` — the counter is
  one node's and a task is scheduled across the fleet, so a task enqueued here may
  run anywhere; see DEPLOY.md.
- **A sandbox belongs to the tenant that created it.** Every route that names one
  resolves it against the caller, so another tenant's sandbox is a `404` — not a
  `403`, which would confirm which of a guessed range exist. Before this an ID was
  a capability: any valid token could exec in, delete, or read files out of any
  sandbox on the node, and reading files means reading the owner's whole
  object-store namespace, which is mounted inside it. `GET /v1/sandboxes` is
  scoped the same way. An admin key keeps the node-wide view.
- **Tokens from a file or the environment**: `-tokens-file` /
  `-admin-tokens-file` and `MICROVM_TOKENS` / `MICROVM_ADMIN_TOKENS`. Sources add
  up, so moving off the flags is a rotation and not a cutover. `-tokens` and
  `-admin-tokens` still work and are **deprecated**: a secret on a command line
  is a secret in `ps`, in shell history, and in the unit file.

### Sandboxes

- **`POST /v1/sandboxes/{id}/extend`** buys a running sandbox more time, for work
  that turned out longer than the caller guessed. The deadline is bounded by the
  host's maximum lifetime measured **from creation**, never from now, which is what
  makes extension buy time and never immortality: a caller that heartbeats forever
  still ends up with a sandbox that dies. Asking past the bound is a `400` naming
  the seconds left rather than a silent trim — a caller told `200` for an hour they
  did not get plans for an hour. It never brings a deadline forward, so it takes no
  `Idempotency-Key` and a retry is a no-op. The idle timeout is untouched: a long
  TTL does not keep an idle sandbox alive.
- **Tags** (`tags` on create, `?tag=key:value` on the list, repeatable and ANDing).
  Labels for finding a sandbox again, capped at 10 pairs of 64/256 bytes because
  they are held for the sandbox's whole life. Node-local, like the list they filter:
  finding a tagged sandbox across a fleet means asking each node. Nothing about
  them reaches the guest, which is what separates them from `env` — a tag names a
  sandbox, it does not configure one — and unlike `env` they are returned, so a tag
  is a label and never a secret.
- **`POST /v1/sandboxes/{id}/files/batch`** writes up to 100 files in one request,
  and **`POST /v1/sandboxes/{id}/dirs`** creates a directory and its parents.
  Validation is all-or-nothing and writing is not, because writing cannot be: a
  batch with one bad mode writes nothing, the files go in the order given, and a
  batch that fails partway names the entry it stopped at. Each path may be named
  once. `dirs` exists for the directory that stays empty, since an upload already
  creates its parents.
- **Network transfer counters** on `stats` (`network_rx_bytes`,
  `network_tx_bytes`), read from the sandbox's TAP device and turned round to the
  guest's point of view — what the host received off the TAP is what the guest
  sent. Absent rather than zero for a sandbox created with `network: false`: it
  transferred nothing measurable, which is a different statement from having
  measured zero. Sampled with the cgroup just before the kill, because the device
  and its counters die with the VM.
- **A file transfer counts as activity.** Only executions used to touch the idle
  clock, so a caller staging a project — minutes of uploads with nothing run in
  between, which is exactly what the batch route is for — was idle by the manager's
  reckoning and had the VM reclaimed underneath it. A download holds the sandbox
  open until its reader is closed, not until the call returns.

### Seeding a sandbox from a source

- **`source` on create** seeds the sandbox before the call returns, so the first
  execution already has the project. Getting code in was otherwise one HTTP
  request per file, or a clone inside the guest — which needs `network: true`, is
  impossible for a network-isolated sandbox, and puts the caller's credential
  inside a VM that is untrusted by design. Two types: `tarball` (`.tar`/`.tar.gz`,
  with `strip_components`) and `git` (`ref`, `depth`, `credential_ref`).
- **The daemon fetches and expands host-side, then writes the tree in over the
  same file endpoints an upload uses.** Nothing was added to the guest agent and
  no image changed: the agent is compiled into every rootfs, so a new guest
  endpoint would mean rebuilding and redistributing every image before the feature
  worked anywhere — and the existing write path already carries a mode and already
  counts as activity, which is exactly what a seed needs.
- **Off unless the operator turns it on**, with `-source-fetch` *and* at least one
  `-source-allow-host`; either missing refuses every source. The gate is the
  feature: the daemon runs outside the firewall it installs for its guests, so a
  URL it will fetch on request is an SSRF primitive pointed at the host's LAN, its
  own loopback API, and the cloud metadata service that holds the host's identity.
  Two independent checks stand in front of every connection — the resolved
  addresses are vetted before the dial, and vetted again inside the dialler's
  `Control` on the syscall path, which closes the DNS-rebinding window between
  them. The blocked ranges are the same set `internal/netpool` gives a guest, kept
  in step by a parity test. https only, no proxy (not even the environment's),
  every redirect hop re-checked against the allowlist.
- **Bounded by the operator**: `-source-max-bytes` on the download,
  `-source-max-expanded-bytes` on what is written, `-source-max-file-bytes` on one
  file inside it, `-source-max-files` on the member count (which also bounds what
  the member names may total), `-source-timeout` on anything a third party controls,
  and `-source-temp-dir` for where a seed is staged — point it at a real disk where
  `/tmp` is a tmpfs. Every cap defaults to a conservative number rather than to
  unlimited, so a flag nobody wired up refuses a hostile archive instead of
  admitting one.
- **A git clone is bounded by those byte caps too**, which needs saying because git
  has no option for it: no `--max-bytes`, and `--depth 1` says nothing about how
  large the one commit it fetches is. The clone's temporary directory is measured
  while git is writing it and the clone is killed at `-source-max-bytes` +
  `-source-max-expanded-bytes` — the packfile bounded like a compressed body, the
  tree it expands to like an expanded archive. During, not after: "the caps were
  satisfied" is worth nothing once the disk is full.
- **Concurrent fetches are bounded node-wide** at 8, answered past that with the
  same `capacity_error` 429 a full node gives. `-tenant-max-sandboxes` is not that
  bound — it defaults to unlimited, and a fetch happens before the node's own
  admission has been asked whether there is room for a VM.
- **Seeding is all-or-nothing.** Fetch, expand and write in that order; a failure
  at any stage destroys the VM and fails the create, so no half-seeded sandbox is
  ever returned, listed or billed. The error names the stage — `fetch`, `expand`
  or `write` — because the three are fixed by different people. Refusals decided
  before a byte leaves the host are one `source_not_permitted` (400) with one
  message, since a caller able to tell "no such host" from "that is a private
  address" would have a port scanner with the host's routing table; an origin that
  was reached and misbehaved is `source_fetch_failed` (502).
- **The fetch happens before the VM boots**, so a caller is never billed for
  somebody else's slow web server: metering is cumulative from boot, and a seed
  fetched after it would land in the sandbox's wall clock as idle time. The writes
  that follow do count as activity, which is what stops the idle reclaim taking
  the VM away mid-seed.
- **A source too big for the sandbox is a request error, not a mystery `ENOSPC`
  from inside the guest.** The writable layer is a tmpfs stacked over the
  read-only image, so both `disk_mib` and `mem_mib` bound it and the smaller wins;
  a tree that will not fit is refused before the VM boots, with the numbers and
  the fields that would fix it.
- **A private repository never hands its credential to the guest.** The clone is
  host-side and only the working tree is copied in — `.git` is not written at all.
  `credential_ref` names a credential the operator configured with
  `-source-credential name@https://host/prefix/=/path/to/file`; the secret never
  enters the request,
  the URL, or argv (`/proc/<pid>/cmdline` is world-readable, so it reaches git
  through an askpass helper reading the environment). git is run with a
  built-from-nothing environment, https only, redirects refused, submodules not
  recursed, and pinned to the addresses this daemon vetted, so git never resolves
  the name itself. A host with no `git` on `PATH` — or one older than 2.31, which
  ignores the `http.curloptResolve` those pins are made of and would silently do its
  own DNS instead — serves tarballs only and says so at startup.
- **A credential is bound to a URL prefix, and that binding is the point of the
  flag.** The map is shared by every caller, so a credential resolved by name alone
  would be a confused deputy twice over: it spends the operator's token on any
  repository a caller names, since the authenticated working tree lands in a sandbox
  the caller reads, and it hands the token *itself* to whatever host answers, since
  git offers its credential to anything that challenges for one and an allowlist
  entry can be a suffix. A `credential_ref` outside its prefix is refused in the same
  words as one that does not exist, so the names and the repositories behind them
  cannot be enumerated either. Matching is host-then-path at a component boundary,
  so `.../acme` does not cover `.../acmecorp`.
- **Seeding does not spend `ttl_seconds`.** The TTL is time to run things in, so the
  clock is restarted once the tree is in the guest; a slow fetch can no longer hand
  back a sandbox whose `expires_at` elapsed during the create. A `ttl_seconds`
  shorter than the seed is refused naming that field rather than surfacing as a 500,
  and a sandbox the TTL claimed mid-seed can no longer be published as running.
- **An archive whose members contradict each other is refused**, not written: a
  file and a directory of one name, or a member inside a path another member holds
  as a file. Each is fine on its own and they cannot both be written, so validating
  members one at a time left the destination to answer the second write with
  `ENOTDIR` — a 500 for an archive the caller controls entirely.
- **Member names are bounded**, at `PATH_MAX` each and 256 bytes per member across
  the archive. A name is not content, so nothing counted it: a tar can carry a
  megabyte of it per member, the validated manifest retains every one, and the
  archive that asks for it compresses to almost nothing. Refusals quote an elided
  name for the same reason.
- **Symlinks, hard links, devices and anything not a regular file are refused**
  rather than skipped, in an archive and in a checkout alike: the guest file
  endpoints cannot express them, and a project silently missing a link is a bug
  someone debugs for an hour. Member paths are validated, never joined to a host
  path — the download is buffered into one unlinked temp file, so host-side path
  traversal is absent rather than mitigated.
- **All three SDKs build a source**: `TarballSource`/`GitSource`,
  `tarballSource`/`gitSource`, `tarball_source`/`git_source`, alongside the
  generated `source` field. They set the type, and leave out what the caller did not
  choose — a `strip_components` of 0 and an empty `ref` are the server's own
  defaults, and sending them would be a caller appearing to have decided something.
  Each also names the two failures worth branching on:
  `IsSourceNotPermitted`/`isSourceNotPermitted`/`is_source_not_permitted`, which no
  retry fixes because an operator has to act, and `IsSourceFetchFailed` and friends,
  which one might. No new error *type*, so no SDK's existing switch changed.
- **`microvm exec go go test ./... -source git=https://host/acme/widgets`** in the
  CLI, with `-source-ref`, `-source-strip` and `-source-credential`. The type is
  written out rather than inferred from the URL: the same path can name a repository
  and a tarball, and inferring the wrong one seeds the wrong thing and reports
  success. A modifier belonging to the other type is passed through to be refused by
  name, not dropped — someone who wrote `-source-ref` with a tarball meant something
  by it — and a modifier with no `-source`, or a `-source` on `microvm submit`, which
  has no sandbox to seed, is an error rather than a flag that vanishes. A refused
  source prints the two flags an operator would have to set, since the API's message
  deliberately does not say which reason applied.

### Retention (daemon)

- **Stopped sandboxes are reaped**: `-sandbox-retention` is how long one stays
  listed and retrievable before the daemon forgets it, default `0` = forever, so
  an existing node behaves as it did. Nothing removed a stopped sandbox from the
  manager before, so a daemon that never restarted grew its map and its
  `GET /sandboxes` pages for the life of the process. Running sandboxes are never
  swept, whatever their age. The window is raised to `-log-retention` when set
  below it, rather than being a second flag to keep in order: the stopped record
  carries the final metering — the only record of what the sandbox cost — and every
  exec record is reached through its sandbox. Past the window the ID answers the
  ordinary `sandbox_not_found` 404. A forgotten sandbox takes its exec records with
  it, because it was the only handle on them: `-log-retention 0` means "keep output
  forever", so without that the records would outlive every route able to reach
  them.

### Storage (daemon)

- Object storage is presented to a sandbox as a FUSE filesystem at
  `/mnt/storage` (configurable); the guest holds no credential.
- Per-tenant limits with `evict`/`preserve` policy, set by an admin token via
  `/v1/tenants`. Redis-backed tenant policy store for fleets.
