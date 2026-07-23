"""Client for the microvm daemon: run untrusted code in Firecracker microVMs.

Standard library only -- no third-party dependencies. API objects come back as
plain dicts (their shapes are the OpenAPI schemas), so ``sb["id"]`` and
``exe["stdout"]`` are how you read them.
"""

from __future__ import annotations

import base64
import json
import random
import time
import urllib.error
import urllib.parse
import urllib.request
from typing import Any, Callable, Iterator, Optional

__all__ = ["Client", "APIError", "RequestInfo", "__version__"]

__version__ = "0.1.0"

DEFAULT_BASE_URL = "http://127.0.0.1:8080"
DEFAULT_TIMEOUT = 30.0
DEFAULT_MAX_RETRIES = 2

_RETRYABLE_STATUS = frozenset({429, 500, 502, 503, 504})
_IDEMPOTENT_METHODS = frozenset({"GET", "HEAD", "PUT", "DELETE"})


class APIError(Exception):
    """A failure the API reported.

    The fields are the API's own. ``type`` is what to branch on -- it says what
    class of thing went wrong -- while ``code`` says exactly which.
    """

    def __init__(
        self,
        status: int,
        type: str = "api_error",
        code: str = "unknown",
        message: str = "",
        param: Optional[str] = None,
        request_id: Optional[str] = None,
    ):
        self.status = status
        self.type = type
        self.code = code
        self.message = message
        self.param = param
        self.request_id = request_id
        detail = f"microvm: {message} ({code})"
        if param:
            detail += f" [param: {param}]"
        if request_id:
            detail += f" [request: {request_id}]"
        super().__init__(detail)

    @property
    def is_not_found(self) -> bool:
        return self.status == 404

    @property
    def is_capacity(self) -> bool:
        """The node has no room. The one error worth retrying as a task."""
        return self.type == "capacity_error"

    @property
    def is_conflict(self) -> bool:
        return self.status == 409

    @property
    def is_forbidden(self) -> bool:
        """The key lacks permission -- an ordinary token on an admin-only route."""
        return self.status == 403


class RequestInfo:
    """What an on_response observer is told about one HTTP attempt."""

    def __init__(self, method, path, attempt, status, error, duration_s):
        self.method = method
        self.path = path
        self.attempt = attempt
        self.status = status
        self.error = error
        self.duration_s = duration_s


class Client:
    """Talks to a microvm daemon.

    Resources hang off it by name: ``client.sandboxes.create(...)`` reads as
    what it does.
    """

    def __init__(
        self,
        base_url: str = DEFAULT_BASE_URL,
        token: Optional[str] = None,
        *,
        timeout: float = DEFAULT_TIMEOUT,
        max_retries: int = DEFAULT_MAX_RETRIES,
        on_response: Optional[Callable[[RequestInfo], None]] = None,
        opener: Optional[urllib.request.OpenerDirector] = None,
    ):
        self._base = base_url.rstrip("/")
        self._token = token
        self._timeout = timeout
        self._max_retries = max_retries
        self._on_response = on_response
        # A custom opener is the injection point for tests and for proxies or TLS
        # settings a caller needs.
        self._opener = opener or urllib.request.build_opener()

        self.sandboxes = Sandboxes(self)
        self.executions = Executions(self)
        self.files = Files(self)
        self.tasks = Tasks(self)
        self.queue = Queue(self)
        self.images = Images(self)
        self.tenants = Tenants(self)

    # -- convenience -----------------------------------------------------

    def health(self) -> dict:
        """Whether the daemon is up. Needs no token."""
        return self._request("GET", "/health")

    def run(
        self,
        sandbox_id: str,
        cmd: str,
        *args: str,
        env: Optional[dict] = None,
        timeout_seconds: Optional[int] = None,
    ) -> dict:
        """Start a command in a sandbox and block until it finishes.

        The returned execution carries the output and exit code. A non-zero exit
        is the code's own verdict; a timeout or a vanished sandbox is not -- see
        its ``status``.
        """
        exe = self.executions.create(
            sandbox_id, cmd, list(args), env=env, timeout_seconds=timeout_seconds
        )
        return self.executions.wait(sandbox_id, exe["id"])

    # -- transport -------------------------------------------------------

    def _request(
        self,
        method: str,
        path: str,
        *,
        body: Any = None,
        query: Optional[dict] = None,
        idempotency_key: Optional[str] = None,
    ) -> Any:
        """Perform a request and return the decoded JSON (or None for an empty
        reply). Transient failures are retried per the client's policy."""
        with self._open(method, path, body, query, idempotency_key) as resp:
            raw = resp.read()
        if not raw:
            return None
        return json.loads(raw)

    def _stream(self, path: str) -> Iterator[dict]:
        """Open an SSE stream and yield its frames. The connection has no timeout:
        a command may legitimately run for hours."""
        resp = self._open("GET", path, None, None, None, no_timeout=True)
        try:
            for line in resp:
                text = line.decode("utf-8", "replace").rstrip("\n")
                if not text.startswith("data: "):
                    continue  # SSE comments and blank separators
                frame = json.loads(text[len("data: ") :])
                if isinstance(frame.get("data"), str):
                    frame["data"] = base64.b64decode(frame["data"])
                yield frame
        finally:
            resp.close()

    def _open(
        self,
        method: str,
        path: str,
        body: Any,
        query: Optional[dict],
        idempotency_key: Optional[str],
        no_timeout: bool = False,
    ):
        url = self._base + "/v1" + path
        if query:
            pairs = {k: v for k, v in query.items() if v is not None}
            if pairs:
                url += "?" + urllib.parse.urlencode(pairs)

        data = None
        headers = {"User-Agent": f"microvm-python/{__version__}"}
        if body is not None:
            data = json.dumps(body).encode("utf-8")
            headers["Content-Type"] = "application/json"
        if self._token:
            headers["Authorization"] = f"Bearer {self._token}"
        if idempotency_key:
            headers["Idempotency-Key"] = idempotency_key

        idempotent = method in _IDEMPOTENT_METHODS or idempotency_key is not None
        # A stream has no timeout: a command may run for hours. None tells urllib
        # to block, which is what "no timeout" means at the socket layer.
        req_timeout = None if no_timeout else self._timeout

        last_exc: Optional[BaseException] = None
        for attempt in range(1, self._max_retries + 2):
            req = urllib.request.Request(url, data=data, method=method, headers=headers)
            start = time.monotonic()
            try:
                resp = self._opener.open(req, timeout=req_timeout)
                self._observe(method, path, attempt, resp.status, None, start)
                return resp
            except urllib.error.HTTPError as e:
                status = e.code
                self._observe(method, path, attempt, status, e, start)
                if status in _RETRYABLE_STATUS and idempotent and attempt <= self._max_retries:
                    retry_after = e.headers.get("Retry-After") if e.headers else None
                    e.close()
                    _backoff(attempt, retry_after)
                    continue
                raise _api_error(e) from None
            except urllib.error.URLError as e:
                self._observe(method, path, attempt, 0, e, start)
                last_exc = e
                if idempotent and attempt <= self._max_retries:
                    _backoff(attempt, None)
                    continue
                raise
        assert last_exc is not None
        raise last_exc

    def _observe(self, method, path, attempt, status, error, start):
        if self._on_response:
            self._on_response(
                RequestInfo(method, path, attempt, status, error, time.monotonic() - start)
            )


# -- resources -----------------------------------------------------------


class Sandboxes:
    def __init__(self, c: Client):
        self._c = c

    def create(self, image: str, **params: Any) -> dict:
        """Boot a sandbox. Raises APIError with is_capacity when the node is full."""
        return self._c._request("POST", "/sandboxes", body={"image": image, **params})

    def retrieve(self, sandbox_id: str) -> dict:
        return self._c._request("GET", f"/sandboxes/{sandbox_id}")

    def delete(self, sandbox_id: str) -> dict:
        return self._c._request("DELETE", f"/sandboxes/{sandbox_id}")

    def list(self, **params: Any) -> dict:
        return self._c._request("GET", "/sandboxes", query=params)

    def all(self, **params: Any) -> Iterator[dict]:
        """Iterate every sandbox, following has_more so no cursor is threaded by
        hand."""
        params = dict(params)
        while True:
            page = self.list(**params)
            data = page.get("data", [])
            for sb in data:
                yield sb
            if not page.get("has_more") or not data:
                return
            params["starting_after"] = data[-1]["id"]
            params.pop("ending_before", None)


class Executions:
    def __init__(self, c: Client):
        self._c = c

    def create(
        self,
        sandbox_id: str,
        cmd: str,
        args: Optional[list] = None,
        *,
        env: Optional[dict] = None,
        timeout_seconds: Optional[int] = None,
        **params: Any,
    ) -> dict:
        body: dict = {"cmd": cmd}
        if args:
            body["args"] = args
        if env:
            body["env"] = env
        if timeout_seconds is not None:
            body["timeout_seconds"] = timeout_seconds
        body.update(params)
        return self._c._request("POST", f"/sandboxes/{sandbox_id}/executions", body=body)

    def retrieve(self, sandbox_id: str, execution_id: str) -> dict:
        return self._c._request(
            "GET", f"/sandboxes/{sandbox_id}/executions/{execution_id}"
        )

    def list(self, sandbox_id: str, **params: Any) -> dict:
        return self._c._request(
            "GET", f"/sandboxes/{sandbox_id}/executions", query=params
        )

    def all(self, sandbox_id: str, **params: Any) -> Iterator[dict]:
        params = dict(params)
        while True:
            page = self.list(sandbox_id, **params)
            data = page.get("data", [])
            for e in data:
                yield e
            if not page.get("has_more") or not data:
                return
            params["starting_after"] = data[-1]["id"]
            params.pop("ending_before", None)

    def cancel(self, sandbox_id: str, execution_id: str, **params: Any) -> dict:
        return self._c._request(
            "POST",
            f"/sandboxes/{sandbox_id}/executions/{execution_id}/cancel",
            body=params or None,
        )

    def stream(self, sandbox_id: str, execution_id: str) -> Iterator[dict]:
        """Follow an execution's output as it is produced. Each frame is a dict;
        a stdout/stderr frame's ``data`` is decoded to bytes."""
        return self._c._stream(
            f"/sandboxes/{sandbox_id}/executions/{execution_id}/stream"
        )

    def wait(self, sandbox_id: str, execution_id: str) -> dict:
        """Poll until the execution finishes, then return it."""
        delay = 0.025
        while True:
            exe = self.retrieve(sandbox_id, execution_id)
            if exe.get("status") != "running":
                return exe
            time.sleep(delay)
            delay = min(delay * 2, 1.0)


class Files:
    def __init__(self, c: Client):
        self._c = c

    def write(self, sandbox_id: str, path: str, content: bytes | str) -> dict:
        return self._c._request(
            "POST",
            f"/sandboxes/{sandbox_id}/files",
            body={"path": path, "content": _encode_file_content(content)},
        )

    def read(self, sandbox_id: str, path: str) -> bytes:
        with self._c._open(
            "GET", f"/sandboxes/{sandbox_id}/files", None, {"path": path}, None
        ) as resp:
            return resp.read()


class Tasks:
    def __init__(self, c: Client):
        self._c = c

    def create(self, image: str, cmd: str, *, idempotency_key: Optional[str] = None, **params: Any) -> dict:
        """Queue work for the fleet. Never fails for capacity; waits for a slot on
        any node, sized to the cpu/mem requested. priority is 0-10, higher first.

        A task's ``files`` (a path -> content map) are written into the sandbox
        before ``cmd`` runs. Give the content as text or bytes; it is
        base64-encoded for you, exactly as ``files.write`` does."""
        if params.get("files") is not None:
            params = {**params, "files": _encode_files(params["files"])}
        return self._c._request(
            "POST",
            "/tasks",
            body={"image": image, "cmd": cmd, **params},
            idempotency_key=idempotency_key,
        )

    def retrieve(self, task_id: str) -> dict:
        return self._c._request("GET", f"/tasks/{task_id}")

    def wait(self, task_id: str) -> dict:
        """Poll until the task has a result, wherever in the fleet it ran."""
        delay = 0.05
        while True:
            task = self.retrieve(task_id)
            if task.get("status") not in ("pending", "running"):
                return task
            time.sleep(delay)
            delay = min(delay * 2, 2.0)


class Queue:
    def __init__(self, c: Client):
        self._c = c

    def retrieve(self) -> dict:
        return self._c._request("GET", "/queue")


class Images:
    def __init__(self, c: Client):
        self._c = c

    def list(self) -> dict:
        return self._c._request("GET", "/images")


class Tenants:
    """Administrative: setting a policy needs an admin token; an ordinary key
    gets a 403 (APIError.is_forbidden)."""

    def __init__(self, c: Client):
        self._c = c

    def update(self, tenant_id: str, max_bytes: int, policy: str) -> dict:
        return self._c._request(
            "PUT",
            f"/tenants/{tenant_id}",
            body={"max_bytes": max_bytes, "policy": policy},
        )

    # An alias that reads as the common intent.
    set_limit = update

    def retrieve(self, tenant_id: str) -> dict:
        """Policy plus current usage, read live from the bucket."""
        return self._c._request("GET", f"/tenants/{tenant_id}")

    def list(self) -> dict:
        return self._c._request("GET", "/tenants")


# -- content helpers -----------------------------------------------------


def _encode_file_content(content: bytes | str) -> str:
    """Base64 of a file's content, given as text or raw bytes. The one place
    that turns what a caller has into what the wire wants, shared by uploads and
    tasks."""
    data = content.encode("utf-8") if isinstance(content, str) else content
    return base64.b64encode(data).decode("ascii")


def _encode_files(files: dict) -> dict:
    """A task's files map with each value base64-encoded, the paths untouched."""
    return {path: _encode_file_content(content) for path, content in files.items()}


# -- retry helpers -------------------------------------------------------


def _backoff(attempt: int, retry_after: Optional[str]) -> None:
    base, cap = 0.1, 3.0
    wait = _parse_retry_after(retry_after)
    if wait <= 0:
        # base, 2x, 4x, ... capped; full jitter spreads a herd of clients that
        # all failed at the same instant.
        ceil = min(cap, base * (2 ** (attempt - 1)))
        wait = random.uniform(0, ceil)
    time.sleep(min(wait, cap))


def _parse_retry_after(v: Optional[str]) -> float:
    if not v:
        return 0.0
    v = v.strip()
    try:
        secs = int(v)
        return float(secs) if secs > 0 else 0.0
    except ValueError:
        pass
    try:
        from email.utils import parsedate_to_datetime

        when = parsedate_to_datetime(v)
        delta = when.timestamp() - time.time()
        return delta if delta > 0 else 0.0
    except (TypeError, ValueError):
        return 0.0


def _api_error(e: urllib.error.HTTPError) -> APIError:
    request_id = e.headers.get("X-Request-Id") if e.headers else None
    try:
        raw = e.read()
    except Exception:
        raw = b""
    if raw:
        try:
            env = json.loads(raw).get("error", {})
            return APIError(
                status=e.code,
                type=env.get("type", "api_error"),
                code=env.get("code", "unknown"),
                message=env.get("message", ""),
                param=env.get("param"),
                request_id=env.get("request_id") or request_id,
            )
        except (ValueError, AttributeError):
            return APIError(status=e.code, message=raw.decode("utf-8", "replace"), request_id=request_id)
    return APIError(status=e.code, message=e.reason or "", request_id=request_id)
