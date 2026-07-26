# microvm

[![npm](https://img.shields.io/npm/v/@pablofdezr/microvm?logo=npm)](https://www.npmjs.com/package/@pablofdezr/microvm)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)
[![Website](https://img.shields.io/badge/website-pablofdezr.github.io%2Fmicrovm-e0521c)](https://pablofdezr.github.io/microvm/)

Run untrusted code — Go, Python, TypeScript, Rust — in Firecracker microVMs, on
your own hardware. Written entirely in Go.

The code inside a sandbox is assumed hostile. Every design decision below falls
out of that.

## Why Firecracker and not V8 isolates

An isolate only runs JavaScript and WASM. Go, Python and Rust would each need a
compromised WASM path — a Python interpreter compiled to WASM, a Go binary with
its own GC bundled in. Four mediocre experiences instead of one good one.

Firecracker gives every sandbox its own kernel. A fork bomb, a runaway
recursion, a memory bomb: all of them can only burn the VM's own fixed vCPU and
RAM allocation. The host never notices. That is not something a container can
promise.

## Architecture

Ports and adapters. The core knows what a sandbox is and when it dies; it does
not know that Firecracker, Redis or HTTP exist.

```
        driving adapter                    core                    driven adapters
  ┌───────────────────────┐      ┌──────────────────────┐    ┌──────────────────────┐
  │ api/       REST + SSE │─────▶│ sandbox   lifetimes  │───▶│ firecracker  jailer, │
  │ cmd/microvm      CLI  │      │ logstore  output     │    │              cgroups │
  └───────────────────────┘      │ pool      N slots    │    │ redis / memory queue │
                                 └──────────────────────┘    │ netpool  TAP, nftables│
                                          ports:              └──────────────────────┘
                                   runtime.Runtime                      │
                                   runtime.GuestClient                  ▼  vsock
                                   queue.Queue              microvm-agent (PID 1, guest)
```

Dependencies point inward, always. That is not decoration: `runtime.Instance`
used to hand back a concrete `*guestclient.Client`, and the effect was that
nothing above it could be tested without KVM — a fake was impossible to write,
so the sandbox manager and the entire API were only ever exercised by hand on
one Raspberry Pi. Turning that one return type into a port is what made
`internal/runtime/runtimetest` possible, and with it the API's test suite.

The control channel is **HTTP over AF_VSOCK**. Firecracker exposes the guest's
vsock as a Unix socket with a `CONNECT <port>` handshake, so a custom
`DialContext` lets both sides use stdlib `net/http`, streaming included. No
gRPC, no protobuf, no network path into the guest at all.

## The spec is the source of truth

`api/openapi.yaml` is the contract. The server's wire types, both SDKs and the
reference docs are generated from it:

```
./api/generate.sh     # validates the spec, then regenerates all three
```

Never edit a `*.gen.*` file — the next run overwrites it. One spec, three
artefacts, and nobody keeping two copies in step by hand.

The conventions are Stripe's, because they are the ones a developer already
knows: plural resource nouns, an `object` field on every resource, a `list`
envelope, errors under one `error` key, cursor pagination, `Idempotency-Key` on
every unsafe method. Two deliberate departures, both where Stripe's choice is
a legacy artefact rather than a good idea:

- **Timestamps are RFC 3339, not Unix seconds.** This system meters in
  milliseconds and routinely runs sandboxes that live under a second. Unix
  seconds would make `created` and `stopped` identical for them.
- **IDs are time-sortable** (`sb_01JZ8QK3M4N5P6R7S8T9V0W1X2` — a prefix and a
  ULID). Stripe's IDs carry no order, so `starting_after` forces the server to
  look the cursor object up and page relative to it; a cursor whose object was
  deleted strands the caller. A sortable ID *is* the position, so pagination is
  exact and survives deletion.

The spec is OpenAPI **3.0.3** rather than 3.1 for one reason: `oapi-codegen`
does not support 3.1 and fails outright on `type: [integer, "null"]`. A spec
that cannot generate code is documentation that rots, and hand-writing the Go
types would reintroduce exactly the duplication the spec exists to remove.

## Scaling: one VPS or three hundred

The queue is the source of truth. Nodes are dumb: each one pulls the
highest-priority task it has room for, and nothing ever tells a node what to run.

That is the whole scaling story. A push-based scheduler would have to know how
many nodes exist, how loaded each one is, and what to do when one dies
mid-assignment — state that is stale the moment it is written, and a component
whose failure stops the fleet. With pull, adding the 300th node requires no
coordination: it starts pulling. Losing a node needs no detection: it stops
pulling, its leases expire, and its work returns to the queue.

**Pulling is resource-aware, so a fleet can mix task sizes.** A node advertises
its free CPU and memory when it asks for work, and the queue hands back the
highest-priority task that *fits*. Set a node's budget with `-cpu` and `-mem`;
`-slots` caps the VM count on top, for the fixed per-VM overhead. Leave the
budgets unset and packing falls back to the slot count alone, which is right only
when every task is the same size. Memory is the dimension that must not
oversubscribe: a microVM reserves real RAM, so packing it is what keeps one
node's tasks out of another tenant's OOM.

A task carries a **priority from 0 to 10** (higher first, ties FIFO). Priority
orders the queue *within what a node can fit*: capacity wins when they disagree,
because a high-priority task no node can place helps no one by stalling the ones
that can run.

**Big tasks are reserved, not starved.** When the head task fits no node right
now, the fleet does not keep backfilling small tasks past it forever. The first
node that *could* run it (it fits that node's total budget) reserves it: that one
node drains, taking nothing, until the task fits — while every other node keeps
pulling work it can run. So a large task waits for one box to clear, not for the
whole fleet to idle, and the small tasks behind it never overtake it
indefinitely. The reservation is owned by one node (coordinated through Redis)
and released the moment the task is placed or the draining node dies. Only the
head is reserved; the next big task's turn comes once the head is running — so
very large tasks run one at a time, which is the deliberate v1 trade-off against
reserving several nodes at once.

```
  10,000 tasks ──▶ [ priority queue ] ◀── pull ── node A (16 cpu, 32 GiB)
   (mixed sizes)                       ◀── pull ── node B (8 cpu, 16 GiB)
                                       ◀── pull ── node C (4 cpu, 8 GiB)
       each node takes the highest-priority task that fits its free resources
```

`queue.Queue` is an interface with two implementations. The in-memory one is
correct for a single host and wrong for a fleet: nothing survives a restart and
no other host can see it. Redis is one flag away:

```
microvmd -redis redis:6379 -redis-prefix microvm -slots 10 -cpu 8 -mem 16384
```

Both pass the same conformance suite (`internal/queue/conformance_test.go`),
which is what turns "drops in behind the same interface" from a hope into a
fact: FIFO, exactly-once delivery, lease expiry, retries, idempotent enqueue,
and resource-aware leasing (a task too big for a node is stepped over, not lost).
Verified on two real nodes sharing one Redis — six tasks submitted to node A
alone, three ran on A and three on B, with no coordination between them, and
the queue survived killing both.

Redis keys are hash-tagged (`{microvm}:pending`) so Cluster maps them all to one
slot. That looks like giving up sharding, and it is: a queue with a global order
cannot be sharded, because "the next task" is a question about every task at
once. The tag makes the scripts legal under Cluster rather than failing with
CROSSSLOT at runtime — and the queue is not the bottleneck anyway, since a slot
takes seconds of VM time to serve.

Every compound operation is a Lua script, because each one is read-then-write:
lease reads the head and marks it taken. As two commands, two nodes both pop the
same task — the exact failure the lease exists to prevent, reintroduced one
layer down.

## Security

| Layer | What it stops |
|---|---|
| Guest kernel | The primary boundary. Guest root is not host root. |
| Jailer | chroot, PID namespace, seccomp (~40 syscalls), non-root uid. The second barrier, for if the first fails. |
| nftables | Egress reaches the public internet and nothing private. |
| cgroup v2 | Hard CPU, memory and PID ceilings, per sandbox and for all of them together. |
| /30 per sandbox | Two sandboxes are never on the same link. Isolation comes from the topology, not from a rule being correct. |

**Egress is filtered, not open.** A sandbox can `pip install`; it cannot reach
RFC1918, link-local, or `169.254.169.254`. Allowing the first without the second
is what keeps a sandbox from scanning your LAN or reading cloud credentials.

**The ceiling is nested.** Per-sandbox limits are what a caller asks for; the
slice ceiling bounds all sandboxes together. Without it, the host's safety would
depend on every individual limit being computed correctly — a bet that
eventually loses.

**Rate limits live in the VMM**, not on the host, so a guest cannot route around
them: there is no interface to reconfigure and no queue to jump. Measured:

```
disk:  24MB read in 6.04s = 4.0 MB/s  against a 4 MB/s cap
net:   1MB down in 3.72s  = 282 KB/s  against a 200 KB/s cap
```

The network default is on for a reason CPU limits do not cover: a sandbox pinned
to a quarter core can still saturate the host's uplink, and the first you hear of
it is the abuse complaint.

**Environment variables** can be injected per sandbox (inherited by every exec)
or per exec, with the more specific winning. They are applied by the host on each
exec rather than written into the guest: writing them in would leave credentials
sitting in the VM's filesystem, readable by anything inside, long after the
command that needed them. They are never logged and no endpoint returns them.

## Metering

Billing is on **active CPU, not wall-clock**, read from cgroup v2's
`cpu.stat`/`usage_usec`. Measured on real hardware:

```
3s of sleeping  →  15ms of active CPU     (0.5% of wall)
3s of spinning  →  2.4s of active CPU
```

A sandbox blocked on I/O bills nearly nothing. `idle = wall − active`.

Two things worth knowing:

- **Stats are cumulative, and booting is expensive** (~2.9 CPU-seconds across
  the VMM's threads). A biller must diff two samples; cumulative idle is
  meaningless for a sandbox's first seconds.
- **Final stats are sampled before the kill.** Once the VM dies its cgroup goes
  with it and the cost is unrecoverable, so "what did this run cost?" is
  answered from a snapshot taken while it still existed.

Transfer is metered too: `network_rx_bytes` and `network_tx_bytes` come from the
sandbox's TAP device, sampled next to the cgroup and for the same reason — stop
deletes the TAP and its counters go with it. They are reported from the *guest's*
point of view, so what the host received off the TAP is the guest's `tx`; naming
them from the host's side would report a sandbox's egress as its ingress, and the
first you would hear of it is an abuse complaint blaming the wrong direction. A
sandbox created with `network: false` reports neither field rather than zero: it
transferred nothing measurable, which is a different statement from having measured
nothing.

## Logs survive the VM

Output is buffered on the **host**, not in the guest. The moment you most need a
run's output is when it was killed — by a timeout, a TTL, the OOM killer — and
output buffered inside the guest dies exactly then.

Statuses distinguish outcomes that all look like failure but are not:

| Status | Meaning |
|---|---|
| `exited` | Your code ran. The exit code is its own verdict. |
| `timed_out` | It exceeded its timeout and we killed it. |
| `vanished` | **We took the VM away.** Your code did not fail. |
| `failed` | The command could never start — a missing binary. |
| `aborted` | You cancelled. |

## Storage

Sandboxes get a filesystem at `/mnt/storage` (configurable per sandbox) backed by
object storage. Code writes files the ordinary way — `open`, `write`, `close`,
`os.listdir` — and they outlive the VM. **The guest never holds a credential.**

The obvious design, s3fs in the guest, is the wrong one here: it needs AWS keys
inside a VM running code we assume is hostile, and a short-lived key is still
"your bucket, for fifteen minutes". So nothing crosses into the guest. The guest
mounts a FUSE filesystem; every `open`/`read`/`write` becomes an HTTP call over
vsock to the **host**, which holds the credentials and makes every S3 call from
its own network namespace. The guest has no S3 client and no network path to the
bucket.

Isolation is the socket, not a token. The host serves each sandbox's storage on a
Unix socket inside that sandbox's jail, created before the VM booted. A request's
identity is *which socket it arrived on* — a fact about the filesystem, not a
claim the guest makes. There is nothing to forge and nothing to steal, so the
storage server has no authentication at all. Your files live under a prefix
derived from your API key; the request body cannot name another tenant's prefix,
only pick a mount path in your own guest.

Object storage is not a filesystem, and this does not pretend otherwise: there is
no atomic rename (it surfaces as a cross-device move, so `mv` copies visibly
rather than the host hiding a whole-object copy behind a cheap-looking call), and
a file open for writing buffers in the guest and uploads once on close, because
S3 has no partial write. Per-tenant limits are set by an admin, not the caller: a
full tenant either rejects writes (`preserve`) or evicts its oldest objects to
make room (`evict`).

**Guest kernel requirement:** the storage mount needs `CONFIG_FUSE_FS=y` (or `=m`
with the module available) in the guest kernel, which is what makes `/dev/fuse`
appear. Without it a sandbox still boots and runs — it just has no storage, and
the agent says so on the serial console rather than failing. The host daemon
needs an S3 bucket configured (see `-s3-bucket`); a node with none simply gives
its sandboxes no storage.

## Images

Slim where a slim variant exists. Measured on a Pi 5 with `microvm bench`, which
times each leg of a full run separately rather than reporting one number for all
of them: 10 iterations after a discarded warm-up, so the image really is hot in
the host page cache. Medians, no warm pool and no snapshots:

| Image | Size | Boot the sandbox | Run the code | Tear down + round trips | Total |
|---|---|---|---|---|---|
| python | 154 MB | 167 ms | 78 ms | 24 ms | **281 ms** |
| node · tsx | 303 MB | 229 ms | 943 ms | 46 ms | **1.20 s** |
| go · Alpine | 620 MB | 255 ms | 841 ms | 53 ms | **1.17 s** |
| rust | 859 MB | 304 ms | 846 ms | 62 ms | **1.15 s** |

The split is the point, and a single total hides it. **Booting the sandbox costs
170–300 ms and barely tracks the image size at all** — a 859 MB rootfs boots in
twice what a 154 MB one does, not six times, because images are hardlinked into
each jail rather than copied. What actually varies is the second column, which is
the code under test compiling itself: a compiler for Go and Rust, the tsx
transform for node. That is 71–79% of those three totals and none of it is a cost
of starting a microVM. Only python's total is mostly sandbox.

Inside that boot, over 42 cold boots across the four images:

| Phase | Median |
|---|---|
| Stage the jail — hardlink kernel and rootfs, render `vm.json`, chown | 0.5 ms |
| Exec the jailer, which execs Firecracker | 0.7 ms |
| Guest kernel, up to the point it execs our init | 82 ms |
| `InitGuest` — overlay root, mounts, network, env, storage | 5 ms |
| VMM start-up before the kernel, the supervisor re-exec, the agent binding vsock, and the 5 ms health-poll granularity | 119 ms |
| **One create, end to end** | **214 ms** |

Host work is ~1 ms of it. The guest kernel is the largest named phase, which is
why sandboxes boot `quiet` (see `-guest-boot-verbose`): every kernel printk is a
synchronous write to an emulated UART that the guest blocks on, so letting the
kernel narrate a successful boot cost 87 ms — quieting it took the guest kernel
from 169 ms to 82 ms and a create from 288 ms to 170 ms. The console stays
attached and the threshold only rises to `KERN_ERR`, so a panic still prints with
its call trace. The 119 ms remainder is the next thing worth attacking.

**Cold starts** are attacked in three layers, each opt-in and independent:

- **Warm build caches** baked into each image. Go 1.20+ ships no precompiled
  standard library, so a cold `go build` recompiles everything it imports, which
  on a Pi 5 is tens of seconds. A `GOCACHE` prewarmed with `go build std`, baked
  into the read-only rootfs and read through the guest's overlay, is what puts the
  841 ms in the table above. (The same cache builds in single-digit milliseconds
  in the image itself; the gap is the read-only rootfs + overlay + virtio-blk the
  guest reads it through, and it degrades further when a busy node's page cache is
  contended — so this is a real win but not the sub-second a raw build sees.) Node
  ships a warm `NODE_COMPILE_CACHE`; Rust links with `mold`.

  The cold, cache-less figures this improves on are not in the table because
  `microvm bench` measures the images as they ship, and they ship with the cache.
  Removing it to quantify the delta is a separate experiment, and the numbers that
  used to sit here predate the harness — they came from wall-clocking whole `run`
  invocations, which is exactly the conflation the table above exists to undo, so
  they are not comparable to it and have been dropped rather than restated.
- **A warm pool** of pristine pre-booted VMs (`-warm image:vcpus:mem:count`), so
  a task skips the boot entirely. Each pooled VM is a distinct VM that has run no
  code, so handing one out keeps the one-sandbox-per-task rule — no snapshot
  collision to fix up.
- **Firecracker snapshots** (`-snapshot-dir`), so the warm pool fills by restoring
  a template instead of cold-booting: a restored guest answers in **~12 ms** on a
  Pi 5 (4–83 ms over 36 restores, on a host busy with other tenants' work).

  A snapshot is a copy of RAM, so every restore rotates the guest's CSPRNG before
  the VM is reachable — and *rotates* is the operative word. Writing fresh bytes
  into the guest's `/dev/urandom` is not a reseed on any kernel since 5.18: it
  mixes into the input pool and does not re-derive the key `getrandom(2)` answers
  from, which a snapshot restores identically into every restore along with the
  jiffies deadline that would have rotated it. So the guest agent mixes the token
  in *and* forces the re-derivation, and a restore that cannot is destroyed rather
  than handed over. See [internal/vmgenid](internal/vmgenid) and
  [internal/agent/reseed.go](internal/agent/reseed.go).

  On an arm64 host with a **GICv2** (a Pi 5's GIC-400, and anything that is not a
  GICv3) Firecracker does not save the guest's per-vCPU interrupt-controller
  state, so a restored guest comes back with its virtual timer disabled and never
  runs anything that waits on time again — it looks exactly like a guest that
  cannot reconnect over vsock. Only the guest can write those registers, so the
  agent carries them across the snapshot itself: the host arms it before pausing,
  each vCPU reapplies its own state in the first instruction it runs on resume,
  and the host stands the carry down when the guest answers — checking, as it does,
  that *every* vCPU was repaired and not just the one that answered the health
  probe. Guests that need none of this say so and nothing happens. The mechanism,
  the measurements and why there is no host-side lever are in
  [internal/agent/gic_linux.go](internal/agent/gic_linux.go).

  Snapshots power two things: the warm pool (above) and **resume-after-stop** —
  `POST /sandboxes/{id}/suspend` snapshots a *used* sandbox and tears its VM down,
  and `POST /sandboxes/{id}/resume` boots a fresh VM from that snapshot under the
  same id. A suspended sandbox costs no CPU or memory, only the snapshot, and keeps
  its slot and name so a resume is guaranteed both. A networked sandbox can be
  resumed: the restore takes a fresh netpool slot, remaps its interface onto the
  new TAP with Firecracker's `network_overrides`, and re-addresses the guest over
  vsock once it answers, so it comes back on its own address rather than the
  template's.

  **What snapshots still are not:**

  - No surviving a daemon restart. A snapshot's only handle is an in-memory ref, so
    both the pool's templates and a suspended sandbox's snapshot are captured per
    run, discarded at shutdown, and swept at startup — `-snapshot-dir` is scratch
    space, not storage, and it wants one directory per daemon. Suspend/resume works
    within a daemon's life, not across a restart of it.
  - No fork or sessions. Restoring one snapshot many times, or an API identity that
    spans several VMs, is not built.
  - Requires guest images rebuilt from this repo (the agent's snapshot and network
    routes), and a restored guest keeps the template's clock, so its `/proc/uptime`
    is the snapshot's rather than wall-clock.

  Networked resume runs only against a real Firecracker on a KVM host; the
  host-side suspend/resume lifecycle is covered by unit tests, but the
  networked-restore path itself has not been exercised on KVM in this change.

One gotcha worth recording: a Dockerfile's `ENV` is container-runtime metadata,
not a file. `docker export` discards it and the guest kernel hands PID 1 an
empty environment — so the build materialises the image's environment into
`/etc/microvm/environment` and init loads it. Without that, `rustc` and `go` are
simply not on the PATH.

Images can be built with **dm-verity** (`MICROVM_VERITY=1 images/build.sh …`):
the build emits a hash tree and root hash beside the `.ext4`, and a daemon that
finds them boots the image as a verified device — the kernel checks every block
against the hash tree and panics before init if the shared image was tampered
with. Opt-in per image; needs a guest kernel with `CONFIG_DM_VERITY` /
`CONFIG_DM_INIT`. See [DEPLOY.md](DEPLOY.md).

## API

Two ways to run code, and the difference matters:

- **Sandbox** — you hold a VM and run commands in it. Creation fails when the
  node is full, so backpressure is yours.
- **Task** — you hand work to the queue. It never fails for capacity; it waits
  for a slot anywhere in the fleet.

Use a sandbox for several commands sharing state, a task for throughput.

```
POST   /v1/sandboxes                                   create (optional name + get_or_create)
GET    /v1/sandboxes                                   list (cursor paginated), ?tag=k:v narrows it
GET    /v1/sandboxes/{sb}                              state + live stats
DELETE /v1/sandboxes/{sb}                              destroy, returns the final cost
POST   /v1/sandboxes/{sb}/extend                       buy more time, bounded from creation
POST   /v1/sandboxes/{sb}/suspend                      snapshot to disk, tear the VM down
POST   /v1/sandboxes/{sb}/resume                       boot a fresh VM from the snapshot

POST   /v1/sandboxes/{sb}/executions                   start a command, returns at once (tty optional)
GET    /v1/sandboxes/{sb}/executions                   list
GET    /v1/sandboxes/{sb}/executions/{exe}             output, even after the VM is gone
GET    /v1/sandboxes/{sb}/executions/{exe}/stream      SSE: replays, then follows
POST   /v1/sandboxes/{sb}/executions/{exe}/cancel      signal the process group
POST   /v1/sandboxes/{sb}/executions/{exe}/resize      resize a tty execution's window

POST   /v1/sandboxes/{sb}/files                        upload
POST   /v1/sandboxes/{sb}/files/batch                  upload up to 100, in order
POST   /v1/sandboxes/{sb}/dirs                         mkdir -p, for the empty one
GET    /v1/sandboxes/{sb}/files?path=...               download

POST   /v1/tasks                                       queue work
GET    /v1/tasks/{tsk}                                 status + result
GET    /v1/queue                                       depth + this node's slots
GET    /v1/images                                      what this node can run
GET    /v1/health                                      liveness (no token)
```

**Starting a command and watching it are two calls**, and the split earns its
keep twice. The execution belongs to its sandbox rather than to an HTTP request,
so a dropped connection no longer kills a running job — it used to, because the
request's context was the exec's context. And because output is buffered on the
host, the stream *replays from the beginning* before it follows: connecting late
or reconnecting after a blip loses nothing. A single create-and-stream call
cannot offer either.

Errors are one shape, always — including for routes that do not exist:

```json
{"error":{"type":"capacity_error","code":"node_at_capacity",
          "message":"This node has no free capacity. Retry shortly, or submit a task instead...",
          "request_id":"req_01JZ8QK3M4N5P6R7S8T9V0W1X2"}}
```

`type` is what to branch on — it says what to *do*. `capacity_error` is worth
retrying; `invalid_request_error` never is. No amount of parsing the message
tells you which you have.

**`Idempotency-Key` on every create.** A request whose reply is lost cannot be
known to have happened, so the caller's only options are to retry (and maybe run
the work twice) or not to (and maybe never run it). A key gives them a third:
retry and get the original answer. Reusing a key with a different body is an
`idempotency_error` rather than a silent replay of the wrong reply.

Four routes take no key and are still safe to retry, because repeating them is a
no-op by construction rather than by bookkeeping: `extend` never brings a deadline
forward, a file write is an overwrite, and `dirs` is `mkdir -p`. All three SDKs
retry them for that reason — otherwise they would be the routes where a rate
limit's 429 reaches your code as a hard failure, which is to say the routes you
upload a project with.

### Buying more time

`POST /v1/sandboxes/{sb}/extend` pushes the TTL out, for work that turned out
longer than you guessed at create time. The alternative is creating a second
sandbox, which does not have the first one's state.

The new deadline is bounded by the host's maximum lifetime measured **from
creation**, never from now. That is the whole design: extension buys time and can
never buy immortality, so a caller heartbeating in a forgotten loop still ends up
with a sandbox that dies rather than a slot pinned for a week. Measured from now
the bound would move with every call and the lifetime hostile code is held to
would stop existing.

Asking past the bound is a `400` naming the seconds that are left, not a silent
trim to the maximum: a caller told `200` for an hour they did not get plans for an
hour and finds out when their work is killed halfway. Read `expires` off the reply
either way — the request says how long you want, the reply says what you have.

It deliberately does **not** touch the idle timeout. A long TTL is not a reason to
keep an idle VM's memory reserved; nothing except running something is.

### Tags

`tags` on create, and a repeatable `?tag=key:value` on the list, ANDing so a
second tag narrows the page rather than widening it. At most 10 pairs, keys 64
bytes and values 256 — they are held for the sandbox's whole life, so the caps are
the reason a create call is not a way to park megabytes in the daemon's heap.

Two things they deliberately are not. They are **node-local**, like the list they
filter: this searches the sandboxes one node holds, not the fleet's, so finding a
tagged sandbox across a fleet means asking each node. And nothing about them
reaches the guest — that is what separates a tag from `env`. A tag names a sandbox;
it does not configure one. Which is also why they are returned and `env` is not: a
tag is a label, never a secret.

### Uploading a project

`POST /v1/sandboxes/{sb}/files/batch` writes up to 100 files in one request, and
`POST /v1/sandboxes/{sb}/dirs` creates a directory and its parents.

Validation is all-or-nothing and writing is not, because writing cannot be: there
is no unwriting the third file. So every entry is checked before the first byte
reaches the guest — a batch with one bad mode writes nothing — the files go in the
order given, and a batch that fails partway names the entry it stopped at. That
ordering is what lets you say which files landed. Each path may be named once,
since two entries for one path would report two sizes for one file.

`dirs` exists for the directory that stays empty: an upload already creates its
parents, so the only ones you have to ask for are the ones nothing is written into
— somewhere a command puts its output, or a layout a build tool expects before it
starts.

A transfer counts as activity, so staging a project for minutes without running
anything does not look idle. It used to: only executions touched the idle clock,
and the reclaim took the VM away mid-batch.

### Seeding from a repo or a tarball

`source` on create seeds the sandbox before the call returns, so the first
execution already has the project:

```json
{"image": "python",
 "source": {"type": "tarball",
            "url": "https://github.com/acme/widgets/archive/refs/tags/v1.2.3.tar.gz",
            "strip_components": 1}}
```

`{"type":"git","url":"...","ref":"main","credential_ref":"github-ci"}` clones
instead. Without it, getting code in is one request per file, or a clone inside the
guest — which needs `network: true`, is impossible for a network-isolated sandbox,
and puts your credential inside a VM that is untrusted by design.

**The fetch happens on the host.** The daemon downloads the source, expands it, and
writes the tree in over the same file endpoints an upload uses. Nothing in the guest
reaches the network to do it, which is why it works with `network: false` and why a
private repository's token never enters the VM: the clone is host-side, only the
working tree is copied in, and `.git` is not written at all. `credential_ref` names a
credential the operator configured — the name travels, the secret does not.

**A credential is bound to a URL prefix, by the operator, and a `credential_ref`
outside it is refused.** Otherwise it would be a confused deputy: the map is shared
by every caller, so naming a credential on some other repository would spend the
operator's token on code they never handed over — and naming it on a host the caller
influences would hand over the token itself, since git offers its credential to
anything that challenges for one.

**It is off until an operator allowlists hosts**, with `-source-fetch` *and* at
least one `-source-allow-host`; either one missing refuses every source. That gate
is not paperwork, it is the feature. "Fetch this URL for me" hands a caller an
outbound request made by the daemon, and the daemon runs *outside* the firewall it
installs for its guests — nftables blocks RFC1918 and `169.254.169.254` for a
sandbox and protects nothing here. Since the caller then reads the result out of
their own sandbox, an unfiltered fetcher would be a full SSRF read primitive: the
host's LAN, the daemon's own API on loopback, and the cloud metadata service that
holds the host's identity. So it is https only, no proxy, every redirect hop
re-checked against the allowlist, and where a name resolves vetted again on the
syscall path — a name that rebinds to the metadata address between the check and
the connect gets no socket.

**Seeding is all-or-nothing.** A failure at any stage destroys the VM and fails the
create, so no half-seeded sandbox is returned, listed or billed. Anything refused
before a byte leaves the host is one `source_not_permitted` (400) with one message —
a caller who could tell "no such host" from "that is a private address" would have a
port scanner with the host's routing table — and an origin that was reached and
misbehaved is `source_fetch_failed` (502), which is worth retrying with the same
`Idempotency-Key`. A tree too big for the sandbox is refused before the VM boots
naming `mem_mib` and `disk_mib`, rather than becoming an `ENOSPC` from somewhere
inside the guest.

**Seeding does not spend the TTL.** `ttl_seconds` is time to run things in, and the
clock is restarted once the tree is in the guest, so a slow fetch cannot hand back a
sandbox whose `expires_at` elapsed during the create. A `ttl_seconds` shorter than
the seed itself is refused naming that field.

DEPLOY.md §10 is the operator side.

### CLI

```
microvm run python main.py            # upload, run, print output
microvm run node app.ts -network      # with filtered internet
microvm run python job.py -env KEY=v -timeout 30s
microvm exec go go test ./... -source git=https://github.com/acme/widgets
microvm exec node npm test -source tarball=https://host/v1.2.3.tar.gz -source-strip 1
microvm submit python job.py          # queue it instead; prints a task ID
microvm result tsk_01JZ8...           # wait for it and print the output
microvm queue                         # depth and this node's slots
microvm ps
microvm ps -tag env=ci                # only sandboxes carrying that tag
microvm logs sb_01JZ8... exe_01JZ8... # an execution's recorded output
microvm bench python main.py -n 10    # time each leg of a run separately
```

`bench` is what produced the numbers under [Images](#images). It runs the same
public API the other commands do and times every leg a caller can see — create,
upload, exec, first byte, the program, teardown — so a total is never quoted
without the breakdown that explains it. `-warmup` (default 1) discards the first
iterations, which is what makes "hot in the page cache" true rather than assumed;
`-warmup=0` measures the cold read instead. `-json` gives the raw per-run numbers.
For the inside of the boot, read the daemon's own `sandbox booted` log line for
the same runs.

The exit code is the program's own, so it composes:

```
microvm run python test.py && deploy
```

Ctrl-C aborts the process *inside the guest*, not just the CLI.

`-source` spells the type out rather than guessing it from the URL: the same path
can name a repository and a tarball, and a CLI that inferred the wrong one would
seed the wrong thing and report success. `-source-ref`, `-source-strip` and
`-source-credential` modify it.

### Go

```go
client := microvm.New("http://127.0.0.1:8080", microvm.WithToken(token))

sb, err := client.Sandboxes.Create(ctx, microvm.SandboxCreateParams{Image: "python"})
if err != nil {
    if microvm.IsCapacity(err) { /* full: retry, or submit a task */ }
    return err
}
defer client.Sandboxes.Delete(ctx, sb.Id)

client.Files.Write(ctx, sb.Id, "main.py", []byte(`print("hello")`))

exe, _ := client.Run(ctx, sb.Id, "python3", "main.py")
fmt.Print(exe.Stdout)
```

Streaming, and paging, are iterators:

```go
for frame, err := range client.Executions.Stream(ctx, sb.Id, exe.Id) {
    if err != nil { return err }
    os.Stdout.Write(frame.Bytes())
}

for sb, err := range client.Sandboxes.All(ctx, microvm.SandboxListParams{}) {
    if err != nil { return err }
    fmt.Println(sb.Id, sb.Stats.ActiveCpuMs)
}
```

### TypeScript

On npm as **[`@pablofdezr/microvm`](https://www.npmjs.com/package/@pablofdezr/microvm)** — ESM, Node ≥ 18, zero runtime dependencies.

```
npm install @pablofdezr/microvm
```

```ts
import { Client } from "@pablofdezr/microvm";

const client = new Client("http://127.0.0.1:8080", { token });

const sb = await client.sandboxes.create({ image: "python" });
try {
  await client.files.write(sb.id, "main.py", 'print("hello")');
  const exe = await client.run(sb.id, "python3", ["main.py"]);
  console.log(exe.stdout);
} finally {
  await client.sandboxes.delete(sb.id);
}

for await (const frame of client.executions.stream(sb.id, exe.id)) {
  if (frame.type === "stdout") process.stdout.write(frameText(frame));
}
```

Both SDKs give you `err(execution)` / `exe.Err()`, which returns nothing for a
non-zero exit and an error for the endings that are **not** your code's doing —
a timeout, a cancel, a VM taken away. That distinction is the one worth having:
a `vanished` execution means we took your sandbox, not that your program failed.

All three build a `source` for you, so a seeded create is one call:

```go
client.Sandboxes.Create(ctx, microvm.SandboxCreateParams{Image: "go",
    Source: microvm.GitSource("https://github.com/acme/widgets", "main")})
```
```ts
await client.sandboxes.create({ image: "go", source: gitSource(url, "main") });
```
```python
client.sandboxes.create("go", source=git_source(url, "main"))
```

`TarballSource` / `tarballSource` / `tarball_source` is the other one, taking the
`strip_components` a release archive needs. Seeding has two failures worth branching
on and each SDK names both: `IsSourceNotPermitted` / `isSourceNotPermitted` /
`is_source_not_permitted` for a source the operator has not allowed, which no retry
will fix, and `IsSourceFetchFailed` and friends for an origin that misbehaved, which
one might.

## Running it

`microvmd` needs root: it manages TAP devices, nftables and cgroups. The VMM it
launches does not — the jailer drops it to the uid you pass.

```
MICROVM_TOKENS="$TOKEN" microvmd \
  -addr 127.0.0.1:8080 \
  -image-dir /var/lib/microvm/images \
  -kernel /var/lib/microvm/vmlinux \
  -uid 1000 -gid 1000 \
  -slots 10 \
  -redis redis:6379 \
  -ceiling-cores 8 -ceiling-mem-mb 16384 \
  -tenant-max-sandboxes 20 -tenant-max-rps 20
```

The queue and the slots are separate decisions, which is what lets a fleet be
shaped rather than cloned. `-redis` with `-slots 0` is an API front end that
takes work and runs none; slots without an exposed address is a pure worker;
both is the single-box case. No node needs to know the others exist.

Tokens come from `MICROVM_TOKENS`, from `-tokens-file` (one per line, `#`
comments allowed), or from `-tokens`. The sources add up, so moving off the flag
is a rotation and not a cutover. Prefer either of the first two: a secret on a
command line is a secret in `ps`, in shell history, and in whatever unit file
started the daemon. `-tokens` and `-admin-tokens` still work and are deprecated.

`-tenant-max-sandboxes` and `-tenant-max-rps` are per-token ceilings, unlike
`-slots` and `-ceiling-*` which bound the host. Without them any one valid token
can hold every slot on the node and call as fast as VMs boot. Both default to
unlimited; over either, a caller gets the usual `capacity_error` 429 — the same
type a full node answers with, deliberately, because the caller's move is the same
and that is the pair every SDK already backs off on.

Only the rate limit adds `Retry-After`, and that asymmetry is the point: a token
bucket knows to the second when it will have another request, whereas when one of
your sandboxes ends is up to you, and a number invented here would be a guess
dressed as a fact.

Both are charged to a *tenant* rather than to an address, because an address is not
an identity: one tenant behind a NAT would be charged for its neighbours, and one
with a pool of egress addresses for almost none of its own. This daemon derives a
tenant per token, so here a ceiling is per key — **two keys are two allowances**, and
a caller who wants one allowance across several keys needs them to share a tenant,
which is a `Principals` configuration these flags do not expose yet.

`-tenant-max-sandboxes` bounds sandboxes, not tasks, and that is a limitation
rather than a decision worth defending: the counter is one node's, and a task is
scheduled across the fleet, so a task enqueued here may run on a node where the
counter says nothing. Bound task-driven load with `-slots`, `-cpu` and `-mem`,
which bound the box whoever the work belongs to.

A sandbox belongs to the tenant that created it. Every route naming one resolves
it against the caller, so another tenant's sandbox is a `404` — not a `403`, which
would confirm which of a guessed range exist — and the list is scoped the same way.
An admin key keeps the node-wide view, since it is the operator's own.

`-sandbox-retention` is how long a stopped sandbox stays listed and retrievable
before the daemon forgets it. It defaults to forever, which is a slow leak on a
node that never restarts. Set it lower than `-log-retention` and it is raised to
it: the stopped record carries the final metering, and every exec record is
reached through its sandbox. Past the window the ID answers the ordinary
`sandbox_not_found`.

`-addr` defaults to loopback on purpose: this API creates VMs that run arbitrary
code, so an open one is an open shell. Put a TLS terminator in front of it.

Set `-ceiling-cores` and `-ceiling-mem-mb` on any host running anything else.
Without them, sandboxes can take the whole box.

`-chroot-base` must share a filesystem with `-image-dir`, or every sandbox
copies its image instead of hardlinking it.

## Building images

```
images/build.sh python arm64      # or amd64
```

Docker assembles the userland; the result is exported flat and packed into ext4
inside a container, so the build needs no root. Build on a machine that is not
serving traffic — compiling the Rust image will saturate every core it can find.

## Testing

```
go test ./...                     # no KVM needed; Redis tests skip without redis-server

# e2e — needs root, KVM, and Firecracker on PATH
sudo env MICROVM_TEST_KERNEL=/path/to/vmlinux \
         MICROVM_TEST_ROOTFS=/path/to/base-arm64.ext4 \
         ./e2e.test -test.v
```

The e2e suite is where the security claims are actually checked. A unit test can
assert a firewall rule was rendered; only a booted guest can prove the packet
does not get out.

Everything else runs against `internal/runtime/runtimetest`, a real
implementation of the runtime port with a pretend VM behind it. That is what
lets the sandbox manager and the whole API be tested on a laptop.

Two habits are worth keeping, because both caught real bugs here:

**Mutate the code and check the test fails.** A test that has never failed is a
test with no evidence behind it. Breaking this queue's ordering on purpose
revealed that the FIFO test passed anyway — it used task IDs `a`, `b`, `c`, so
it was asserting that the alphabet is sorted, not that the queue is. It now uses
IDs that contradict alphabetical order, and enough of them to cross a digit
boundary, which is where an unpadded sequence puts task 10 ahead of task 9.

**Write the test that needs a second implementation.** The conformance suite
exists because Redis had to prove it matches the in-memory queue — and in
writing it, the in-memory queue turned out to accept a duplicate task ID while
one was pending, run the work twice, and silently discard the first worker's
result.

## License

[Apache License 2.0](LICENSE). See [SECURITY.md](SECURITY.md) for how to report
vulnerabilities and [CONTRIBUTING.md](CONTRIBUTING.md) to get started.
