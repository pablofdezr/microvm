# microvm

TypeScript client for the microvm daemon: run untrusted code in Firecracker
microVMs. ESM, Node ≥ 18, zero runtime dependencies (it uses the built-in
`fetch`). Types are generated from the same OpenAPI spec as the server.

```
npm install microvm
```

## Quick start

```ts
import { Client } from "microvm";

const client = new Client("http://127.0.0.1:8080", { token });

const sb = await client.sandboxes.create({ image: "python" });
try {
  const exe = await client.run(sb.id, "python3", ["-c", "print('hi')"]);
  console.log(exe.stdout);
} finally {
  await client.sandboxes.delete(sb.id);
}
```

## Sandboxes vs tasks

- **Sandbox** — you hold a VM and run commands in it. `create` throws a capacity
  error when the node is full (`err.isCapacity`), so backpressure is yours.
- **Task** — you hand work to the fleet; it never fails for capacity and waits
  for a slot on any node, sized to the CPU and memory you request.

```ts
const task = await client.tasks.create({
  image: "python",
  cmd: "python3",
  args: ["-c", "print(2 + 2)"],
  vcpus: 2,
  mem_mib: 1024,
  priority: 7, // 0-10, higher first
});
const done = await client.tasks.wait(task.id);
console.log(done.stdout);
```

Optional fields are plain optional properties — no pointer wrappers.

A task has no live sandbox to upload to first, so its files travel inside
`create`, keyed by path and written before `cmd` runs. Pass the content as text
or bytes — it is base64-encoded for you, exactly as `files.write` does:

```ts
await client.tasks.create({
  image: "python",
  cmd: "python3",
  args: ["/app/main.py"],
  files: { "/app/main.py": 'print("hi")' },
});
```

## Streaming output

```ts
for await (const frame of client.executions.stream(sb.id, exe.id)) {
  process.stdout.write(frame.data);
}
```

## Pagination

`all` is an async iterator that follows `has_more` to the end:

```ts
for await (const sb of client.sandboxes.all({})) {
  console.log(sb.id, sb.state);
}
```

`client.executions.all(sandboxId, {})` works the same way.

## Retries

Transient failures — a network error, or a 429/500/502/503/504 — are retried
with exponential backoff, full jitter, and any `Retry-After` honoured. Only
idempotent requests are retried: GET/PUT/DELETE always, and POST only when it
carries an idempotency key.

```ts
const client = new Client(baseURL, { token, maxRetries: 4 }); // default 2, 0 disables

await client.tasks.create(params, { idempotencyKey: crypto.randomUUID() });
```

## Errors

Failures are `APIError` with the API's `type`, `code`, `message`, `param` and
`requestId`. Branch on the guards:

```ts
try {
  await client.sandboxes.retrieve(id);
} catch (e) {
  if (e instanceof APIError) {
    if (e.isNotFound) { /* 404 */ }
    if (e.isCapacity) { /* node full — consider a task */ }
    if (e.isConflict) { /* e.g. executing in a stopped sandbox */ }
    if (e.isForbidden) { /* key lacks permission (admin-only route) */ }
  }
}
```

## Observability

`onResponse` is called once per HTTP attempt — retries included:

```ts
new Client(baseURL, {
  token,
  onResponse: (info) =>
    console.log(`${info.method} ${info.path} attempt=${info.attempt} status=${info.status} ${info.durationMs}ms`),
});
```

## Tenants (admin)

Setting a tenant's storage policy needs an admin token; an ordinary key gets a
403 (`err.isForbidden`).

```ts
await admin.tenants.setLimit(tenantId, 500 * 1024 * 1024, "evict"); // or "preserve"
const t = await admin.tenants.retrieve(tenantId); // policy + live usage
```
