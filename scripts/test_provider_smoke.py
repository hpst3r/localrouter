#!/usr/bin/env python3
"""Hermetic unit tests for ``scripts/provider-smoke.py``.

These tests never touch a real provider and never read a real credential:

* every HTTP request goes to a throwaway loopback server started in-process
  (``http.server`` on 127.0.0.1 with an ephemeral port);
* the "zero network / no key read" behaviour is asserted by monkeypatching the
  module's connection factory and key reader to raise if they are ever called;
* the sentinel key, the synthetic prompt and a sentinel response body are
  asserted absent from stdout, stderr and the JSON report in every test.

Run with:  python3 -m unittest discover -s scripts -p 'test_provider_smoke.py'
or:        python3 scripts/test_provider_smoke.py
"""

from __future__ import annotations

import contextlib
import http.server
import importlib.util
import io
import json
import os
import pathlib
import socket
import sys
import tempfile
import threading
import time
import unittest

HERE = pathlib.Path(__file__).resolve().parent


def _load_module():
    spec = importlib.util.spec_from_file_location("provider_smoke", HERE / "provider-smoke.py")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


ps = _load_module()

KEY = "SENTINEL-KEY-DO-NOT-LEAK-abc123"
SECRET_BODY = "SENTINEL-RESPONSE-CONTENT-SHOULD-NOT-APPEAR"
MODEL = "test/model-1"


# ---------------------------------------------------------------------------
# Fake loopback LocalRouter
# ---------------------------------------------------------------------------


class _Handler(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *args):  # keep test output clean
        return

    def _record_and_dispatch(self):
        try:
            length = int(self.headers.get("Content-Length") or 0)
        except ValueError:
            length = 0
        body = self.rfile.read(length) if length > 0 else b""
        self.server.requests.append(
            {
                "method": self.command,
                "path": self.path,
                "headers": {k.lower(): v for k, v in self.headers.items()},
                "body": body,
            }
        )
        try:
            self.server.responder(self, self.server.requests)
        except (BrokenPipeError, ConnectionResetError):
            pass

    do_GET = _record_and_dispatch
    do_POST = _record_and_dispatch


class _QuietServer(http.server.ThreadingHTTPServer):
    def handle_error(self, request, client_address):
        # Tests disconnect on purpose (caps, timeouts); the keep-alive readline
        # then sees a reset outside _record_and_dispatch. Anything else still prints.
        if isinstance(sys.exc_info()[1], (BrokenPipeError, ConnectionResetError)):
            return
        super().handle_error(request, client_address)


class FakeRouter:
    """In-process loopback HTTP server. Nothing is ever reached but 127.0.0.1."""

    def __init__(self, responder):
        self._httpd = _QuietServer(("127.0.0.1", 0), _Handler)
        self._httpd.daemon_threads = True
        self._httpd.responder = responder
        self._httpd.requests = []
        self._thread = threading.Thread(
            target=self._httpd.serve_forever, kwargs={"poll_interval": 0.05}, daemon=True
        )

    def __enter__(self):
        self._thread.start()
        return self

    def __exit__(self, *exc):
        self._httpd.shutdown()
        self._httpd.server_close()
        self._thread.join(timeout=2)
        return False

    @property
    def url(self):
        host, port = self._httpd.server_address[:2]
        return "http://%s:%d" % (host, port)

    @property
    def requests(self):
        return self._httpd.requests


def json_response(handler, obj, status=200, extra_headers=None):
    raw = json.dumps(obj).encode("utf-8")
    handler.send_response(status)
    handler.send_header("Content-Type", "application/json")
    handler.send_header("Content-Length", str(len(raw)))
    for name, value in (extra_headers or {}).items():
        handler.send_header(name, value)
    handler.end_headers()
    handler.wfile.write(raw)
    handler.wfile.flush()


def raw_response(handler, raw, status=200, content_type="application/json"):
    handler.send_response(status)
    handler.send_header("Content-Type", content_type)
    handler.send_header("Content-Length", str(len(raw)))
    handler.end_headers()
    handler.wfile.write(raw)
    handler.wfile.flush()


def sse_response(handler, pieces, delay=0.0, status=200):
    blob = b"".join(pieces)
    handler.send_response(status)
    handler.send_header("Content-Type", "text/event-stream; charset=utf-8")
    handler.send_header("Content-Length", str(len(blob)))
    handler.end_headers()
    for piece in pieces:
        handler.wfile.write(piece)
        handler.wfile.flush()
        if delay:
            time.sleep(delay)


def sse_stall_response(handler, pieces, declared_len, stall=1.5):
    """Send valid headers plus a partial SSE body, then stall past the read timeout."""
    handler.send_response(200)
    handler.send_header("Content-Type", "text/event-stream")
    handler.send_header("Content-Length", str(declared_len))
    handler.end_headers()
    for piece in pieces:
        handler.wfile.write(piece)
        handler.wfile.flush()
    time.sleep(stall)


def redirect_response(handler, location, status=302):
    handler.send_response(status)
    handler.send_header("Location", location)
    handler.send_header("Content-Length", "0")
    handler.end_headers()


def chat_responder(nonstream=None, stream_pieces=None, stream_delay=0.0, models=None):
    """Dispatch /v1/chat/completions by the request's ``stream`` flag."""

    def responder(handler, requests):
        if handler.path.endswith("/v1/models"):
            data = [{"id": m, "object": "model", "owned_by": "localrouter"} for m in (models or [])]
            json_response(handler, {"object": "list", "data": data})
            return
        if handler.path.endswith("/v1/chat/completions"):
            try:
                body = json.loads(requests[-1]["body"] or b"{}")
            except ValueError:
                body = {}
            if body.get("stream"):
                sse_response(handler, stream_pieces or [], delay=stream_delay)
            else:
                json_response(handler, nonstream if nonstream is not None else {})
            return
        handler.send_response(404)
        handler.send_header("Content-Length", "0")
        handler.end_headers()

    return responder


# ---------------------------------------------------------------------------
# Fixtures
# ---------------------------------------------------------------------------

NONSTREAM_OK = {
    "id": "chatcmpl-1",
    "object": "chat.completion",
    "model": MODEL,
    "choices": [
        {"index": 0, "message": {"role": "assistant", "content": SECRET_BODY}, "finish_reason": "stop"}
    ],
    "usage": {
        "prompt_tokens": 5,
        "completion_tokens": 2,
        "prompt_tokens_details": {"cached_tokens": 1},
        "completion_tokens_details": {"reasoning_tokens": 0},
        "cost": 0.0,
    },
}

# Fragmented on purpose: events split across writes, a comment line, a
# multi-line data field, the usage-bearing final chunk and a [DONE] terminator.
STREAM_PIECES = [
    b'data: {"choices":[{"delta":{"content":"o"}}]}\n\n',
    b'data: {"choices":[{"delta":{"cont',
    b'ent":"k"}}]}\n\n',
    b': keep-alive comment\n\n',
    b'data: {"choices":\n',
    b'data: []}\n\n',
    b'\n',
    b'data: {"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":2,'
    b'"prompt_tokens_details":{"cached_tokens":1},"completion_tokens_details":'
    b'{"reasoning_tokens":0},"cost":0.0002}}\n\n',
    b'data: [DONE]\n\n',
]
STREAM_EVENTS = 5  # 2 deltas + 1 multi-line + 1 usage chunk + [DONE]


class ToolTestCase(unittest.TestCase):
    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self._tmp.cleanup)
        self.key_path = os.path.join(self._tmp.name, "client-key")
        with open(self.key_path, "w", encoding="utf-8") as handle:
            handle.write(KEY + "\n")

    # -- helpers ----------------------------------------------------------

    def patch(self, name, value):
        original = getattr(ps, name)
        setattr(ps, name, value)
        self.addCleanup(setattr, ps, name, original)

    def forbid_network(self):
        def boom(*args, **kwargs):
            raise AssertionError("a network call was attempted")

        self.patch("_open_connection", boom)

    def forbid_key_read(self):
        def boom(*args, **kwargs):
            raise AssertionError("the key file was read")

        self.patch("_read_key_file", boom)

    def run_tool(self, argv):
        out, err = io.StringIO(), io.StringIO()
        with contextlib.redirect_stdout(out), contextlib.redirect_stderr(err):
            code = ps.main(list(argv), out, err)
        return code, out.getvalue(), err.getvalue()

    def assert_no_secrets(self, *texts):
        combined = "".join(texts)
        self.assertNotIn(KEY, combined)
        self.assertNotIn(SECRET_BODY, combined)
        self.assertNotIn(ps.SYNTHETIC_PROMPT, combined)

    def live_args(self, url, **overrides):
        args = ["--live", "--url", url, "--key-file", self.key_path, "--model", MODEL]
        for flag, value in overrides.items():
            args.extend(["--" + flag.replace("_", "-"), str(value)])
        return args

    @staticmethod
    def checks_by_name(report):
        return {check["name"]: check for check in report["checks"]}


# ---------------------------------------------------------------------------
# Zero-network: help, defaults, missing consent, bad args
# ---------------------------------------------------------------------------


class ZeroNetworkTests(ToolTestCase):
    def test_help_makes_no_network_call_and_no_key_read(self):
        self.forbid_network()
        self.forbid_key_read()
        code, out, err = self.run_tool(["--help"])
        self.assertEqual(code, 0)
        self.assertIn("provider-smoke", out)

    def test_bare_invocation_prints_help_and_makes_no_network_call(self):
        self.forbid_network()
        self.forbid_key_read()
        code, out, err = self.run_tool([])
        self.assertEqual(code, 2)
        self.assertIn("usage", (out + err).lower())

    def test_missing_live_consent_refuses_before_network_or_key_read(self):
        self.forbid_network()
        self.forbid_key_read()
        code, out, err = self.run_tool(
            ["--url", "http://127.0.0.1:9", "--key-file", self.key_path, "--model", MODEL]
        )
        self.assertEqual(code, 2)
        self.assertIn("live_required", err)
        self.assert_no_secrets(out, err)

    def test_missing_url_refuses_before_network_or_key_read(self):
        self.forbid_network()
        self.forbid_key_read()
        code, out, err = self.run_tool(["--live", "--key-file", self.key_path, "--model", MODEL])
        self.assertEqual(code, 2)
        self.assertIn("missing_url", err)

    def test_missing_model_refuses_before_network_or_key_read(self):
        self.forbid_network()
        self.forbid_key_read()
        code, out, err = self.run_tool(
            ["--live", "--url", "http://127.0.0.1:9", "--key-file", self.key_path]
        )
        self.assertEqual(code, 2)
        self.assertIn("missing_model", err)

    def test_missing_key_file_refuses_before_network(self):
        self.forbid_network()
        self.forbid_key_read()
        code, out, err = self.run_tool(["--live", "--url", "http://127.0.0.1:9", "--model", MODEL])
        self.assertEqual(code, 2)
        self.assertIn("missing_key_file", err)

    def test_nonloopback_plain_http_refused_without_override(self):
        self.forbid_network()
        self.forbid_key_read()
        code, out, err = self.run_tool(
            ["--live", "--url", "http://192.0.2.10:8787", "--key-file", self.key_path, "--model", MODEL]
        )
        self.assertEqual(code, 2)
        self.assertIn("nonloopback_plain_http_refused", err)

    def test_url_with_embedded_credentials_refused(self):
        self.forbid_network()
        self.forbid_key_read()
        code, out, err = self.run_tool(
            self.live_args("http://user:pass@127.0.0.1:8787")
        )
        self.assertEqual(code, 2)
        self.assertIn("url_userinfo_not_allowed", err)
        self.assertNotIn("pass", err.replace("url_userinfo_not_allowed", ""))

    def test_bad_timeout_values_refused(self):
        self.forbid_network()
        for value in ("0", "-1", "nan", "inf", "abc"):
            code, out, err = self.run_tool(self.live_args("http://127.0.0.1:9", timeout=value))
            self.assertEqual(code, 2, "timeout=%s" % value)
            self.assertIn("bad_timeout", err)

    def test_unreadable_key_file_refused_before_network(self):
        self.forbid_network()
        code, out, err = self.run_tool(
            ["--live", "--url", "http://127.0.0.1:9", "--key-file",
             os.path.join(self._tmp.name, "nope"), "--model", MODEL]
        )
        self.assertEqual(code, 2)
        self.assertIn("key_file_unreadable", err)

    def test_empty_key_file_refused_before_network(self):
        self.forbid_network()
        empty = os.path.join(self._tmp.name, "empty")
        with open(empty, "w", encoding="utf-8") as handle:
            handle.write("   \n")
        code, out, err = self.run_tool(
            ["--live", "--url", "http://127.0.0.1:9", "--key-file", empty, "--model", MODEL]
        )
        self.assertEqual(code, 2)
        self.assertIn("key_file_empty", err)


class UrlPolicyUnitTests(ToolTestCase):
    def test_loopback_http_allowed(self):
        target = ps.guard_url("http://127.0.0.1:8787/")
        self.assertTrue(target.loopback)
        self.assertEqual(target.port, 8787)
        self.assertEqual(target.base_path, "")

    def test_loopback_ipv6_and_name_allowed(self):
        self.assertTrue(ps.guard_url("http://[::1]:8787").loopback)
        self.assertTrue(ps.guard_url("http://localhost:8787").loopback)

    def test_nonloopback_https_allowed_without_override(self):
        target = ps.guard_url("https://router.example.com:8787")
        self.assertFalse(target.loopback)
        self.assertEqual(target.scheme, "https")
        self.assertEqual(target.port, 8787)

    def test_nonloopback_http_allowed_with_explicit_override(self):
        target = ps.guard_url("http://192.0.2.10:8787", allow_insecure_http=True)
        self.assertFalse(target.loopback)

    def test_unsupported_scheme_refused(self):
        with self.assertRaises(ps.SmokeError) as caught:
            ps.guard_url("ftp://127.0.0.1:8787")
        self.assertEqual(caught.exception.code, "unsupported_scheme")

    def test_query_and_fragment_refused(self):
        for url in ("http://127.0.0.1:8787/?token=1", "http://127.0.0.1:8787/#x"):
            with self.assertRaises(ps.SmokeError):
                ps.guard_url(url)


# ---------------------------------------------------------------------------
# Integration against a fake loopback router
# ---------------------------------------------------------------------------


class LiveHappyPathTests(ToolTestCase):
    def test_nonstream_and_fragmented_stream_pass(self):
        with FakeRouter(chat_responder(nonstream=NONSTREAM_OK, stream_pieces=STREAM_PIECES,
                                       stream_delay=0.005)) as router:
            code, out, err = self.run_tool(
                self.live_args(router.url, connect_timeout=2, read_timeout=2, timeout=15)
            )
            requests = list(router.requests)

        self.assertEqual(code, 0, out + err)
        report = json.loads(out)
        self.assertTrue(report["ok"])
        self.assert_no_secrets(out, err)

        for key in ("tool", "schema", "ok", "live", "model", "max_tokens",
                    "prompt_bytes", "target", "checks", "warnings", "errors"):
            self.assertIn(key, report)
        self.assertEqual(report["model"], MODEL)
        self.assertEqual(report["max_tokens"], 64)
        self.assertLess(report["prompt_bytes"], 128)
        self.assertEqual(report["target"]["host"], "127.0.0.1")
        self.assertTrue(report["target"]["loopback"])
        self.assertEqual(report["errors"], [])

        checks = self.checks_by_name(report)
        nonstream = checks["chat_nonstream"]
        self.assertTrue(nonstream["ok"])
        self.assertEqual(nonstream["status"], 200)
        self.assertEqual(nonstream["usage"], "present")
        self.assertEqual(nonstream["tokens"], {"input": 5, "output": 2, "cached_input": 1, "reasoning": 0})
        self.assertEqual(nonstream["cost"], {"usd": 0.0, "source": "usage.cost"})

        stream = checks["chat_stream"]
        self.assertTrue(stream["ok"], stream)
        self.assertEqual(stream["sse"], {"events": STREAM_EVENTS, "done": True})
        self.assertEqual(stream["usage"], "present")
        self.assertEqual(stream["tokens"]["input"], 5)
        self.assertEqual(stream["tokens"]["output"], 2)
        self.assertEqual(stream["cost"], {"usd": 0.0002, "source": "usage.cost"})

        # Only the fake loopback router is ever contacted, with the bearer key,
        # on the documented endpoints.
        self.assertEqual(len(requests), 2)
        for request in requests:
            self.assertEqual(request["headers"].get("authorization"), "Bearer " + KEY)
            self.assertTrue(request["path"].startswith("/v1/"), request["path"])

        bodies = [json.loads(request["body"]) for request in requests]
        nonstream_body = next(b for b in bodies if not b.get("stream"))
        stream_body = next(b for b in bodies if b.get("stream"))
        self.assertEqual(nonstream_body["max_tokens"], 64)
        self.assertFalse(nonstream_body["stream"])
        self.assertEqual(nonstream_body["messages"], [{"role": "user", "content": ps.SYNTHETIC_PROMPT}])
        self.assertTrue(stream_body["stream_options"]["include_usage"])

    def test_stream_usage_missing_is_reported_not_fabricated(self):
        pieces = [
            b'data: {"choices":[{"delta":{"content":"ok"}}]}\n\n',
            b'data: [DONE]\n\n',
        ]
        with FakeRouter(chat_responder(nonstream=NONSTREAM_OK, stream_pieces=pieces)) as router:
            code, out, err = self.run_tool(self.live_args(router.url, read_timeout=2, timeout=15))
        self.assertEqual(code, 0, out + err)
        report = json.loads(out)
        stream = self.checks_by_name(report)["chat_stream"]
        self.assertTrue(stream["ok"])
        self.assertEqual(stream["usage"], "missing")
        self.assertIsNone(stream["tokens"])
        self.assertIsNone(stream["cost"])
        self.assertTrue(any("chat_stream" in w for w in report["warnings"]))

    def test_nonstream_usage_missing_is_reported_not_fabricated(self):
        obj = {"id": "c", "choices": [{"message": {"content": SECRET_BODY}}]}
        with FakeRouter(chat_responder(nonstream=obj, stream_pieces=STREAM_PIECES)) as router:
            code, out, err = self.run_tool(self.live_args(router.url, read_timeout=2, timeout=15))
        self.assertEqual(code, 0, out + err)
        self.assert_no_secrets(out, err)
        report = json.loads(out)
        nonstream = self.checks_by_name(report)["chat_nonstream"]
        self.assertTrue(nonstream["ok"])
        self.assertEqual(nonstream["usage"], "missing")
        self.assertIsNone(nonstream["tokens"])
        self.assertIsNone(nonstream["cost"])
        self.assertTrue(any("usage missing" in w for w in report["warnings"]))

    def test_unusable_cost_is_ignored_never_fabricated(self):
        obj = json.loads(json.dumps(NONSTREAM_OK))
        obj["usage"]["cost"] = "free-tier"
        with FakeRouter(chat_responder(nonstream=obj, stream_pieces=STREAM_PIECES)) as router:
            code, out, err = self.run_tool(self.live_args(router.url, read_timeout=2, timeout=15))
        self.assertEqual(code, 0, out + err)
        report = json.loads(out)
        nonstream = self.checks_by_name(report)["chat_nonstream"]
        self.assertEqual(nonstream["usage"], "present")
        self.assertEqual(nonstream["tokens"]["input"], 5)
        self.assertIsNone(nonstream["cost"])
        self.assertTrue(any("cost" in w for w in report["warnings"]))

    def test_negative_cost_is_ignored(self):
        obj = json.loads(json.dumps(NONSTREAM_OK))
        obj["usage"]["cost"] = -1
        with FakeRouter(chat_responder(nonstream=obj, stream_pieces=STREAM_PIECES)) as router:
            code, out, err = self.run_tool(self.live_args(router.url, read_timeout=2, timeout=15))
        report = json.loads(out)
        nonstream = self.checks_by_name(report)["chat_nonstream"]
        self.assertIsNone(nonstream["cost"])

    def test_check_models_optional_and_routed(self):
        with FakeRouter(chat_responder(nonstream=NONSTREAM_OK, stream_pieces=STREAM_PIECES,
                                       models=[MODEL, "other/model"])) as router:
            code, out, err = self.run_tool(
                self.live_args(router.url, read_timeout=2, timeout=15) + ["--check-models"]
            )
            request_count = len(router.requests)
        self.assertEqual(code, 0, out + err)
        report = json.loads(out)
        self.assertEqual(request_count, 3)
        models = self.checks_by_name(report)["models"]
        self.assertTrue(models["ok"])
        self.assertTrue(models["model_listed"])
        self.assertEqual(models["models_count"], 2)
        # The model list itself is not echoed.
        self.assertNotIn("other/model", out)

    def test_check_models_not_routed_fails(self):
        with FakeRouter(chat_responder(nonstream=NONSTREAM_OK, stream_pieces=STREAM_PIECES,
                                       models=["other/model"])) as router:
            code, out, err = self.run_tool(
                self.live_args(router.url, read_timeout=2, timeout=15) + ["--check-models"]
            )
        self.assertEqual(code, 1)
        report = json.loads(out)
        models = self.checks_by_name(report)["models"]
        self.assertFalse(models["ok"])
        self.assertEqual(models["error"], "model_not_routed")

    def test_pretty_flag_still_emits_machine_readable_json(self):
        with FakeRouter(chat_responder(nonstream=NONSTREAM_OK, stream_pieces=STREAM_PIECES)) as router:
            code, out, err = self.run_tool(
                self.live_args(router.url, read_timeout=2, timeout=15) + ["--pretty"]
            )
        self.assertEqual(code, 0)
        self.assertIn("\n", out.strip())
        self.assertTrue(json.loads(out)["ok"])


# ---------------------------------------------------------------------------
# Failure modes
# ---------------------------------------------------------------------------


class FailureModeTests(ToolTestCase):
    def test_nonstream_malformed_json_fails(self):
        responder = lambda handler, requests: raw_response(handler, b"{not json")
        with FakeRouter(responder) as router:
            code, out, err = self.run_tool(self.live_args(router.url, read_timeout=2, timeout=15))
        self.assertEqual(code, 1)
        report = json.loads(out)
        self.assertFalse(report["ok"])
        self.assertEqual(self.checks_by_name(report)["chat_nonstream"]["error"], "malformed_json")

    def test_http_error_does_not_leak_key_or_body(self):
        def responder(handler, requests):
            json_response(
                handler,
                {"error": {"message": "leak " + KEY + " " + SECRET_BODY, "type": "invalid_request_error"}},
                status=401,
                extra_headers={"WWW-Authenticate": 'Bearer realm="localrouter"'},
            )

        with FakeRouter(responder) as router:
            code, out, err = self.run_tool(self.live_args(router.url, read_timeout=2, timeout=15))
        self.assertEqual(code, 1)
        self.assert_no_secrets(out, err)
        report = json.loads(out)
        for name in ("chat_nonstream", "chat_stream"):
            check = self.checks_by_name(report)[name]
            self.assertFalse(check["ok"])
            self.assertEqual(check["error"], "http_status")
            # Reviewed blocker: provider-controlled error.type/detail must NOT
            # be surfaced, even when it looks like a token; omit it entirely.
            self.assertIsNone(check["detail"])

    def test_stream_missing_done_fails(self):
        pieces = [
            b'data: {"choices":[{"delta":{"content":"x"}}]}\n\n',
            b'data: {"choices":[{"delta":{"content":"y"}}]}\n\n',
        ]
        with FakeRouter(chat_responder(nonstream=NONSTREAM_OK, stream_pieces=pieces)) as router:
            code, out, err = self.run_tool(self.live_args(router.url, read_timeout=2, timeout=15))
        self.assertEqual(code, 1)
        report = json.loads(out)
        stream = self.checks_by_name(report)["chat_stream"]
        self.assertEqual(stream["error"], "missing_done")
        self.assertFalse(stream["sse"]["done"])
        self.assertIn("chat_stream: missing_done", report["errors"])

    def test_stream_malformed_event_fails(self):
        pieces = [
            b'data: {"choices":[{"delta":{"content":"ok"}}]}\n\n',
            b'data: {not valid json}\n\n',
        ]
        with FakeRouter(chat_responder(nonstream=NONSTREAM_OK, stream_pieces=pieces)) as router:
            code, out, err = self.run_tool(self.live_args(router.url, read_timeout=2, timeout=15))
        self.assertEqual(code, 1)
        report = json.loads(out)
        self.assertEqual(self.checks_by_name(report)["chat_stream"]["error"], "malformed_sse")

    def test_stream_requires_event_stream_content_type(self):
        responder = lambda handler, requests: json_response(
            handler, {"choices": [{"message": {"content": SECRET_BODY}}],
                      "usage": {"prompt_tokens": 1, "completion_tokens": 1}}
        )
        with FakeRouter(responder) as router:
            code, out, err = self.run_tool(self.live_args(router.url, read_timeout=2, timeout=15))
        self.assertEqual(code, 1)
        self.assert_no_secrets(out, err)
        report = json.loads(out)
        self.assertEqual(self.checks_by_name(report)["chat_stream"]["error"], "not_event_stream")

    def test_stalled_sse_hits_read_timeout(self):
        pieces = [b'data: {"choices":[{"delta":{"content":"x"}}]}\n\n']

        def responder(handler, requests):
            sse_stall_response(handler, pieces, declared_len=4096, stall=1.5)

        with FakeRouter(responder) as router:
            code, out, err = self.run_tool(
                self.live_args(router.url, read_timeout=0.4, timeout=5)
            )
        self.assertEqual(code, 1)
        report = json.loads(out)
        stream = self.checks_by_name(report)["chat_stream"]
        self.assertIn(stream["error"], ("read_timeout", "overall_timeout"))

    def test_redirect_is_reported_and_never_followed(self):
        def responder(handler, requests):
            redirect_response(handler, "http://198.51.100.9/steal?k=" + KEY, status=302)

        with FakeRouter(responder) as router:
            code, out, err = self.run_tool(self.live_args(router.url, read_timeout=2, timeout=15))
            request_count = len(router.requests)
        self.assertEqual(code, 1)
        self.assert_no_secrets(out, err)
        report = json.loads(out)
        for name in ("chat_nonstream", "chat_stream"):
            self.assertEqual(self.checks_by_name(report)[name]["error"], "unexpected_redirect")
        # One request per check, and nothing was ever sent to the redirect target.
        self.assertEqual(request_count, 2)
        self.assertNotIn("198.51.100.9", out + err)
        self.assertNotIn("Location", out + err)

    def test_oversized_response_is_capped(self):
        big = {"choices": [{"message": {"content": SECRET_BODY}}], "padding": "x" * 4000}
        self.patch("MAX_RESPONSE_BYTES", 512)
        with FakeRouter(chat_responder(nonstream=big, stream_pieces=STREAM_PIECES)) as router:
            code, out, err = self.run_tool(self.live_args(router.url, read_timeout=2, timeout=15))
        self.assertEqual(code, 1)
        self.assert_no_secrets(out, err)
        report = json.loads(out)
        nonstream = self.checks_by_name(report)["chat_nonstream"]
        self.assertEqual(nonstream["error"], "response_too_large")
        self.assertTrue(nonstream["truncated"])
        self.assertLessEqual(nonstream["bytes_read"], 512)

    def test_connect_failure_is_reported(self):
        # Port 9 (discard) on loopback: nothing listens there.
        code, out, err = self.run_tool(
            self.live_args("http://127.0.0.1:9", connect_timeout=0.5, read_timeout=0.5, timeout=5)
        )
        self.assertEqual(code, 1)
        report = json.loads(out)
        errors = " ".join(report["errors"])
        self.assertTrue(
            any(token in errors for token in ("connect_failed", "connect_timeout", "read_timeout")),
            errors,
        )
        self.assert_no_secrets(out, err)


# ---------------------------------------------------------------------------
# Responses API fixtures (codex-shaped; no chat [DONE] terminator)
# ---------------------------------------------------------------------------

RESPONSES_NONSTREAM_OK = {
    "id": "resp_1",
    "object": "response",
    "status": "completed",
    "model": MODEL,
    "output": [
        {"type": "message", "role": "assistant",
         "content": [{"type": "output_text", "text": SECRET_BODY}]}
    ],
    "usage": {
        "input_tokens": 5,
        "output_tokens": 2,
        "input_tokens_details": {"cached_tokens": 1},
        "output_tokens_details": {"reasoning_tokens": 0},
        "cost": 0.0003,
    },
}

RESPONSES_STREAM_EVENTS = 5
RESPONSES_STREAM_PIECES = [
    b'event: response.created\ndata: {"type":"response.created","response":{"id":"resp_1","status":"in_progress"}}\n\n',
    b'event: response.output_text.delta\ndata: {"type":"response.output_text.delta","delta":"o"}\n\n',
    b'event: response.output_text.delta\ndata: {"type":"response.output_text.delta","delta":"k"}\n\n',
    b'event: response.output_text.done\ndata: {"type":"response.output_text.done","text":"ok"}\n\n',
    b'event: response.completed\ndata: {"type":"response.completed","response":{"id":"resp_1",'
    b'"status":"completed","usage":{"input_tokens":5,"output_tokens":2,"cost":0.0003}}}\n\n',
]


def responses_responder(nonstream=None, stream_pieces=None, stream_delay=0.0):
    """Dispatch /v1/responses by the request's ``stream`` flag."""

    def responder(handler, requests):
        if handler.path.endswith("/v1/models"):
            json_response(handler, {"object": "list", "data": []})
            return
        if handler.path.endswith("/v1/responses"):
            try:
                body = json.loads(requests[-1]["body"] or b"{}")
            except ValueError:
                body = {}
            if body.get("stream"):
                sse_response(handler, stream_pieces or [], delay=stream_delay)
            else:
                json_response(handler, nonstream if nonstream is not None else {})
            return
        handler.send_response(404)
        handler.send_header("Content-Length", "0")
        handler.end_headers()

    return responder


# A credential-shaped provider error type, assembled at runtime so that no
# literal secret-shaped token lives in source while the value under test still
# looks exactly like the thing a regex must not echo.
PROVIDER_TYPE_TOKEN = "sk-" + "proj-" + "LEAKTOKEN1234567890"


# ---------------------------------------------------------------------------
# Reviewed live-smoke blockers: generation proof, error hygiene, hostile
# inputs, hard wall-clock budget, exact-cap handling, Responses protocol.
# ---------------------------------------------------------------------------


class GenerationProofTests(ToolTestCase):
    """A 200 must carry genuine, nonempty generated text -- not a token count."""

    def _nonstream_error(self, nonstream, stream_pieces=STREAM_PIECES):
        with FakeRouter(chat_responder(nonstream=nonstream, stream_pieces=stream_pieces)) as router:
            code, out, err = self.run_tool(self.live_args(router.url, read_timeout=2, timeout=15))
        return code, json.loads(out), err

    def test_empty_choices_is_no_generation(self):
        code, report, _ = self._nonstream_error(
            {"id": "c", "object": "chat.completion", "choices": [],
             "usage": {"prompt_tokens": 5, "completion_tokens": 0}}
        )
        self.assertEqual(code, 1)
        check = self.checks_by_name(report)["chat_nonstream"]
        self.assertFalse(check["ok"])
        self.assertEqual(check["error"], "no_generation")

    def test_string_choices_is_no_generation(self):
        code, report, _ = self._nonstream_error(
            {"id": "c", "choices": "ok", "usage": {"prompt_tokens": 5, "completion_tokens": 1}}
        )
        self.assertEqual(code, 1)
        self.assertEqual(self.checks_by_name(report)["chat_nonstream"]["error"], "no_generation")

    def test_usage_only_is_no_generation(self):
        code, report, _ = self._nonstream_error(
            {"id": "c", "usage": {"prompt_tokens": 5, "completion_tokens": 1}}
        )
        self.assertEqual(code, 1)
        self.assertEqual(self.checks_by_name(report)["chat_nonstream"]["error"], "no_generation")

    def test_blank_content_is_no_generation(self):
        code, report, _ = self._nonstream_error(
            {"id": "c", "choices": [{"message": {"role": "assistant", "content": "   "}}],
             "usage": {"prompt_tokens": 5, "completion_tokens": 0}}
        )
        self.assertEqual(code, 1)
        self.assertEqual(self.checks_by_name(report)["chat_nonstream"]["error"], "no_generation")

    def test_done_only_stream_is_no_generation(self):
        with FakeRouter(chat_responder(nonstream=NONSTREAM_OK,
                                       stream_pieces=[b"data: [DONE]\n\n"])) as router:
            code, out, err = self.run_tool(self.live_args(router.url, read_timeout=2, timeout=15))
        self.assertEqual(code, 1)
        report = json.loads(out)
        stream = self.checks_by_name(report)["chat_stream"]
        self.assertFalse(stream["ok"])
        self.assertEqual(stream["error"], "no_generation")
        self.assertTrue(stream["sse"]["done"])

    def test_whitespace_only_chat_stream_is_no_generation(self):
        pieces = [
            b'data: {"choices":[{"delta":{"content":" "}}]}\n\n',
            b'data: {"choices":[{"delta":{"content":"\\n\\t"}}]}\n\n',
            b'data: [DONE]\n\n',
        ]
        with FakeRouter(chat_responder(nonstream=NONSTREAM_OK, stream_pieces=pieces)) as router:
            code, out, err = self.run_tool(self.live_args(router.url, read_timeout=2, timeout=15))
        self.assertEqual(code, 1, out + err)
        stream = self.checks_by_name(json.loads(out))["chat_stream"]
        self.assertFalse(stream["ok"])
        self.assertEqual(stream["error"], "no_generation")

    def test_whitespace_only_responses_stream_is_no_generation(self):
        cases = {
            "deltas": [
                b'event: response.output_text.delta\ndata: {"type":"response.output_text.delta","delta":" "}\n\n',
                b'event: response.completed\ndata: {"type":"response.completed","response":'
                b'{"status":"completed","usage":{"input_tokens":5,"output_tokens":1}}}\n\n',
            ],
            "completed_output": [
                b'event: response.completed\ndata: {"type":"response.completed","response":'
                b'{"status":"completed","output":[{"type":"message","content":'
                b'[{"type":"output_text","text":"  \\n"}]}]}}\n\n',
            ],
        }
        for name, pieces in cases.items():
            with FakeRouter(responses_responder(nonstream=RESPONSES_NONSTREAM_OK,
                                                 stream_pieces=pieces)) as router:
                code, out, err = self.run_tool(
                    self.live_args(router.url, read_timeout=2, timeout=15) + ["--protocol", "responses"]
                )
            self.assertEqual(code, 1, name)
            stream = self.checks_by_name(json.loads(out))["responses_stream"]
            self.assertEqual(stream["error"], "no_generation", name)

    def test_genuine_generation_reports_char_count(self):
        with FakeRouter(chat_responder(nonstream=NONSTREAM_OK, stream_pieces=STREAM_PIECES)) as router:
            code, out, err = self.run_tool(self.live_args(router.url, read_timeout=2, timeout=15))
        self.assertEqual(code, 0, out + err)
        report = json.loads(out)
        checks = self.checks_by_name(report)
        self.assertGreater(checks["chat_nonstream"]["generated_chars"], 0)
        self.assertGreater(checks["chat_stream"]["generated_chars"], 0)


class ErrorHygieneTests(ToolTestCase):
    def test_provider_error_type_never_echoed(self):
        # A plausible-looking provider type token must be omitted, not regexed
        # back out: it may itself be (part of) a credential.
        def responder(handler, requests):
            json_response(
                handler,
                {"error": {"message": "leak " + KEY, "type": PROVIDER_TYPE_TOKEN}},
                status=401,
            )

        with FakeRouter(responder) as router:
            code, out, err = self.run_tool(self.live_args(router.url, read_timeout=2, timeout=15))
        self.assertEqual(code, 1)
        self.assert_no_secrets(out, err)
        self.assertNotIn("sk-" + "proj", out + err)
        report = json.loads(out)
        for name in ("chat_nonstream", "chat_stream"):
            check = self.checks_by_name(report)[name]
            self.assertFalse(check["ok"])
            self.assertEqual(check["error"], "http_status")
            self.assertIsNone(check["detail"])

    def test_nonstream_error_object_omits_detail(self):
        obj = {"error": {"type": PROVIDER_TYPE_TOKEN, "message": KEY}}
        with FakeRouter(chat_responder(nonstream=obj, stream_pieces=STREAM_PIECES)) as router:
            code, out, err = self.run_tool(self.live_args(router.url, read_timeout=2, timeout=15))
        self.assertEqual(code, 1)
        self.assert_no_secrets(out, err)
        self.assertNotIn("sk-" + "proj", out)
        check = self.checks_by_name(json.loads(out))["chat_nonstream"]
        self.assertEqual(check["error"], "error_object")
        self.assertIsNone(check["detail"])

    def test_http_503_event_stream_body_does_not_crash(self):
        def responder(handler, requests):
            raw_response(handler, b'{"error":{"type":"upstream_unavailable"}}',
                         status=503, content_type="text/event-stream")

        with FakeRouter(responder) as router:
            code, out, err = self.run_tool(self.live_args(router.url, read_timeout=2, timeout=15))
        self.assertEqual(code, 1, out + err)
        report = json.loads(out)
        for name in ("chat_nonstream", "chat_stream"):
            check = self.checks_by_name(report)[name]
            self.assertFalse(check["ok"])
            self.assertEqual(check["error"], "http_status")
            self.assertIsNone(check["detail"])

    def test_http_error_event_stream_keeps_http_status(self):
        # A non-200 body is never parsed as SSE: a malformed, oversized or
        # overlong error stream must still report http_status and the status.
        bodies = {
            "malformed_data": b'data: {not json ' + KEY.encode() + b'}\n\n',
            "oversize_line": b"data: " + b"x" * 200 + b"\n\n",
            "too_many_events": b'data: {"choices":[]}\n\n' * 8,
        }
        self.patch("MAX_EVENT_BYTES", 64)
        self.patch("MAX_EVENTS", 4)
        for name, raw in bodies.items():
            def responder(handler, requests, raw=raw):
                raw_response(handler, raw, status=401, content_type="text/event-stream")

            with FakeRouter(responder) as router:
                code, out, err = self.run_tool(self.live_args(router.url, read_timeout=2, timeout=15))
            self.assertEqual(code, 1, name)
            self.assert_no_secrets(out, err)
            check = self.checks_by_name(json.loads(out))["chat_stream"]
            self.assertEqual(check["error"], "http_status", name)
            self.assertEqual(check["status"], 401, name)
            self.assertIsNone(check["detail"], name)

    def test_stream_error_object_rejected_even_with_done(self):
        pieces = [
            b'data: {"error":{"type":"upstream_error","message":"boom"}}\n\n',
            b'data: [DONE]\n\n',
        ]
        with FakeRouter(chat_responder(nonstream=NONSTREAM_OK, stream_pieces=pieces)) as router:
            code, out, err = self.run_tool(self.live_args(router.url, read_timeout=2, timeout=15))
        self.assertEqual(code, 1)
        self.assert_no_secrets(out, err)
        self.assertEqual(self.checks_by_name(json.loads(out))["chat_stream"]["error"], "stream_error")


class HostileInputTests(ToolTestCase):
    def test_non_utf8_key_file_rejected_before_network(self):
        self.forbid_network()
        bad = os.path.join(self._tmp.name, "badkey")
        with open(bad, "wb") as handle:
            handle.write(b"\xff\xfe\x00bad\xc3\x28not-utf8\n")
        code, out, err = self.run_tool(
            ["--live", "--url", "http://127.0.0.1:9", "--key-file", bad, "--model", MODEL]
        )
        self.assertEqual(code, 2)
        self.assertIn("key_file_invalid", err)
        self.assert_no_secrets(out, err)

    def test_non_ascii_key_rejected_before_network_without_echo(self):
        # Valid UTF-8 that http.client cannot put in a header (or that is not a
        # bearer token at all) must be refused before any connection, and the
        # offending character must never be echoed.
        for name, key in (("euro", "abc\u20acdef"), ("nbsp", "abc\u00a0def"),
                          ("space", "abc def"), ("del", "abc\x7fdef")):
            self.forbid_network()
            path = os.path.join(self._tmp.name, "key-" + name)
            with open(path, "w", encoding="utf-8") as handle:
                handle.write(key + "\n")
            code, out, err = self.run_tool(
                ["--live", "--url", "http://127.0.0.1:9", "--key-file", path, "--model", MODEL]
            )
            self.assertEqual(code, 2, name)
            self.assertIn("key_file_invalid", err, name)
            self.assertNotIn("Traceback", err, name)
            self.assertNotIn(key, out + err, name)
            self.assertNotIn("\u20ac", out + err, name)

    def test_non_ascii_url_path_rejected_before_network(self):
        for url in ("http://127.0.0.1:8787/r\u00e9", "http://127.0.0.1:8787/a b"):
            with self.assertRaises(ps.SmokeError) as caught:
                ps.guard_url(url)
            self.assertEqual(caught.exception.code, "invalid_url", url)
            self.forbid_network()
            self.forbid_key_read()
            code, out, err = self.run_tool(self.live_args(url))
            self.assertEqual(code, 2, url)
            self.assertIn("invalid_url", err)

    def test_malformed_ipv6_url_is_a_sanitized_error(self):
        for url in ("http://[::1:8787", "http://[::1", "https://[fe80::1:8787"):
            with self.assertRaises(ps.SmokeError) as caught:
                ps.guard_url(url)
            self.assertEqual(caught.exception.code, "invalid_url", url)
            self.forbid_network()
            code, out, err = self.run_tool(self.live_args(url))
            self.assertEqual(code, 2, url)
            self.assertIn("invalid_url", err)


class WallClockBudgetTests(ToolTestCase):
    def test_header_stall_respects_overall_budget(self):
        # Server accepts, then never sends a status line. A per-request connect
        # timeout must not let the run exceed the overall budget (was ~4s/2req).
        def responder(handler, requests):
            time.sleep(1.5)

        with FakeRouter(responder) as router:
            start = time.monotonic()
            code, out, err = self.run_tool(
                self.live_args(router.url, connect_timeout=2, read_timeout=2, timeout=0.5)
            )
            elapsed = time.monotonic() - start
        self.assertEqual(code, 1)
        self.assertLess(elapsed, 1.25, "overall budget not enforced: %.2fs" % elapsed)
        report = json.loads(out)
        for name in ("chat_nonstream", "chat_stream"):
            check = self.checks_by_name(report)[name]
            self.assertFalse(check["ok"])
            self.assertIn(check["error"], ("overall_timeout", "read_timeout"))


class ConnectTimeoutTests(ToolTestCase):
    def test_tcp_connect_timeout_is_connect_timeout(self):
        class StallingConnection:
            sock = None

            def connect(self):
                raise socket.timeout("timed out")

            def close(self):
                pass

        self.patch("_open_connection", lambda *args, **kwargs: StallingConnection())
        code, out, err = self.run_tool(
            self.live_args("http://127.0.0.1:9", connect_timeout=1, read_timeout=1, timeout=10)
        )
        self.assertEqual(code, 1)
        report = json.loads(out)
        for name in ("chat_nonstream", "chat_stream"):
            check = self.checks_by_name(report)[name]
            self.assertEqual(check["error"], "connect_timeout", name)
            self.assertIsNone(check["status"])

    def test_tls_handshake_stall_is_connect_timeout(self):
        # The kernel completes the TCP handshake from the listen backlog, but
        # nothing ever answers the ClientHello: the TLS handshake stalls inside
        # the connect phase. Loopback only; no certificate is involved.
        listener = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        self.addCleanup(listener.close)
        listener.bind(("127.0.0.1", 0))
        listener.listen(8)
        port = listener.getsockname()[1]
        start = time.monotonic()
        code, out, err = self.run_tool(
            self.live_args("https://127.0.0.1:%d" % port,
                           connect_timeout=0.3, read_timeout=2, timeout=10)
        )
        elapsed = time.monotonic() - start
        self.assertEqual(code, 1)
        self.assertLess(elapsed, 5)
        self.assert_no_secrets(out, err)
        report = json.loads(out)
        for name in ("chat_nonstream", "chat_stream"):
            self.assertEqual(self.checks_by_name(report)[name]["error"], "connect_timeout", name)


class CafileTests(ToolTestCase):
    def test_bad_cafile_fails_each_check_with_exit_1(self):
        # The CA bundle is loaded per check, before any connection: a missing
        # or invalid bundle is a per-check failure (exit 1), not an exit-2
        # pre-request error. Documented that way in PROVIDER-ACCEPTANCE.md.
        bogus = os.path.join(self._tmp.name, "not-a-ca.pem")
        with open(bogus, "w", encoding="ascii") as handle:
            handle.write("-----BEGIN CERTIFICATE-----\nnot base64\n-----END CERTIFICATE-----\n")
        cases = (
            (os.path.join(self._tmp.name, "missing.pem"), "cafile_unreadable"),
            (bogus, "tls_config_error"),
        )
        for cafile, want in cases:
            code, out, err = self.run_tool(
                self.live_args("https://127.0.0.1:9", cafile=cafile, connect_timeout=1,
                               read_timeout=1, timeout=10)
            )
            self.assertEqual(code, 1, want)
            report = json.loads(out)
            for name in ("chat_nonstream", "chat_stream"):
                self.assertEqual(self.checks_by_name(report)[name]["error"], want, name)
            self.assert_no_secrets(out, err)


class ResponseCapTests(ToolTestCase):
    def test_exact_cap_body_is_not_oversized(self):
        obj = {"choices": [{"message": {"role": "assistant", "content": SECRET_BODY}}],
               "usage": {"prompt_tokens": 5, "completion_tokens": 2}}
        raw = json.dumps(obj).encode("utf-8")
        self.patch("MAX_RESPONSE_BYTES", len(raw))
        with FakeRouter(chat_responder(nonstream=obj, stream_pieces=STREAM_PIECES)) as router:
            code, out, err = self.run_tool(self.live_args(router.url, read_timeout=2, timeout=15))
        check = self.checks_by_name(json.loads(out))["chat_nonstream"]
        self.assertTrue(check["ok"], check)
        self.assertFalse(check["truncated"])
        self.assertEqual(check["bytes_read"], len(raw))

    def test_cap_plus_one_byte_is_detected_and_bounded(self):
        big = {"choices": [{"message": {"content": SECRET_BODY}}], "padding": "x" * 4000}
        self.patch("MAX_RESPONSE_BYTES", 512)
        with FakeRouter(chat_responder(nonstream=big, stream_pieces=STREAM_PIECES)) as router:
            code, out, err = self.run_tool(self.live_args(router.url, read_timeout=2, timeout=15))
        check = self.checks_by_name(json.loads(out))["chat_nonstream"]
        self.assertEqual(check["error"], "response_too_large")
        self.assertTrue(check["truncated"])
        # One extra byte past the cap is read to detect overflow; never more.
        self.assertLessEqual(check["bytes_read"], 512 + 1)


class ResponsesProtocolTests(ToolTestCase):
    def test_chat_is_the_default_protocol(self):
        with FakeRouter(chat_responder(nonstream=NONSTREAM_OK, stream_pieces=STREAM_PIECES)) as router:
            code, out, err = self.run_tool(self.live_args(router.url, read_timeout=2, timeout=15))
            paths = [request["path"] for request in router.requests]
        self.assertEqual(code, 0, out + err)
        report = json.loads(out)
        self.assertEqual(report["protocol"], "chat")
        self.assertTrue(all(p.endswith("/v1/chat/completions") for p in paths), paths)

    def test_unknown_protocol_argument_is_refused(self):
        code, out, err = self.run_tool(
            self.live_args("http://127.0.0.1:9") + ["--protocol", "bogus"]
        )
        self.assertEqual(code, 2)
        self.assertIn("invalid choice", err.lower())

    def test_responses_nonstream_and_stream_pass(self):
        with FakeRouter(responses_responder(nonstream=RESPONSES_NONSTREAM_OK,
                                             stream_pieces=RESPONSES_STREAM_PIECES,
                                             stream_delay=0.005)) as router:
            code, out, err = self.run_tool(
                self.live_args(router.url, read_timeout=2, timeout=15) + ["--protocol", "responses"]
            )
            requests = list(router.requests)
        self.assertEqual(code, 0, out + err)
        self.assert_no_secrets(out, err)
        report = json.loads(out)
        self.assertTrue(report["ok"], report["errors"])
        self.assertEqual(report["protocol"], "responses")
        checks = self.checks_by_name(report)
        nonstream = checks["responses_nonstream"]
        self.assertTrue(nonstream["ok"], nonstream)
        self.assertEqual(nonstream["tokens"],
                         {"input": 5, "output": 2, "cached_input": 1, "reasoning": 0})
        self.assertEqual(nonstream["cost"], {"usd": 0.0003, "source": "usage.cost"})
        self.assertGreater(nonstream["generated_chars"], 0)
        stream = checks["responses_stream"]
        self.assertTrue(stream["ok"], stream)
        self.assertTrue(stream["sse"]["completed"])
        self.assertEqual(stream["sse"]["events"], RESPONSES_STREAM_EVENTS)
        self.assertEqual(stream["tokens"]["input"], 5)
        self.assertEqual(stream["cost"], {"usd": 0.0003, "source": "usage.cost"})

        self.assertEqual(len(requests), 2)
        for request in requests:
            self.assertTrue(request["path"].endswith("/v1/responses"), request["path"])
            self.assertEqual(request["headers"].get("authorization"), "Bearer " + KEY)
        bodies = [json.loads(request["body"]) for request in requests]
        nonstream_body = next(b for b in bodies if not b.get("stream"))
        stream_body = next(b for b in bodies if b.get("stream"))
        self.assertEqual(nonstream_body["max_output_tokens"], 64)
        self.assertNotIn("max_tokens", nonstream_body)
        self.assertFalse(nonstream_body["store"])
        self.assertEqual(nonstream_body["input"][0]["role"], "user")
        self.assertEqual(nonstream_body["input"][0]["content"][0]["type"], "input_text")
        self.assertNotIn("stream_options", stream_body)
        self.assertTrue(stream_body["stream"])

    def test_responses_stream_does_not_require_chat_done(self):
        with FakeRouter(responses_responder(nonstream=RESPONSES_NONSTREAM_OK,
                                             stream_pieces=RESPONSES_STREAM_PIECES)) as router:
            code, out, err = self.run_tool(
                self.live_args(router.url, read_timeout=2, timeout=15) + ["--protocol", "responses"]
            )
        self.assertEqual(code, 0, out + err)
        stream = self.checks_by_name(json.loads(out))["responses_stream"]
        self.assertTrue(stream["ok"])
        self.assertNotIn("data: [DONE]", "".join(p.decode() for p in RESPONSES_STREAM_PIECES))

    def test_responses_stream_missing_terminal_fails(self):
        pieces = [b'event: response.output_text.delta\n'
                  b'data: {"type":"response.output_text.delta","delta":"ok"}\n\n']
        with FakeRouter(responses_responder(nonstream=RESPONSES_NONSTREAM_OK,
                                             stream_pieces=pieces)) as router:
            code, out, err = self.run_tool(
                self.live_args(router.url, read_timeout=2, timeout=15) + ["--protocol", "responses"]
            )
        self.assertEqual(code, 1)
        self.assertEqual(self.checks_by_name(json.loads(out))["responses_stream"]["error"],
                         "missing_terminal")

    def test_responses_stream_failed_event_fails(self):
        pieces = [
            b'event: response.output_text.delta\ndata: {"type":"response.output_text.delta","delta":"ok"}\n\n',
            b'event: response.failed\ndata: {"type":"response.failed","response":{"status":"failed"}}\n\n',
        ]
        with FakeRouter(responses_responder(nonstream=RESPONSES_NONSTREAM_OK,
                                             stream_pieces=pieces)) as router:
            code, out, err = self.run_tool(
                self.live_args(router.url, read_timeout=2, timeout=15) + ["--protocol", "responses"]
            )
        self.assertEqual(code, 1)
        self.assertEqual(self.checks_by_name(json.loads(out))["responses_stream"]["error"],
                         "response_failed")

    def test_responses_stream_incomplete_event_fails(self):
        pieces = [
            b'event: response.output_text.delta\ndata: {"type":"response.output_text.delta","delta":"ok"}\n\n',
            b'event: response.incomplete\ndata: {"type":"response.incomplete","response":{"status":"incomplete"}}\n\n',
        ]
        with FakeRouter(responses_responder(nonstream=RESPONSES_NONSTREAM_OK,
                                             stream_pieces=pieces)) as router:
            code, out, err = self.run_tool(
                self.live_args(router.url, read_timeout=2, timeout=15) + ["--protocol", "responses"]
            )
        self.assertEqual(code, 1)
        self.assertEqual(self.checks_by_name(json.loads(out))["responses_stream"]["error"],
                         "response_incomplete")

    def test_responses_stream_requires_output_text(self):
        pieces = [b'event: response.completed\ndata: {"type":"response.completed","response":'
                  b'{"status":"completed","usage":{"input_tokens":5,"output_tokens":0}}}\n\n']
        with FakeRouter(responses_responder(nonstream=RESPONSES_NONSTREAM_OK,
                                             stream_pieces=pieces)) as router:
            code, out, err = self.run_tool(
                self.live_args(router.url, read_timeout=2, timeout=15) + ["--protocol", "responses"]
            )
        self.assertEqual(code, 1)
        self.assertEqual(self.checks_by_name(json.loads(out))["responses_stream"]["error"],
                         "no_generation")

    def test_responses_stream_error_event_rejected(self):
        pieces = [b'event: error\ndata: {"type":"error","error":{"type":"server_error"}}\n\n']
        with FakeRouter(responses_responder(nonstream=RESPONSES_NONSTREAM_OK,
                                             stream_pieces=pieces)) as router:
            code, out, err = self.run_tool(
                self.live_args(router.url, read_timeout=2, timeout=15) + ["--protocol", "responses"]
            )
        self.assertEqual(code, 1)
        self.assertEqual(self.checks_by_name(json.loads(out))["responses_stream"]["error"],
                         "stream_error")

    def test_responses_nonstream_failed_status_fails(self):
        obj = {"id": "resp_1", "object": "response", "status": "failed", "output": []}
        with FakeRouter(responses_responder(nonstream=obj,
                                             stream_pieces=RESPONSES_STREAM_PIECES)) as router:
            code, out, err = self.run_tool(
                self.live_args(router.url, read_timeout=2, timeout=15) + ["--protocol", "responses"]
            )
        self.assertEqual(code, 1)
        self.assertEqual(self.checks_by_name(json.loads(out))["responses_nonstream"]["error"],
                         "response_failed")

    def test_responses_nonstream_error_object_omits_detail(self):
        obj = {"error": {"type": PROVIDER_TYPE_TOKEN, "message": KEY}}
        with FakeRouter(responses_responder(nonstream=obj,
                                             stream_pieces=RESPONSES_STREAM_PIECES)) as router:
            code, out, err = self.run_tool(
                self.live_args(router.url, read_timeout=2, timeout=15) + ["--protocol", "responses"]
            )
        self.assert_no_secrets(out, err)
        self.assertNotIn("sk-" + "proj", out + err)
        check = self.checks_by_name(json.loads(out))["responses_nonstream"]
        self.assertEqual(check["error"], "error_object")
        self.assertIsNone(check["detail"])


if __name__ == "__main__":
    unittest.main(verbosity=2)
