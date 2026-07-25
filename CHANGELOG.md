# Changelog

Notable changes to the microvm daemon and its SDKs. The SDKs are versioned
together at the version below; each is released to its own registry (pkg.go.dev,
npm, PyPI) by tagging this repository.

The format follows [Keep a Changelog](https://keepachangelog.com), and the
project aims for [Semantic Versioning](https://semver.org).

## [Unreleased]

### Added

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
  - *Firecracker snapshots* (`-snapshot-dir`, **experimental**). VMs boot with the
    control API socket so they can be paused and snapshotted; the warm pool then
    tries to fill by restoring from a template snapshot. Every restore reseeds the
    guest's entropy (a snapshot is a copy of RAM, so restored VMs would otherwise
    share a CSPRNG — see `internal/vmgenid`). Validated end-to-end on a Pi 5:
    snapshot *create* and the VMM *restore/resume* work, but the restored guest
    agent does not re-establish its vsock session (the hard, still-open part of
    snapshot restore, shared with the prior art), so the pool falls back to a cold
    boot for now. Off by default and safe: the fallback keeps the warm pool
    working via cold boots.
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
