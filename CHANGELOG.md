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

### Storage (daemon)

- Object storage is presented to a sandbox as a FUSE filesystem at
  `/mnt/storage` (configurable); the guest holds no credential.
- Per-tenant limits with `evict`/`preserve` policy, set by an admin token via
  `/v1/tenants`. Redis-backed tenant policy store for fleets.
