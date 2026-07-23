# microvm Python SDK

Client for the microvm daemon: run untrusted code in Firecracker microVMs.
**Standard library only** — no third-party dependencies. Python ≥ 3.9.

```
pip install microvm
```

API objects come back as plain dicts (their shapes are the OpenAPI schemas), so
you read them with `sb["id"]`, `exe["stdout"]`, and so on.

## Quick start

```python
from microvm import Client

client = Client("http://127.0.0.1:8080", token)

sb = client.sandboxes.create("python")
try:
    exe = client.run(sb["id"], "python3", "-c", "print('hi')")
    print(exe["stdout"], end="")
finally:
    client.sandboxes.delete(sb["id"])
```

## Sandboxes vs tasks

- **Sandbox** — you hold a VM and run commands in it. `create` raises `APIError`
  with `.is_capacity` when the node is full, so backpressure is yours.
- **Task** — you hand work to the fleet; it never fails for capacity and waits
  for a slot on any node, sized to the cpu/mem you request.

```python
task = client.tasks.create(
    "python", "python3",
    args=["-c", "print(2 + 2)"],
    vcpus=2, mem_mib=1024,
    priority=7,  # 0-10, higher first
)
done = client.tasks.wait(task["id"])
print(done["stdout"], end="")
```

Extra fields are just keyword arguments — they go straight into the request body.

A task has no live sandbox to upload to first, so its files travel inside
`create`, keyed by path and written before `cmd` runs. Give the content as text
or bytes; it is base64-encoded for you, exactly as `files.write` does:

```python
client.tasks.create(
    "python", "python3",
    args=["/app/main.py"],
    files={"/app/main.py": 'print("hi")'},
)
```

## Streaming output

```python
for frame in client.executions.stream(sb["id"], exe["id"]):
    if frame.get("data"):
        sys.stdout.buffer.write(frame["data"])  # data frames are decoded to bytes
```

## Pagination

`all()` is a generator that follows `has_more` to the end:

```python
for sb in client.sandboxes.all():
    print(sb["id"], sb["state"])
```

`client.executions.all(sandbox_id)` works the same way.

## Retries

Transient failures — a network error, or a 429/500/502/503/504 — are retried
with exponential backoff, full jitter, and any `Retry-After` honoured. Only
idempotent requests are retried: GET/PUT/DELETE always, and POST only when it
carries an idempotency key.

```python
client = Client(base_url, token, max_retries=4)  # default 2, 0 disables

client.tasks.create("python", "python3", idempotency_key=str(uuid4()))
```

## Errors

Failures are `APIError` with the API's `type`, `code`, `message`, `param` and
`request_id`. Branch on the guards:

```python
from microvm import APIError

try:
    client.sandboxes.retrieve(sandbox_id)
except APIError as e:
    if e.is_not_found: ...   # 404
    if e.is_capacity: ...    # node full — consider a task
    if e.is_conflict: ...    # e.g. executing in a stopped sandbox
    if e.is_forbidden: ...   # key lacks permission (admin-only route)
```

## Observability

`on_response` is called once per HTTP attempt — retries included:

```python
Client(base_url, token,
       on_response=lambda i: print(f"{i.method} {i.path} attempt={i.attempt} status={i.status}"))
```

## Tenants (admin)

Setting a tenant's storage policy needs an admin token; an ordinary key gets a
403 (`APIError.is_forbidden`).

```python
admin.tenants.set_limit(tenant_id, 500 * 1024 * 1024, "evict")  # or "preserve"
t = admin.tenants.retrieve(tenant_id)  # policy + live usage
```

## Tests

```
python3 sdk/python/tests/test_client.py   # no pytest needed
```
