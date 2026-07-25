"""Tests for the microvm Python client.

They run a real local HTTP server so the whole urllib path -- retries included --
is exercised, not a mock of it. Standard library only, runnable with:

    python3 -m pytest sdk/python/tests      # or:
    python3 sdk/python/tests/test_client.py  # runs a tiny built-in harness
"""

import json
import threading
from http.server import BaseHTTPRequestHandler, HTTPServer

import sys
import os

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from microvm import APIError, Client, RequestInfo  # noqa: E402


class _Handler(BaseHTTPRequestHandler):
    # Set per test.
    behaviour = None  # callable(handler) -> None

    def log_message(self, *a):
        pass

    def _run(self):
        type(self).behaviour(self)

    do_GET = _run
    do_POST = _run
    do_PUT = _run
    do_DELETE = _run

    def json(self, status, obj):
        body = json.dumps(obj).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def empty(self, status, headers=None):
        self.send_response(status)
        for k, v in (headers or {}).items():
            self.send_header(k, v)
        self.send_header("Content-Length", "0")
        self.end_headers()


def serve(behaviour):
    _Handler.behaviour = staticmethod(behaviour)
    srv = HTTPServer(("127.0.0.1", 0), _Handler)
    t = threading.Thread(target=srv.serve_forever, daemon=True)
    t.start()
    url = f"http://127.0.0.1:{srv.server_address[1]}"
    return srv, url


def test_retries_transient_then_succeeds():
    state = {"n": 0}

    def behaviour(h):
        state["n"] += 1
        if state["n"] < 3:
            h.empty(503)
        else:
            h.json(200, {"ok": True})

    srv, url = serve(behaviour)
    try:
        c = Client(url, max_retries=3)
        assert c.health() == {"ok": True}
        assert state["n"] == 3, state["n"]
    finally:
        srv.shutdown()


def test_gives_up_after_max_retries():
    state = {"n": 0}

    def behaviour(h):
        state["n"] += 1
        h.empty(503)

    srv, url = serve(behaviour)
    try:
        c = Client(url, max_retries=2)
        raised = False
        try:
            c.health()
        except APIError as e:
            raised = True
            assert e.status == 503
        assert raised
        assert state["n"] == 3, state["n"]  # 1 try + 2 retries
    finally:
        srv.shutdown()


def test_does_not_retry_client_error():
    state = {"n": 0}

    def behaviour(h):
        state["n"] += 1
        h.json(400, {"error": {"type": "invalid_request_error", "code": "bad", "message": "no"}})

    srv, url = serve(behaviour)
    try:
        c = Client(url, max_retries=3)
        try:
            c.health()
            assert False
        except APIError as e:
            assert e.code == "bad"
        assert state["n"] == 1
    finally:
        srv.shutdown()


def test_unkeyed_post_not_retried_but_keyed_is():
    state = {"n": 0}

    def behaviour(h):
        state["n"] += 1
        h.empty(503)

    srv, url = serve(behaviour)
    try:
        c = Client(url, max_retries=2)
        # Unkeyed POST: one attempt only.
        try:
            c.tasks.create("python", "python3")
        except APIError:
            pass
        assert state["n"] == 1, state["n"]

        state["n"] = 0
        # Keyed POST: retried.
        try:
            c.tasks.create("python", "python3", idempotency_key="k1")
        except APIError:
            pass
        assert state["n"] == 3, state["n"]
    finally:
        srv.shutdown()


def test_forbidden_is_typed():
    def behaviour(h):
        h.json(403, {"error": {"type": "authentication_error", "code": "forbidden", "message": "no"}})

    srv, url = serve(behaviour)
    try:
        c = Client(url)
        try:
            c.tenants.set_limit("t_a", 1024, "evict")
            assert False
        except APIError as e:
            assert e.is_forbidden
    finally:
        srv.shutdown()


def test_pagination_follows_has_more():
    pages = [
        {"data": [{"id": "sb_1"}, {"id": "sb_2"}], "has_more": True},
        {"data": [{"id": "sb_3"}], "has_more": False},
    ]
    state = {"i": 0}

    def behaviour(h):
        page = pages[min(state["i"], len(pages) - 1)]
        state["i"] += 1
        h.json(200, page)

    srv, url = serve(behaviour)
    try:
        c = Client(url)
        ids = [sb["id"] for sb in c.sandboxes.all()]
        assert ids == ["sb_1", "sb_2", "sb_3"], ids
    finally:
        srv.shutdown()


def test_observer_sees_every_attempt():
    state = {"n": 0}

    def behaviour(h):
        state["n"] += 1
        if state["n"] < 3:
            h.empty(502)
        else:
            h.json(200, {"ok": True})

    seen = []
    srv, url = serve(behaviour)
    try:
        c = Client(url, max_retries=3, on_response=lambda info: seen.append(info))
        c.health()
        assert len(seen) == 3, len(seen)
        assert [i.attempt for i in seen] == [1, 2, 3]
        assert seen[-1].status == 200
    finally:
        srv.shutdown()


def test_tag_filter_repeats_the_key():
    """The server splits each value at its first colon and ANDs the repeats, so a
    joined value would name a tag nobody set."""
    seen = {}

    def behaviour(h):
        seen["query"] = h.path
        h.json(200, {"data": [], "has_more": False})

    srv, url = serve(behaviour)
    try:
        c = Client(url)
        c.sandboxes.list(limit=10, tags={"owner": "me", "env": "ci"})
        # Sorted by key, so the same filter always produces the same URL.
        assert seen["query"] == "/v1/sandboxes?limit=10&tag=env%3Aci&tag=owner%3Ame", seen["query"]

        c.sandboxes.list(limit=10)
        assert seen["query"] == "/v1/sandboxes?limit=10", seen["query"]
    finally:
        srv.shutdown()


def test_routes_idempotent_by_design_are_retried():
    """These four take no Idempotency-Key, so without being marked replayable a 429
    from a per-tenant rate limit reaches calling code unretried. Repeating them is a
    no-op by construction: extend never brings a deadline forward, a write is an
    overwrite, mkdir -p on an existing directory is success."""
    state = {"n": 0}

    def behaviour(h):
        state["n"] += 1
        h.empty(429, {"Retry-After": "1"})

    srv, url = serve(behaviour)
    try:
        c = Client(url, max_retries=2)
        calls = [
            lambda: c.sandboxes.extend("sb_1", 600),
            lambda: c.files.write("sb_1", "/app/main.py", "x = 1\n"),
            lambda: c.files.write_batch("sb_1", {"/app/main.py": "x = 1\n"}),
            lambda: c.files.mkdir("sb_1", "/app/out"),
        ]
        for call in calls:
            state["n"] = 0
            try:
                call()
                assert False, "expected an APIError"
            except APIError:
                pass
            assert state["n"] == 3, state["n"]
    finally:
        srv.shutdown()


if __name__ == "__main__":
    # A dependency-free runner, so the suite works without pytest installed.
    fns = [v for k, v in sorted(globals().items()) if k.startswith("test_") and callable(v)]
    failed = 0
    for fn in fns:
        try:
            fn()
            print(f"ok   {fn.__name__}")
        except Exception as e:  # noqa: BLE001
            failed += 1
            print(f"FAIL {fn.__name__}: {e!r}")
    print(f"\n{len(fns) - failed}/{len(fns)} passed")
    sys.exit(1 if failed else 0)
