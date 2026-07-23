/**
 * The TypeScript client for the microvm API.
 *
 * The resource types in `types.gen.ts` are generated from `api/openapi.yaml`,
 * the same file the server and the Go SDK are generated from. What is written
 * here by hand is only what a generator does badly: the transport, typed
 * errors, streaming, auto-pagination, and the few helpers that turn three calls
 * into one.
 *
 * ```ts
 * const client = new Client("http://127.0.0.1:8080", { token });
 *
 * const sb = await client.sandboxes.create({ image: "python" });
 * try {
 *   await client.files.write(sb.id, "main.py", 'print("hello")');
 *   const exe = await client.run(sb.id, "python3", ["main.py"]);
 *   console.log(exe.stdout);
 * } finally {
 *   await client.sandboxes.delete(sb.id);
 * }
 * ```
 */

import type { components } from "./types.gen.js";

type Schemas = components["schemas"];

export type Sandbox = Schemas["Sandbox"];
export type SandboxList = Schemas["SandboxList"];
export type SandboxState = Schemas["SandboxState"];
export type SandboxCreateParams = Schemas["SandboxCreateParams"];
export type Execution = Schemas["Execution"];
export type ExecutionList = Schemas["ExecutionList"];
export type ExecutionStatus = Schemas["ExecutionStatus"];
export type ExecutionCreateParams = Schemas["ExecutionCreateParams"];
export type ExecutionCancelParams = Schemas["ExecutionCancelParams"];
export type File = Schemas["File"];
export type Task = Schemas["Task"];
export type TaskStatus = Schemas["TaskStatus"];
export type TaskCreateParams = Omit<Schemas["TaskCreateParams"], "files"> & {
  /**
   * Files written into the sandbox before `cmd` runs, keyed by path. Pass the
   * content as text or raw bytes; it is base64-encoded for you, exactly as
   * `files.write` does — the wire form is base64, but that is not the caller's
   * job to produce here any more than it is there.
   */
  files?: Record<string, string | Uint8Array>;
};
export type Queue = Schemas["Queue"];
export type Image = Schemas["Image"];
export type ImageList = Schemas["ImageList"];
export type Tenant = Schemas["Tenant"];
export type TenantList = Schemas["TenantList"];
export type TenantUpdateParams = Schemas["TenantUpdateParams"];
export type TenantFullPolicy = Schemas["TenantFullPolicy"];
export type Health = Schemas["Health"];
export type Frame = Schemas["Frame"];
export type ErrorType = Schemas["ErrorType"];

/** How long an ordinary request may take. Streams and waits opt out. */
export const DEFAULT_TIMEOUT_MS = 30_000;

/** Where a daemon listens unless told otherwise. */
export const DEFAULT_BASE_URL = "http://127.0.0.1:8080";

/** This SDK's version, sent in the User-Agent. */
export const SDK_VERSION = "0.1.0";

/** How many times a transient failure is retried unless told otherwise. */
export const DEFAULT_MAX_RETRIES = 2;

/** What an onResponse observer is told about one HTTP attempt. */
export interface RequestInfo {
  method: string;
  path: string;
  /** 1 for the first try, 2 for the first retry, ... */
  attempt: number;
  /** 0 when the request never got a response. */
  status: number;
  error?: unknown;
  durationMs: number;
}

export interface ClientOptions {
  token?: string;
  /** Overrides the global fetch, for tests or a custom agent. */
  fetch?: typeof globalThis.fetch;
  timeoutMs?: number;
  /**
   * How many times a transient failure -- a network error, or a
   * 429/500/502/503/504 -- is retried before it is thrown, with exponential
   * backoff and jitter and any Retry-After honoured. Only idempotent requests
   * are retried: GET/PUT/DELETE always, POST only with an idempotency key.
   * Defaults to DEFAULT_MAX_RETRIES; 0 disables.
   */
  maxRetries?: number;
  /** Called once per HTTP attempt -- retries included -- for logging or metrics. */
  onResponse?: (info: RequestInfo) => void;
}

/** Per-call options. */
export interface RequestOptions {
  signal?: AbortSignal;
  /**
   * Makes a create safe to retry.
   *
   * A request whose reply never arrived cannot be known to have failed, so a
   * bare retry may run the work twice. With a key, the retry returns the
   * original answer instead.
   */
  idempotencyKey?: string;
}

/**
 * An error the API reported.
 *
 * `type` is what to branch on: it says what class of thing went wrong and so
 * what to do about it. `code` says exactly which thing, for when that matters.
 */
export class APIError extends Error {
  constructor(
    readonly status: number,
    readonly type: ErrorType,
    readonly code: string,
    message: string,
    readonly param?: string,
    readonly requestId?: string,
  ) {
    super(message);
    this.name = "APIError";
  }

  /** The object does not exist. */
  get isNotFound(): boolean {
    return this.status === 404;
  }

  /**
   * The node has no room.
   *
   * The one error worth retrying unchanged — and the signal to consider a task
   * instead, since tasks wait for a slot anywhere in the fleet rather than
   * failing.
   */
  get isCapacity(): boolean {
    return this.type === "capacity_error";
  }

  /** The object is in a state that forbids the call. */
  get isConflict(): boolean {
    return this.status === 409;
  }

  /**
   * The key lacks permission — an ordinary token calling an admin-only endpoint,
   * such as setting a tenant's policy. Distinct from a missing token: the
   * request was authenticated, and refused.
   */
  get isForbidden(): boolean {
    return this.status === 403;
  }
}

export class Client {
  readonly sandboxes: SandboxResource;
  readonly executions: ExecutionResource;
  readonly files: FileResource;
  readonly tasks: TaskResource;
  readonly queue: QueueResource;
  readonly images: ImageResource;
  readonly tenants: TenantResource;

  readonly #baseURL: string;
  readonly #token?: string;
  readonly #fetch: typeof globalThis.fetch;
  readonly #timeoutMs: number;
  readonly #maxRetries: number;
  readonly #onResponse?: (info: RequestInfo) => void;

  constructor(baseURL: string = DEFAULT_BASE_URL, opts: ClientOptions = {}) {
    this.#baseURL = baseURL.replace(/\/+$/, "");
    this.#token = opts.token;
    // Bound, not merely captured: a detached `fetch` throws "Illegal
    // invocation" in a browser.
    this.#fetch = opts.fetch ?? globalThis.fetch.bind(globalThis);
    this.#timeoutMs = opts.timeoutMs ?? DEFAULT_TIMEOUT_MS;
    this.#maxRetries = opts.maxRetries ?? DEFAULT_MAX_RETRIES;
    this.#onResponse = opts.onResponse;

    this.sandboxes = new SandboxResource(this);
    this.executions = new ExecutionResource(this);
    this.files = new FileResource(this);
    this.tasks = new TaskResource(this);
    this.queue = new QueueResource(this);
    this.images = new ImageResource(this);
    this.tenants = new TenantResource(this);
  }

  /** @internal */
  async request(
    method: string,
    path: string,
    init: {
      body?: unknown;
      query?: Record<string, string | number | undefined>;
      opts?: RequestOptions;
      /** Streams manage their own lifetime, so no timeout is imposed. */
      noTimeout?: boolean;
    } = {},
  ): Promise<Response> {
    const url = new URL(this.#baseURL + "/v1" + path);
    for (const [k, v] of Object.entries(init.query ?? {})) {
      if (v !== undefined) url.searchParams.set(k, String(v));
    }

    const headers: Record<string, string> = {
      "User-Agent": `microvm-ts/${SDK_VERSION}`,
    };
    if (this.#token) headers["Authorization"] = `Bearer ${this.#token}`;
    if (init.body !== undefined) headers["Content-Type"] = "application/json";
    if (init.opts?.idempotencyKey) {
      headers["Idempotency-Key"] = init.opts.idempotencyKey;
    }
    const body =
      init.body === undefined ? undefined : JSON.stringify(init.body);

    // Retrying a request that is not idempotent could run the work twice.
    const idempotent =
      method === "GET" ||
      method === "HEAD" ||
      method === "PUT" ||
      method === "DELETE" ||
      !!init.opts?.idempotencyKey;

    let lastErr: unknown;
    for (let attempt = 1; attempt <= this.#maxRetries + 1; attempt++) {
      // The caller's signal and our timeout must both be able to abort, and the
      // caller's must survive: AbortSignal.any takes whichever fires first.
      const signals: AbortSignal[] = [];
      if (init.opts?.signal) signals.push(init.opts.signal);
      let timer: ReturnType<typeof setTimeout> | undefined;
      if (!init.noTimeout) {
        const ctrl = new AbortController();
        timer = setTimeout(() => ctrl.abort(), this.#timeoutMs);
        signals.push(ctrl.signal);
      }

      const start = Date.now();
      let resp: Response | undefined;
      let err: unknown;
      try {
        resp = await this.#fetch(url.toString(), {
          method,
          headers,
          body,
          signal: signals.length > 0 ? AbortSignal.any(signals) : undefined,
        });
      } catch (e) {
        err = e;
      } finally {
        if (timer !== undefined) clearTimeout(timer);
      }

      this.#onResponse?.({
        method,
        path,
        attempt,
        status: resp?.status ?? 0,
        error: err,
        durationMs: Date.now() - start,
      });

      const callerAborted = init.opts?.signal?.aborted ?? false;

      if (err !== undefined) {
        // A network error or a timeout. A caller-triggered abort is final; our
        // own timeout on an idempotent call is worth another try.
        lastErr = err;
        if (idempotent && attempt <= this.#maxRetries && !callerAborted) {
          await backoff(attempt, undefined, init.opts?.signal);
          continue;
        }
        throw err;
      }

      if (!resp!.ok) {
        if (
          retryableStatus(resp!.status) &&
          idempotent &&
          attempt <= this.#maxRetries
        ) {
          const retryAfter = resp!.headers.get("Retry-After") ?? undefined;
          await resp!.text().catch(() => undefined); // free the body
          await backoff(attempt, retryAfter, init.opts?.signal);
          continue;
        }
        throw await parseError(resp!);
      }
      return resp!;
    }
    throw lastErr;
  }

  /** @internal */
  async json<T>(
    method: string,
    path: string,
    init: Parameters<Client["request"]>[2] = {},
  ): Promise<T> {
    const resp = await this.request(method, path, init);
    return (await resp.json()) as T;
  }

  /** Whether the daemon is up. Needs no token. */
  health(opts?: RequestOptions): Promise<Health> {
    return this.json<Health>("GET", "/health", { opts });
  }

  /**
   * Start a command and wait for it to finish.
   *
   * Check `err(exe)` afterwards: a non-zero exit is the code's own verdict,
   * whereas a timeout or a vanished sandbox is not.
   */
  async run(
    sandboxID: string,
    cmd: string,
    args: string[] = [],
    opts?: RequestOptions,
  ): Promise<Execution> {
    const exe = await this.executions.create(sandboxID, { cmd, args }, opts);
    return this.executions.wait(sandboxID, exe.id, opts);
  }
}

/**
 * Turns an error reply into an APIError.
 *
 * It falls back rather than throwing. A proxy in front of the daemon can answer
 * with an HTML error page that is not our envelope at all, and a client that
 * breaks on that breaks exactly when things are already going wrong.
 */
async function parseError(resp: Response): Promise<APIError> {
  const requestId = resp.headers.get("X-Request-Id") ?? undefined;
  const fallback = () =>
    new APIError(
      resp.status,
      "api_error",
      "unknown",
      resp.statusText || `HTTP ${resp.status}`,
      undefined,
      requestId,
    );

  let body: unknown;
  try {
    body = await resp.json();
  } catch {
    return fallback();
  }

  const err = (body as { error?: Schemas["Error"] })?.error;
  if (!err?.code) return fallback();

  return new APIError(
    resp.status,
    err.type,
    err.code,
    err.message,
    err.param,
    err.request_id ?? requestId,
  );
}

export type ListParams = {
  limit?: number;
  starting_after?: string;
  ending_before?: string;
};

class SandboxResource {
  constructor(private readonly c: Client) {}

  /**
   * Boot a sandbox and wait for it to be ready.
   *
   * Throws a capacity error when the node is full — see `APIError.isCapacity`.
   * That is by design: a sandbox is a reservation, so you are told at once
   * rather than left waiting. Submit a task if you would rather wait.
   */
  create(params: SandboxCreateParams, opts?: RequestOptions): Promise<Sandbox> {
    return this.c.json<Sandbox>("POST", "/sandboxes", { body: params, opts });
  }

  retrieve(id: string, opts?: RequestOptions): Promise<Sandbox> {
    return this.c.json<Sandbox>("GET", `/sandboxes/${id}`, { opts });
  }

  /**
   * Kill the sandbox and get back its final cost.
   *
   * Those numbers are sampled just before the kill and cannot be had after: the
   * accounting dies with the VM. This reply is the only record of what the
   * sandbox consumed.
   */
  delete(id: string, opts?: RequestOptions): Promise<Sandbox> {
    return this.c.json<Sandbox>("DELETE", `/sandboxes/${id}`, { opts });
  }

  list(
    params: ListParams & { state?: SandboxState } = {},
    opts?: RequestOptions,
  ): Promise<SandboxList> {
    return this.c.json<SandboxList>("GET", "/sandboxes", { query: params, opts });
  }

  /**
   * Every sandbox, fetching pages as needed.
   *
   * Paging is mechanical and easy to get subtly wrong — forgetting `has_more`,
   * or taking the cursor from the wrong end — and it is the same loop every
   * time, so it lives here rather than in every caller.
   */
  async *all(
    params: ListParams & { state?: SandboxState } = {},
    opts?: RequestOptions,
  ): AsyncGenerator<Sandbox> {
    let cursor = params.starting_after;
    for (;;) {
      const page = await this.list({ ...params, starting_after: cursor }, opts);
      for (const sb of page.data) yield sb;
      if (!page.has_more || page.data.length === 0) return;
      cursor = page.data[page.data.length - 1]!.id;
    }
  }
}

class ExecutionResource {
  constructor(private readonly c: Client) {}

  /**
   * Start a command and return at once, without waiting for it.
   *
   * The command belongs to the sandbox, not to this call: dropping the
   * connection does not kill it. Follow it with `stream`, or collect it later
   * with `retrieve`.
   */
  create(
    sandboxID: string,
    params: ExecutionCreateParams,
    opts?: RequestOptions,
  ): Promise<Execution> {
    return this.c.json<Execution>("POST", `/sandboxes/${sandboxID}/executions`, {
      body: params,
      opts,
    });
  }

  /**
   * An execution and everything it printed.
   *
   * Works after the sandbox is gone, which is the point: the output you most
   * want is from the run that was killed.
   */
  retrieve(
    sandboxID: string,
    executionID: string,
    opts?: RequestOptions,
  ): Promise<Execution> {
    return this.c.json<Execution>(
      "GET",
      `/sandboxes/${sandboxID}/executions/${executionID}`,
      { opts },
    );
  }

  list(
    sandboxID: string,
    params: ListParams = {},
    opts?: RequestOptions,
  ): Promise<ExecutionList> {
    return this.c.json<ExecutionList>("GET", `/sandboxes/${sandboxID}/executions`, {
      query: params,
      opts,
    });
  }

  /**
   * Signal a running execution.
   *
   * The signal reaches the whole process group, so a program that spawned
   * children does not leave them behind. Defaults to SIGKILL. Cancelling
   * something that already finished is not an error.
   */
  cancel(
    sandboxID: string,
    executionID: string,
    params: ExecutionCancelParams = {},
    opts?: RequestOptions,
  ): Promise<Execution> {
    return this.c.json<Execution>(
      "POST",
      `/sandboxes/${sandboxID}/executions/${executionID}/cancel`,
      { body: params, opts },
    );
  }

  /**
   * Follow an execution's output as it is produced.
   *
   * The stream replays from the beginning before it follows, so connecting late
   * — or reconnecting after a dropped connection — loses nothing. Aborting the
   * signal stops watching; the execution keeps running, because it belongs to
   * its sandbox. To stop the execution itself, use `cancel`.
   *
   * ```ts
   * for await (const frame of client.executions.stream(sbID, exeID)) {
   *   if (frame.type === "stdout") process.stdout.write(frameText(frame));
   * }
   * ```
   */
  async *stream(
    sandboxID: string,
    executionID: string,
    opts?: RequestOptions,
  ): AsyncGenerator<Frame> {
    const resp = await this.c.request(
      "GET",
      `/sandboxes/${sandboxID}/executions/${executionID}/stream`,
      { opts, noTimeout: true },
    );
    if (!resp.body) throw new Error("microvm: the stream had no body");

    const reader = resp.body.pipeThrough(new TextDecoderStream()).getReader();
    let buffer = "";

    try {
      for (;;) {
        const { done, value } = await reader.read();
        if (done) break;
        buffer += value;

        // SSE events are separated by a blank line, and a network chunk can
        // split one anywhere. Only whole events are parsed; the tail waits for
        // the rest of itself.
        let sep: number;
        while ((sep = buffer.indexOf("\n\n")) !== -1) {
          const event = buffer.slice(0, sep);
          buffer = buffer.slice(sep + 2);
          for (const line of event.split("\n")) {
            if (!line.startsWith("data: ")) continue;
            yield JSON.parse(line.slice(6)) as Frame;
          }
        }
      }
    } finally {
      // Matters on an early return: breaking out of the loop must not leave the
      // connection held open.
      await reader.cancel().catch(() => {});
    }
  }

  /**
   * Wait for an execution to finish.
   *
   * Polls rather than streams: streaming is for showing output as it appears,
   * waiting is for knowing the result, and polling survives a dropped
   * connection without any work from the caller.
   */
  async wait(
    sandboxID: string,
    executionID: string,
    opts?: RequestOptions,
  ): Promise<Execution> {
    let delay = 25;
    for (;;) {
      const exe = await this.retrieve(sandboxID, executionID, opts);
      if (exe.status !== "running") return exe;
      await sleep(delay, opts?.signal);
      // Tight at first so a 50ms command is noticed in 50ms, easing off so a
      // ten-minute one is not asked about twelve thousand times.
      delay = Math.min(delay * 2, 1_000);
    }
  }
}

class FileResource {
  constructor(private readonly c: Client) {}

  /** Write a file into the sandbox, making parent directories. */
  write(
    sandboxID: string,
    path: string,
    content: string | Uint8Array,
    opts?: RequestOptions,
  ): Promise<File> {
    return this.c.json<File>("POST", `/sandboxes/${sandboxID}/files`, {
      body: { path, content: encodeContent(content) },
      opts,
    });
  }

  /** Download a file's bytes. */
  async retrieve(sandboxID: string, path: string, opts?: RequestOptions): Promise<Uint8Array> {
    const resp = await this.c.request("GET", `/sandboxes/${sandboxID}/files`, {
      query: { path },
      opts,
    });
    return new Uint8Array(await resp.arrayBuffer());
  }

  /** Download a file as text. */
  async readText(sandboxID: string, path: string, opts?: RequestOptions): Promise<string> {
    return new TextDecoder().decode(await this.retrieve(sandboxID, path, opts));
  }
}

class TaskResource {
  constructor(private readonly c: Client) {}

  /**
   * Queue work for the fleet.
   *
   * Unlike creating a sandbox this never fails for capacity: the task waits for
   * a slot on any node. Use it for throughput, and a sandbox for several
   * commands that share state.
   */
  create(params: TaskCreateParams, opts?: RequestOptions): Promise<Task> {
    // The files arrive as text or bytes for the caller's convenience; the wire
    // wants base64, so encode them here rather than leaving it as a trap.
    const body = params.files
      ? { ...params, files: encodeFiles(params.files) }
      : params;
    return this.c.json<Task>("POST", "/tasks", { body, opts });
  }

  retrieve(taskID: string, opts?: RequestOptions): Promise<Task> {
    return this.c.json<Task>("GET", `/tasks/${taskID}`, { opts });
  }

  /** Wait for a task to have a result. */
  async wait(taskID: string, opts?: RequestOptions): Promise<Task> {
    let delay = 50;
    for (;;) {
      const task = await this.retrieve(taskID, opts);
      if (task.status !== "pending" && task.status !== "running") return task;
      await sleep(delay, opts?.signal);
      delay = Math.min(delay * 2, 2_000);
    }
  }
}

class QueueResource {
  constructor(private readonly c: Client) {}

  /**
   * The queue's depth and this node's slots.
   *
   * The depth is the fleet's; the slots are this node's alone. No node knows
   * the fleet's capacity, which is what lets one be added without telling
   * anything else.
   */
  retrieve(opts?: RequestOptions): Promise<Queue> {
    return this.c.json<Queue>("GET", "/queue", { opts });
  }
}

class ImageResource {
  constructor(private readonly c: Client) {}

  list(opts?: RequestOptions): Promise<ImageList> {
    return this.c.json<ImageList>("GET", "/images", { opts });
  }
}

/**
 * The `/v1/tenants` resource, and it is administrative: a tenant's storage cap
 * is set by an operator, never by the code that runs under it. Updating needs an
 * admin token; an ordinary key is refused with a 403 (see `APIError.isForbidden`).
 */
class TenantResource {
  constructor(private readonly c: Client) {}

  /** Set a tenant's byte cap and full policy, replacing any previous one. */
  update(
    tenantID: string,
    params: TenantUpdateParams,
    opts?: RequestOptions,
  ): Promise<Tenant> {
    return this.c.json<Tenant>("PUT", `/tenants/${tenantID}`, { body: params, opts });
  }

  /**
   * `update` for the common case: a byte cap and a policy. Pass `"preserve"` to
   * reject writes when full, or `"evict"` to delete the oldest objects to make
   * room. A `maxBytes` of 0 means unlimited.
   */
  setLimit(
    tenantID: string,
    maxBytes: number,
    policy: TenantFullPolicy,
    opts?: RequestOptions,
  ): Promise<Tenant> {
    return this.update(tenantID, { max_bytes: maxBytes, policy }, opts);
  }

  /**
   * A tenant's policy and its current usage, the usage read live from the bucket
   * at call time (so it costs a listing — see `Tenant.usage_bytes`).
   */
  retrieve(tenantID: string, opts?: RequestOptions): Promise<Tenant> {
    return this.c.json<Tenant>("GET", `/tenants/${tenantID}`, { opts });
  }

  /** Every configured tenant. A tenant with no policy is absent: it is unlimited. */
  list(opts?: RequestOptions): Promise<TenantList> {
    return this.c.json<TenantList>("GET", "/tenants", { opts });
  }
}

/**
 * Why an execution did not simply run to completion, or null.
 *
 * A non-zero exit returns null: the process ran, and that is its own verdict
 * rather than a failure of ours. The endings that are *not* the code's doing —
 * a timeout, a cancel, a VM taken away, a command that never started — return
 * an error, because those are the ones that must not be mistaken for a program
 * choosing to fail.
 */
export function err(exe: Execution): Error | null {
  switch (exe.status) {
    case "running":
    case "exited":
      return null;
    case "timed_out":
      return new Error(`microvm: execution ${exe.id} exceeded its timeout and was killed`);
    case "canceled":
      return new Error(`microvm: execution ${exe.id} was cancelled`);
    case "vanished":
      return new Error(
        `microvm: the sandbox holding execution ${exe.id} was taken away ` +
          `(its TTL, the idle reclaim, or the VM died); your code did not fail`,
      );
    case "failed":
      return new Error(
        `microvm: execution ${exe.id}: ${exe.error ?? "the command could never start"}`,
      );
    default:
      return new Error(`microvm: execution ${exe.id} has an unknown status`);
  }
}

/** A frame's bytes. */
export function frameBytes(frame: Frame): Uint8Array {
  return frame.data ? fromBase64(frame.data) : new Uint8Array(0);
}

/** A frame's bytes as text. */
export function frameText(frame: Frame): string {
  return new TextDecoder().decode(frameBytes(frame));
}

/**
 * Sleep, honouring an abort signal.
 *
 * The listener is removed on the way out. Without that, polling in a loop
 * against one long-lived signal adds a listener per iteration, and a wait that
 * runs for an hour leaks steadily.
 */
function sleep(ms: number, signal?: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    if (signal?.aborted) return reject(signal.reason);
    const onAbort = () => {
      clearTimeout(timer);
      reject(signal!.reason);
    };
    const timer = setTimeout(() => {
      signal?.removeEventListener("abort", onAbort);
      resolve();
    }, ms);
    signal?.addEventListener("abort", onAbort, { once: true });
  });
}

/** Whether a status is worth another try: the server overloaded (429) or
 * momentarily broken (5xx), not the request being wrong. */
function retryableStatus(status: number): boolean {
  return (
    status === 429 ||
    status === 500 ||
    status === 502 ||
    status === 503 ||
    status === 504
  );
}

/** Waits before the next attempt: Retry-After if the server gave one, else
 * exponential backoff with full jitter, capped. Aborts with the signal. */
async function backoff(
  attempt: number,
  retryAfter: string | undefined,
  signal?: AbortSignal,
): Promise<void> {
  const baseMs = 100;
  const maxMs = 3_000;

  let waitMs = parseRetryAfter(retryAfter);
  if (waitMs <= 0) {
    // base, 2x, 4x, ... capped; full jitter spreads a herd of clients that all
    // failed at the same instant.
    const ceil = Math.min(maxMs, baseMs * 2 ** (attempt - 1));
    waitMs = Math.floor(Math.random() * (ceil + 1));
  }
  await sleep(Math.min(waitMs, maxMs), signal);
}

/** Reads Retry-After in both forms: a number of seconds, or an HTTP date. An
 * unparseable value is 0, which falls back to the client's own backoff. */
function parseRetryAfter(v: string | undefined): number {
  if (!v) return 0;
  const trimmed = v.trim();
  const secs = Number(trimmed);
  if (Number.isFinite(secs)) return secs > 0 ? secs * 1000 : 0;
  const date = Date.parse(trimmed);
  if (!Number.isNaN(date)) {
    const delta = date - Date.now();
    return delta > 0 ? delta : 0;
  }
  return 0;
}

/**
 * Base64, in chunks.
 *
 * `String.fromCharCode(...bytes)` is the obvious one-liner and it throws on
 * anything large: spreading a megabyte-long array overflows the call stack, and
 * a file upload is exactly that size.
 */
const CHUNK = 0x8000;

function toBase64(bytes: Uint8Array): string {
  let binary = "";
  for (let i = 0; i < bytes.length; i += CHUNK) {
    binary += String.fromCharCode(...bytes.subarray(i, i + CHUNK));
  }
  return btoa(binary);
}

/** Base64 of file content given as text or raw bytes — the one place that turns
 * what a caller has into what the wire wants, shared by uploads and tasks. */
function encodeContent(content: string | Uint8Array): string {
  const bytes = typeof content === "string" ? new TextEncoder().encode(content) : content;
  return toBase64(bytes);
}

/** A task's files map, each value encoded, the paths untouched. */
function encodeFiles(
  files: Record<string, string | Uint8Array>,
): Record<string, string> {
  const out: Record<string, string> = {};
  for (const [path, content] of Object.entries(files)) {
    out[path] = encodeContent(content);
  }
  return out;
}

function fromBase64(b64: string): Uint8Array {
  const binary = atob(b64);
  const out = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i++) out[i] = binary.charCodeAt(i);
  return out;
}
