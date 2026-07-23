# microvm

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

Slim where a slim variant exists. Measured, running real code on a Pi 5:

| Image | Size | Cold run |
|---|---|---|
| python | 127 MB | 164 ms |
| node (TypeScript via tsx) | 278 MB | 3.2 s |
| rust | 801 MB | 1.4 s |
| go | 914 MB | 27 s (compiles cold, no cache) |

Go and Rust are large because running their code means compiling it. Their cold
times are the case a warm pool of snapshots exists to fix.

One gotcha worth recording: a Dockerfile's `ENV` is container-runtime metadata,
not a file. `docker export` discards it and the guest kernel hands PID 1 an
empty environment — so the build materialises the image's environment into
`/etc/microvm/environment` and init loads it. Without that, `rustc` and `go` are
simply not on the PATH.

## API

Two ways to run code, and the difference matters:

- **Sandbox** — you hold a VM and run commands in it. Creation fails when the
  node is full, so backpressure is yours.
- **Task** — you hand work to the queue. It never fails for capacity; it waits
  for a slot anywhere in the fleet.

Use a sandbox for several commands sharing state, a task for throughput.

```
POST   /v1/sandboxes                                   create
GET    /v1/sandboxes                                   list (cursor paginated)
GET    /v1/sandboxes/{sb}                              state + live stats
DELETE /v1/sandboxes/{sb}                              destroy, returns the final cost

POST   /v1/sandboxes/{sb}/executions                   start a command, returns at once
GET    /v1/sandboxes/{sb}/executions                   list
GET    /v1/sandboxes/{sb}/executions/{exe}             output, even after the VM is gone
GET    /v1/sandboxes/{sb}/executions/{exe}/stream      SSE: replays, then follows
POST   /v1/sandboxes/{sb}/executions/{exe}/cancel      signal the process group

POST   /v1/sandboxes/{sb}/files                        upload
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

### CLI

```
microvm run python main.py            # upload, run, print output
microvm run node app.ts -network      # with filtered internet
microvm run python job.py -env KEY=v -timeout 30s
microvm submit python job.py          # queue it instead; prints a task ID
microvm result tsk_01JZ8...           # wait for it and print the output
microvm queue                         # depth and this node's slots
microvm ps
microvm logs sb_01JZ8... exe_01JZ8... # an execution's recorded output
```

The exit code is the program's own, so it composes:

```
microvm run python test.py && deploy
```

Ctrl-C aborts the process *inside the guest*, not just the CLI.

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

```ts
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

## Running it

`microvmd` needs root: it manages TAP devices, nftables and cgroups. The VMM it
launches does not — the jailer drops it to the uid you pass.

```
microvmd \
  -addr 127.0.0.1:8080 \
  -image-dir /var/lib/microvm/images \
  -kernel /var/lib/microvm/vmlinux \
  -uid 1000 -gid 1000 \
  -slots 10 \
  -redis redis:6379 \
  -ceiling-cores 8 -ceiling-mem-mb 16384 \
  -tokens "$TOKEN"
```

The queue and the slots are separate decisions, which is what lets a fleet be
shaped rather than cloned. `-redis` with `-slots 0` is an API front end that
takes work and runs none; slots without an exposed address is a pure worker;
both is the single-box case. No node needs to know the others exist.

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
