# microvm Go SDK

Client for the microvm daemon: run untrusted code in Firecracker microVMs.

```
go get github.com/pablofdezr/microvm-sdk-go@latest
```

It is its own module, so importing it does not drag in the daemon's host-side
dependencies (netlink, vsock, and the rest). The types in `types.gen.go` are
generated from the same OpenAPI spec the server is; everything else — transport,
typed errors, retries, streaming, pagination — is hand-written.

## Quick start

```go
client := microvm.New("http://127.0.0.1:8080", microvm.WithToken(token))
ctx := context.Background()

sb, err := client.Sandboxes.Create(ctx, microvm.SandboxCreateParams{Image: "python"})
if err != nil {
    log.Fatal(err)
}
defer client.Sandboxes.Delete(ctx, sb.Id)

exe, err := client.Run(ctx, sb.Id, "python3", "-c", "print('hi')")
if err != nil {
    log.Fatal(err)
}
fmt.Print(exe.Stdout)
```

## Sandboxes vs tasks

- **Sandbox** — you hold a VM and run commands in it. `Create` fails with a
  capacity error when the node is full (`microvm.IsCapacity(err)`), so
  backpressure is yours.
- **Task** — you hand work to the fleet. It never fails for capacity; it waits
  for a slot on any node, sized to the CPU and memory you request.

```go
task, _ := client.Tasks.Create(ctx, microvm.TaskCreateParams{
    Image:    "python",
    Cmd:      "python3",
    Args:     &[]string{"-c", "print(2+2)"},
    Vcpus:    microvm.Ptr(2),
    MemMib:   microvm.Ptr(1024),
    Priority: microvm.Ptr(7), // 0-10, higher first
})
done, _ := client.Tasks.Wait(ctx, task.Id)
fmt.Print(done.Stdout)
```

Optional fields are pointers so an absent field is distinct from a zero one;
`microvm.Ptr(v)` sets them without a throwaway variable each.

A task has no live sandbox to upload to first, so its files travel inside
`Create`, keyed by path and written before `Cmd` runs. `Files` is a
`map[string][]byte`, so the bytes are base64-encoded for the wire by `encoding/json`
itself — you pass the raw content, just as `Files.Write` takes raw bytes:

```go
client.Tasks.Create(ctx, microvm.TaskCreateParams{
    Image: "python",
    Cmd:   "python3",
    Args:  &[]string{"/app/main.py"},
    Files: &map[string][]byte{"/app/main.py": []byte(`print("hi")`)},
})
```

## Streaming output

```go
for frame, err := range client.Executions.Stream(ctx, sb.Id, exe.Id) {
    if err != nil {
        log.Fatal(err)
    }
    os.Stdout.Write(frame.Data)
}
```

## Pagination

`All` follows `has_more` to the end, so you never thread a cursor by hand:

```go
for sb, err := range client.Sandboxes.All(ctx, microvm.SandboxListParams{}) {
    if err != nil {
        log.Fatal(err)
    }
    fmt.Println(sb.Id, sb.State)
}
```

`Executions.All(ctx, sandboxID, params)` works the same way.

## Retries

Transient failures — a network error, or a 429/500/502/503/504 — are retried
with exponential backoff, full jitter, and any `Retry-After` honoured. Only
idempotent requests are retried: GET, PUT and DELETE always, and POST only when
it carries an idempotency key (retrying a keyless create could run it twice).

```go
client := microvm.New(addr,
    microvm.WithToken(token),
    microvm.WithMaxRetries(4), // default is 2; 0 disables
)

// A create that must be safe to retry:
client.Tasks.Create(ctx, params, microvm.WithIdempotencyKey(uuid))
```

## Errors

`*microvm.APIError` carries the API's own `Type`, `Code`, `Message`, `Param` and
`RequestID`. Branch on the guards rather than status codes:

```go
switch {
case microvm.IsNotFound(err):  // 404
case microvm.IsCapacity(err):  // node full — consider a task
case microvm.IsConflict(err):  // e.g. executing in a stopped sandbox
case microvm.IsForbidden(err): // key lacks permission (admin-only route)
}
```

## Observability

`WithObserver` is called once per HTTP attempt — retries included — for logging,
metrics or tracing:

```go
microvm.WithObserver(func(info microvm.RequestInfo) {
    log.Printf("%s %s attempt=%d status=%d in %s",
        info.Method, info.Path, info.Attempt, info.Status, info.Duration)
})
```

## Tenants (admin)

Setting a tenant's storage policy needs an admin token; an ordinary key gets a
403 (`microvm.IsForbidden`).

```go
admin.Tenants.SetLimit(ctx, tenantID, 500<<20, microvm.Evict) // or microvm.Preserve
t, _ := admin.Tenants.Retrieve(ctx, tenantID)                 // policy + live usage
```

See the runnable examples in `example_test.go` and the full reference on
[pkg.go.dev](https://pkg.go.dev/github.com/pablofdezr/microvm-sdk-go/microvm).
