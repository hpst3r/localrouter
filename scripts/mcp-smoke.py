#!/usr/bin/env python3
"""mcp-smoke - bounded stdio acceptance smoke test for the `localrouter mcp` server.

Purpose
-------
Drive a *provided* LocalRouter MCP stdio server binary (`<binary> mcp --url URL
[--key-file PATH]`) through a single spec-shaped MCP session and check the
contract the MCP layer must offer on top of the control API:

* initialize negotiates a protocol version the server actually supports;
* notifications/initialized is accepted;
* tools/list advertises exactly three tools with object input schemas:
  ``quota_status``, ``usage_summary``, ``check_admission``;
* each tool returns a *structured* successful result (``structuredContent`` is
  a JSON object, ``isError`` unset/false);
* ``usage_summary`` honours its ``limit`` (rows are bounded, not dumped whole);
* ``check_admission`` answers a definite decision — a *deny* is a normal,
  successful outcome for a dry run, never a failure;
* invalid / unknown arguments yield an error signal (a JSON-RPC error, or a
  tool result flagged ``isError``);
* every stdout line is valid JSON-RPC (no log chatter on stdout).

Safety / scope
--------------
* Only the stdio MCP server is spoken to, and only the MCP methods above; the
  tool *arguments* map to the control API's read-only surface (GET status, GET
  usage, POST /control/v1/admit dry-run). Nothing is written, ingested,
  reserved, or reconfigured.
* ``--key-file`` is passed *through* to the server as a path; its contents are
  never read, stored or printed. No key material can reach stdout: only
  sanitized outcome metadata is emitted, and server stderr is never echoed to
  stdout.
* If the binary does not implement the ``mcp`` subcommand (it exits / closes
  stdout before answering ``initialize``) the run FAILS with
  ``server_unavailable`` — there is no fabricated success path.

Portability
-----------
Runs on POSIX and Windows from the same source. stdout is drained by a
background thread into a queue (no ``select`` over a pipe, which Windows does
not support for pipes); teardown signals the process group via ``os.killpg``
on POSIX and falls back to ``Popen.terminate``/``Popen.kill`` elsewhere, so
``signal.SIGKILL`` is never touched on a platform that lacks it. Nothing in
the report depends on a shell, a shebang, or an executable bit.

Exit codes
----------
``0`` every check passed; ``1`` at least one check failed; ``2`` a usage,
argument or precondition error before/around the session (missing binary,
unreadable key file, bad timeout).

Usage
-----
    scripts/mcp-smoke.py --binary ~/.local/bin/localrouter
    scripts/mcp-smoke.py --binary ./localrouter --url http://127.0.0.1:8787 \
        --key-file ~/.config/localrouter/agent.key --account codex-primary --json
"""

from __future__ import annotations

import argparse
import json
import os
import queue
import shutil
import signal
import subprocess
import sys
import threading
import time

TOOL_NAME = "mcp-smoke"
SCHEMA_VERSION = 1

EXPECTED_TOOLS = ("quota_status", "usage_summary", "check_admission")

# Protocol versions the official Go MCP SDK v1.8.0 advertises (shared.go:58),
# newest first. The server may counter-offer any of these during initialize.
SUPPORTED_PROTOCOL_VERSIONS = (
    "2026-07-28",
    "2025-11-25",
    "2025-06-18",
    "2025-03-26",
    "2024-11-05",
)
# We request a version the legacy `initialize` handshake echoes back cleanly.
# The SDK caps initialize negotiation at 2025-11-25 (shared.go:76-99), so asking
# for 2026-07-28 would legitimately be answered with a lower version; asking
# for 2025-11-25 is echoed unchanged by any compliant server.
REQUEST_PROTOCOL_VERSION = "2025-11-25"
CLIENT_INFO = {"name": TOOL_NAME, "version": "1"}

DEFAULT_URL = "http://127.0.0.1:8787"
DEFAULT_RESPONSE_TIMEOUT = 10.0
DEFAULT_OVERALL_TIMEOUT = 30.0
# Teardown windows (module constants so tests can shrink them): how long to
# wait after stdin closes, after terminate, and after kill.
DEFAULT_TERM_TIMEOUT = 5.0
TERM_GRACE = 2.0
KILL_GRACE = 2.0

MAX_LINE_BYTES = 4 << 20  # one JSON-RPC line
STDERR_CHUNK = 65536  # drain granularity; stderr is counted, never retained
STDOUT_CHUNK = 65536  # reader granularity for the stdout pipe
POLL_SLICE = 0.25  # bounded queue wait so a deadline is always re-checked

# Sentinel events pushed by the stdout reader thread onto the client queue.
_EOF = "eof"  # child closed stdout (or the read failed permanently)
_READ_ERROR = "read_error"  # stdout read raised after the child started


class SmokeError(Exception):
    """A sanitized, machine-readable failure. ``code`` is a stable token;
    ``detail`` is an optional already-sanitized short token (never secrets)."""

    def __init__(self, code, detail=None):
        super().__init__(code)
        self.code = code
        self.detail = detail


# ---------------------------------------------------------------------------
# stdio JSON-RPC client
# ---------------------------------------------------------------------------


class StdioClient:
    """A minimal newline-delimited JSON-RPC client over a child process's
    stdin/stdout.

    Portability: stdout is drained by a background reader thread into a
    ``queue.Queue`` and consumed with a deadline-bounded ``queue.get``. This
    replaces the previous ``select.select`` over a pipe fd, which is POSIX-only
    (Windows ``select`` accepts sockets, not pipes). Process teardown uses a
    process group on POSIX (``os.killpg``, so a forked helper dies too) and
    ``Popen.terminate``/``Popen.kill`` elsewhere; ``signal.SIGKILL`` is only
    ever referenced inside the POSIX branch, since the name does not exist on
    Windows.
    """

    def __init__(self, argv, response_timeout, overall_deadline, term_timeout=DEFAULT_TERM_TIMEOUT):
        self.argv = argv
        self.response_timeout = response_timeout
        self.overall_deadline = overall_deadline
        self.term_timeout = term_timeout
        self.anomalies = []  # non-JSON stdout lines (bounded)
        self.notifications = []
        self.responses = {}
        self.stdout_bytes = 0
        self.stderr_bytes = 0
        self._buf = bytearray()
        self._eof = False
        self._proc = None
        self._events = queue.Queue()
        self._stdout_thread = None
        self._stderr_thread = None

    # -- lifecycle ----------------------------------------------------------

    def start(self):
        try:
            self._proc = subprocess.Popen(
                self.argv,
                stdin=subprocess.PIPE,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                start_new_session=(os.name == "posix"),
            )
        except FileNotFoundError:
            raise SmokeError("binary_not_found")
        except OSError as exc:
            raise SmokeError("spawn_failed", exc.__class__.__name__)
        self._stdout_thread = threading.Thread(target=self._read_stdout, daemon=True)
        self._stdout_thread.start()
        self._stderr_thread = threading.Thread(target=self._drain_stderr, daemon=True)
        self._stderr_thread.start()

    def _read_stdout(self):
        # One blocking os.read per chunk: returns as soon as any bytes are
        # available (unlike BufferedReader.read(n), which would wait for n
        # bytes and stall a line-at-a-time server). Works on pipe handles on
        # both POSIX and Windows.
        try:
            fd = self._proc.stdout.fileno()
        except (OSError, ValueError):
            self._events.put((_EOF, None))
            return
        while True:
            try:
                chunk = os.read(fd, STDOUT_CHUNK)
            except (OSError, ValueError):
                self._events.put((_READ_ERROR, None))
                return
            if not chunk:
                self._events.put((_EOF, None))
                return
            self.stdout_bytes += len(chunk)
            self._events.put(("data", chunk))

    def _drain_stderr(self):
        # Drain stderr so a full pipe can never stall the server, but retain
        # nothing: only the byte count is reported. stderr is never echoed to
        # stdout, so no server-controlled text (which may carry key material)
        # can reach the report.
        try:
            stream = self._proc.stderr
            while True:
                chunk = stream.read(STDERR_CHUNK)
                if not chunk:
                    return
                self.stderr_bytes += len(chunk)
        except (OSError, ValueError):
            return

    def close(self):
        """Guaranteed, bounded teardown: close stdin, wait, then escalate
        terminate -> kill, then release the pipes. Never raises; safe to call
        twice."""
        if self._proc is None:
            return
        try:
            if self._proc.stdin:
                self._proc.stdin.close()
        except OSError:
            pass
        try:
            if not self._wait(self.term_timeout):
                self._terminate()
                if not self._wait(TERM_GRACE):
                    self._kill()
                    self._wait(KILL_GRACE)
        finally:
            self._release_pipes()

    def _release_pipes(self):
        """Join the reader threads (bounded) and close the child's pipes, so a
        finished run never leaves a descriptor or a live thread behind."""
        for thread in (self._stdout_thread, self._stderr_thread):
            if thread is not None:
                thread.join(timeout=1.0)
        for stream in (self._proc.stdin, self._proc.stdout, self._proc.stderr):
            try:
                if stream is not None:
                    stream.close()
            except (OSError, ValueError):
                pass

    def _wait(self, timeout):
        try:
            self._proc.wait(timeout=timeout)
            return True
        except subprocess.TimeoutExpired:
            return False

    def _signal_group(self, sig):
        # POSIX only: signal the whole process group. Callers guard on os.name.
        try:
            os.killpg(os.getpgid(self._proc.pid), sig)
        except (OSError, ProcessLookupError, AttributeError):
            pass

    def _terminate(self):
        if os.name == "posix":
            self._signal_group(signal.SIGTERM)
            return
        try:
            self._proc.terminate()
        except OSError:
            pass

    def _kill(self):
        if os.name == "posix":
            self._signal_group(signal.SIGKILL)
            return
        try:
            self._proc.kill()
        except OSError:
            pass

    def returncode(self):
        return None if self._proc is None else self._proc.returncode

    # -- I/O ----------------------------------------------------------------

    def send(self, obj):
        data = (json.dumps(obj, separators=(",", ":")) + "\n").encode("utf-8")
        try:
            self._proc.stdin.write(data)
            self._proc.stdin.flush()
        except (BrokenPipeError, OSError):
            raise SmokeError("stdin_closed")

    def notify(self, method, params=None):
        msg = {"jsonrpc": "2.0", "method": method}
        if params is not None:
            msg["params"] = params
        self.send(msg)

    def request(self, msg_id, method, params, timeout=None):
        deadline = self.overall_deadline
        if timeout is not None:
            deadline = min(deadline, time.monotonic() + timeout)
        elif self.response_timeout:
            deadline = min(deadline, time.monotonic() + self.response_timeout)
        self.send({"jsonrpc": "2.0", "id": msg_id, "method": method, "params": params})
        if deadline is None or deadline <= time.monotonic():
            raise SmokeError("overall_timeout", method)
        while msg_id not in self.responses:
            if self._eof:
                raise SmokeError("server_closed_stdout", method)
            if time.monotonic() >= deadline:
                raise SmokeError("response_timeout", method)
            self._pump(min(deadline, time.monotonic() + POLL_SLICE))
        return self.responses.pop(msg_id)

    def _pump(self, deadline):
        """Consume already-read stdout chunks until *deadline* or the queue is
        empty. The reader thread owns the blocking read; here we only wait a
        bounded slice so the deadline is always re-checked."""
        while True:
            remain = deadline - time.monotonic()
            if remain <= 0:
                return
            try:
                kind, payload = self._events.get(timeout=min(remain, POLL_SLICE))
            except queue.Empty:
                return
            if kind == _EOF:
                self._eof = True
                return
            if kind == _READ_ERROR:
                raise SmokeError("stdout_closed")
            self._buf += payload
            if len(self._buf) > MAX_LINE_BYTES:
                raise SmokeError("oversized_stdout_line")
            while b"\n" in self._buf:
                line, self._buf = self._buf.split(b"\n", 1)
                self._handle_line(line)

    def _handle_line(self, raw):
        text = raw.strip()
        if not text:
            return
        try:
            msg = json.loads(text.decode("utf-8"))
        except (ValueError, UnicodeDecodeError):
            if len(self.anomalies) < 5:
                self.anomalies.append(text[:120].decode("utf-8", "replace"))
            return
        if isinstance(msg, dict) and "id" in msg and ("result" in msg or "error" in msg):
            self.responses[msg["id"]] = msg
        elif isinstance(msg, dict) and "method" in msg:
            if len(self.notifications) < 50:
                self.notifications.append(msg.get("method"))
        # anything else: structurally odd but still valid JSON; ignore.

    # -- response helpers ---------------------------------------------------

    @staticmethod
    def result_of(resp):
        if not isinstance(resp, dict):
            raise SmokeError("malformed_response")
        if "error" in resp and resp["error"] is not None:
            err = resp["error"]
            code = err.get("code") if isinstance(err, dict) else None
            return None, code
        return resp.get("result"), None


# ---------------------------------------------------------------------------
# check bookkeeping
# ---------------------------------------------------------------------------


class Report:
    def __init__(self):
        self.checks = []

    def add(self, name, status, detail=None):
        entry = {"check": name, "status": status}
        if detail is not None:
            entry["detail"] = detail
        self.checks.append(entry)

    def failed(self):
        return any(c["status"] == "fail" for c in self.checks)


def _is_object(v):
    return isinstance(v, dict)


def _discover_accounts(structured):
    """Best-effort extraction of account ids from a quota_status payload, so
    the smoke does not have to hardcode one."""
    found = []

    def walk(obj):
        if isinstance(obj, dict):
            for key, val in obj.items():
                if key in ("accounts", "rows") and isinstance(val, list):
                    for item in val:
                        if isinstance(item, dict) and isinstance(item.get("id"), str):
                            found.append(item["id"])
                walk(val)
        elif isinstance(obj, list):
            for item in obj:
                walk(item)

    walk(structured)
    # de-dup, preserve order
    seen, out = set(), []
    for a in found:
        if a not in seen:
            seen.add(a)
            out.append(a)
    return out


# ---------------------------------------------------------------------------
# main flow
# ---------------------------------------------------------------------------


def build_argv(args):
    argv = [args.binary, "mcp", "--url", args.url]
    if args.key_file:
        argv += ["--key-file", args.key_file]
    return argv


def run(args):
    report = Report()
    overall_deadline = time.monotonic() + args.overall_timeout

    if args.key_file and not os.path.isfile(args.key_file):
        raise SmokeError("key_file_missing")
    if os.sep not in args.binary and not os.path.isfile(args.binary):
        resolved = shutil.which(args.binary)
        if resolved:
            args.binary = resolved
    if not os.path.isfile(args.binary):
        raise SmokeError("binary_not_found")

    client = StdioClient(
        build_argv(args), args.response_timeout, overall_deadline,
        term_timeout=DEFAULT_TERM_TIMEOUT,
    )
    exit_code = 2
    try:
        client.start()
        _handshake(client, report, args)
        tools = _list_tools(client, report, args)
        account = _call_quota_status(client, report, args)
        _call_usage_summary(client, report, args)
        _call_check_admission(client, report, account, args)
        _adversarial(client, report, tools)
        if client.anomalies:
            report.add(
                "stdout_is_json_only",
                "fail",
                f"{len(client.anomalies)} non-JSON line(s)",
            )
        else:
            report.add("stdout_is_json_only", "pass")
    except SmokeError as exc:
        # Give a child that closed stdout a moment to exit so its status can be
        # reported; a binary without the `mcp` subcommand prints usage and exits
        # 2, which must surface as server_unavailable, never as a pass.
        if client.returncode() is None and exc.code in ("server_closed_stdout", "stdin_closed"):
            client._wait(0.5)
        rc = client.returncode()
        detail = exc.code if exc.detail is None else f"{exc.code}: {exc.detail}"
        if rc is not None:
            detail += f" (subprocess exit {rc})"
        report.add("session", "fail", detail)
        unavailable = exc.code in ("server_closed_stdout", "stdin_closed") or rc not in (None, 0)
        report.add("verdict", "fail", "server_unavailable" if unavailable else exc.code)
    finally:
        client.close()

    ok = not report.failed()
    exit_code = 0 if ok else 1
    _emit(report, client, args, ok, exit_code)
    return exit_code


def _handshake(client, report, args):
    resp = client.request(
        1,
        "initialize",
        {
            "protocolVersion": REQUEST_PROTOCOL_VERSION,
            "capabilities": {},
            "clientInfo": CLIENT_INFO,
        },
    )
    result, err = client.result_of(resp)
    if err is not None:
        report.add("initialize", "fail", f"jsonrpc error {err}")
        return
    if not _is_object(result):
        report.add("initialize", "fail", "missing result object")
        return
    version = result.get("protocolVersion")
    if version not in SUPPORTED_PROTOCOL_VERSIONS:
        report.add("initialize", "fail", f"unnegotiated version {version!r}")
    else:
        report.add("initialize", "pass", f"negotiated {version}")
    server_info = result.get("serverInfo")
    name = server_info.get("name") if isinstance(server_info, dict) else None
    report.add("server_info", "pass" if name else "fail", name or "absent")
    # initialized notification (no id); server rejects it if sent before init.
    client.notify("notifications/initialized")


def _list_tools(client, report, args):
    names = []
    cursor = None
    for _ in range(10):
        params = {} if cursor is None else {"cursor": cursor}
        resp = client.request(2, "tools/list", params)
        result, err = client.result_of(resp)
        if err is not None or not _is_object(result):
            report.add("tools_list", "fail", f"error {err}" if err else "missing result")
            return names
        tools = result.get("tools")
        if not isinstance(tools, list):
            report.add("tools_list", "fail", "tools not a list")
            return names
        for t in tools:
            if isinstance(t, dict) and isinstance(t.get("name"), str):
                names.append(t["name"])
        cursor = result.get("nextCursor")
        if not cursor:
            break
    got = set(names)
    want = set(EXPECTED_TOOLS)
    if got == want and len(names) == len(EXPECTED_TOOLS):
        report.add("tools_list", "pass", ",".join(sorted(got)))
    else:
        report.add(
            "tools_list",
            "fail",
            f"want {{{','.join(sorted(want))}}} got {{{','.join(sorted(got))}}}",
        )
    return names


def _structured_ok(client, resp, check):
    """Return (structuredContent, None) or (None, fail-detail)."""
    result, err = client.result_of(resp)
    if err is not None:
        return None, f"jsonrpc error {err}"
    if not _is_object(result):
        return None, "missing result object"
    if result.get("isError"):
        return None, "isError set"
    sc = result.get("structuredContent")
    if not _is_object(sc):
        return None, "no structuredContent object"
    return sc, None


def _call_quota_status(client, report, args):
    resp = client.request(3, "tools/call", {"name": "quota_status", "arguments": {}})
    sc, err = _structured_ok(client, resp, "quota_status")
    if err:
        report.add("quota_status", "fail", err)
        return args.account
    accounts = _discover_accounts(sc)
    if accounts:
        detail = f"{len(accounts)} accounts: {','.join(accounts[:8])}"
        if len(accounts) > 8:
            detail += ",..."
    else:
        detail = "no account ids discovered"
    report.add("quota_status", "pass", detail)
    if args.account:
        report.add(
            "account",
            "pass" if args.account in accounts or not accounts else "warn",
            f"using --account {args.account}",
        )
        return args.account
    return accounts[0] if accounts else ""


def _call_usage_summary(client, report, args):
    args_in = {"since": "24h", "group": "model", "limit": args.limit}
    resp = client.request(4, "tools/call", {"name": "usage_summary", "arguments": args_in})
    sc, err = _structured_ok(client, resp, "usage_summary")
    if err:
        report.add("usage_summary", "fail", err)
        return
    rows = None
    if isinstance(sc.get("rows"), list):
        rows = sc["rows"]
    elif isinstance(sc.get("usage"), dict) and isinstance(sc["usage"].get("rows"), list):
        rows = sc["usage"]["rows"]
    if rows is None:
        report.add("usage_summary", "fail", "no rows array in structuredContent")
        return
    if len(rows) > args.limit:
        report.add("usage_summary", "fail", f"limit {args.limit} ignored ({len(rows)} rows)")
    else:
        keys = sorted(rows[0].keys()) if rows and isinstance(rows[0], dict) else []
        report.add("usage_summary", "pass", f"{len(rows)} row(s); row keys {keys}")


def _call_check_admission(client, report, account, args):
    if not account:
        report.add("check_admission", "fail", "no account to check (discovery empty)")
        return
    args_in = {"class": "background", "account": account}
    resp = client.request(5, "tools/call", {"name": "check_admission", "arguments": args_in})
    sc, err = _structured_ok(client, resp, "check_admission")
    if err:
        report.add("check_admission", "fail", err)
        return
    decision = sc.get("decision")
    if decision not in ("allow", "deny"):
        report.add("check_admission", "fail", f"unexpected decision {decision!r}")
        return
    # Either decision is a valid dry-run outcome; record which.
    report.add("check_admission", "pass", f"{decision} on {account}")


def _adversarial(client, report, tools):
    # 1. Unknown tool must produce a JSON-RPC error (SDK: callTool -> -32602).
    if "check_admission" in tools:
        resp = client.request(6, "tools/call", {"name": "no_such_tool_zzz", "arguments": {}})
        result, err = client.result_of(resp)
        if err is not None:
            report.add("unknown_tool_errors", "pass", f"jsonrpc error {err}")
        elif isinstance(result, dict) and result.get("isError"):
            report.add("unknown_tool_errors", "pass", "tool isError")
        else:
            report.add("unknown_tool_errors", "fail", "unknown tool accepted")
    else:
        report.add("unknown_tool_errors", "skip", "check_admission absent")

    # 2. Wrongly-typed arguments for a known tool must be rejected (SDK schema
    #    validation yields a tool result with isError=true, or a JSON-RPC error
    #    if the server validates itself).
    if "usage_summary" in tools:
        resp = client.request(
            7,
            "tools/call",
            {"name": "usage_summary", "arguments": {"since": "24h", "group": "model", "limit": "not-an-int"}},
        )
        result, err = client.result_of(resp)
        if err is not None:
            report.add("bad_args_errors", "pass", f"jsonrpc error {err}")
        elif isinstance(result, dict) and result.get("isError"):
            report.add("bad_args_errors", "pass", "tool isError")
        else:
            report.add("bad_args_errors", "fail", "wrong-typed limit accepted")
    else:
        report.add("bad_args_errors", "skip", "usage_summary absent")

    # 3. Semantically invalid enum (bad class) should be refused, not silently
    #    coerced. Accept either an error signal or an explicit deny.
    resp = client.request(8, "tools/call", {"name": "check_admission", "arguments": {"class": "bogus", "account": "x"}})
    result, err = client.result_of(resp)
    if err is not None:
        report.add("bad_enum_handled", "pass", f"jsonrpc error {err}")
    elif isinstance(result, dict) and result.get("isError"):
        report.add("bad_enum_handled", "pass", "tool isError")
    else:
        sc = result.get("structuredContent") if isinstance(result, dict) else None
        decision = sc.get("decision") if isinstance(sc, dict) else None
        if decision == "deny":
            report.add("bad_enum_handled", "pass", "deny")
        else:
            report.add("bad_enum_handled", "fail", "bad class accepted")


def _emit(report, client, args, ok, exit_code):
    summary = {
        "tool": TOOL_NAME,
        "schema_version": SCHEMA_VERSION,
        "ok": ok,
        "exit_code": exit_code,
        "checks": report.checks,
        "stdout_bytes": client.stdout_bytes,
        "stderr_bytes": client.stderr_bytes,
    }
    if args.json:
        print(json.dumps(summary, indent=2))
    else:
        print(f"{TOOL_NAME}: {'PASS' if ok else 'FAIL'} (exit {exit_code})")
        for c in report.checks:
            line = f"  [{c['status']}] {c['check']}"
            if "detail" in c:
                line += f": {c['detail']}"
            print(line)
        print(f"  stdout {client.stdout_bytes}B, stderr {client.stderr_bytes}B")


def parse_args(argv):
    p = argparse.ArgumentParser(
        prog=TOOL_NAME,
        description="Bounded stdio acceptance smoke test for the `localrouter mcp` server.",
    )
    p.add_argument("--binary", required=True, help="path to the localrouter binary")
    p.add_argument("--url", default=DEFAULT_URL, help=f"control API base URL (default {DEFAULT_URL})")
    p.add_argument("--key-file", default="", help="client key FILE passed through to the server (never read)")
    p.add_argument("--account", default="", help="account id for check_admission (default: discover from quota_status)")
    p.add_argument("--limit", type=int, default=3, help="usage_summary row limit (default 3)")
    p.add_argument("--response-timeout", type=float, default=DEFAULT_RESPONSE_TIMEOUT,
                   help=f"seconds to wait for each response (default {DEFAULT_RESPONSE_TIMEOUT})")
    p.add_argument("--overall-timeout", type=float, default=DEFAULT_OVERALL_TIMEOUT,
                   help=f"seconds for the whole session (default {DEFAULT_OVERALL_TIMEOUT})")
    p.add_argument("--json", action="store_true", help="emit a single JSON object")
    args = p.parse_args(argv)
    if args.limit < 1:
        p.error("--limit must be >= 1")
    if args.response_timeout <= 0 or args.overall_timeout <= 0:
        p.error("timeouts must be positive")
    return args


def main(argv=None):
    args = parse_args(sys.argv[1:] if argv is None else argv)
    try:
        return run(args)
    except SmokeError as exc:
        detail = exc.code if exc.detail is None else f"{exc.code}: {exc.detail}"
        payload = {
            "tool": TOOL_NAME,
            "schema_version": SCHEMA_VERSION,
            "ok": False,
            "exit_code": 2,
            "checks": [{"check": "preconditions", "status": "fail", "detail": detail}],
        }
        if args.json:
            print(json.dumps(payload, indent=2))
        else:
            print(f"{TOOL_NAME}: ERROR ({detail})")
        return 2


if __name__ == "__main__":
    sys.exit(main())
