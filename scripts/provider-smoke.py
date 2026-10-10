#!/usr/bin/env python3
"""provider-smoke - opt-in live acceptance smoke test for a LocalRouter endpoint.

Purpose
-------
Prove that an *operator-supplied* LocalRouter instance can actually complete a
real inference request through whatever upstream provider its routes select.
Two API surfaces are supported, chosen with ``--protocol``:

* ``chat`` (default) - the OpenAI chat-completions API: a non-streaming
  completion plus a genuine streamed (SSE) completion, checking the SSE framing
  (``data:`` lines, a terminating ``data: [DONE]``) and the token/cost usage the
  provider actually returned.
* ``responses`` - the OpenAI Responses API as driven by Codex: a non-streaming
  response plus a streamed response using the ``input`` / ``max_output_tokens``
  request shape and the ``response.output_text.delta`` / ``response.completed``
  event family. Responses streams do NOT carry a chat ``[DONE]``; completion is
  ``response.completed`` and failure is ``response.failed``/``response.incomplete``.

A 200 is only accepted when it carries genuine, nonempty generated text. Token
counts alone, an empty ``choices`` list, a usage-only object, or a stream whose
only content is ``[DONE]`` are failures, not passes.

Safety model (read this before using ``--live``)
-----------------------------------------------
* Nothing is discovered. The tool reads *only* the URL, key file, model, protocol
  and timeouts given on the command line. It never reads LocalRouter config, never
  looks at ``~/.config/localrouter``, never consults environment variables and
  has no default key path.
* ``--live`` is required before any network call. Without it (including the
  bare invocation and ``--help``) the tool makes ZERO network calls and never
  opens the key file.
* Plain ``http://`` is accepted only for loopback hosts. A non-loopback plain
  HTTP target is refused unless the operator explicitly passes
  ``--allow-insecure-http``, which sends the client key in cleartext.
* Redirects are never followed, so the bearer key cannot be replayed to
  another origin. A 3xx is reported as a failure and the ``Location`` header is
  never read or printed.
* The client key, the synthetic prompt, and every response body are never
  written to stdout, to the report, or into an error message. Only sanitized
  machine-readable outcome metadata is emitted.
* NO provider-controlled ``error.type``, ``error.message`` or any other
  provider-supplied detail is ever surfaced. Provider errors are reduced to a
  stable local code (``http_status`` / ``error_object`` / ``stream_error``) and
  ``detail`` is ``None``; nothing is matched back out with a regex, because a
  plausible-looking error token can itself be (part of) a credential.
* Response bodies are read under a hard byte cap, checked with a single extra
  byte so a body of exactly the cap is not misreported as oversized; connect,
  per-read and overall deadlines bound execution, including a stalled SSE
  stream. Each connect/header/read socket timeout is clamped to the time
  remaining in the overall budget, and on the main thread a wall-clock alarm
  also interrupts a stalled TLS handshake, a stalled header read and a body
  that trickles bytes within the per-read window. DNS resolution is the
  exception: a blocking ``getaddrinfo`` is bounded only by the system
  resolver's own timeouts, because the alarm's Python handler cannot run until
  that C call returns.
* The tool changes no production state: no database access, no schema changes,
  no writes anywhere.

Exit codes
----------
``0`` every check passed; ``1`` a check failed (HTTP error, malformed SSE, a
missing ``[DONE]``/terminal event, timeout, oversized response, no generation,
an unreadable ``--cafile`` or bad TLS configuration, ...); ``2`` a usage,
consent, URL-policy, key-file, protocol or argument error raised *before* any
request (key-file errors are detected by reading the key file).

Live requests spend provider quota and money and cause network egress. Run this
only against an isolated LocalRouter you control. See
``docs/PROVIDER-ACCEPTANCE.md``.
"""

from __future__ import annotations

import argparse
import http.client
import ipaddress
import json
import math
import signal
import socket
import ssl
import sys
import threading
import time
import urllib.parse

TOOL_NAME = "provider-smoke"
SCHEMA_VERSION = 1

# A deliberately small synthetic request: a fixed prompt and a 64-token cap.
MAX_TOKENS = 64
SYNTHETIC_PROMPT = "Reply with the single word: ok"

# Hard caps so a hostile or broken endpoint cannot exhaust memory.
MAX_RESPONSE_BYTES = 1 << 20  # 1 MiB per response body
MAX_EVENT_BYTES = 64 << 10  # one SSE line or event
MAX_EVENTS = 10000  # events parsed per stream
MAX_MODEL_LEN = 256
_CHUNK = 256  # read size; small enough to exercise fragmented SSE framing
_REDIRECT_STATUSES = frozenset({301, 302, 303, 307, 308})
_LOOPBACK_NAMES = frozenset({"localhost", "localhost.localdomain", "ip6-localhost", "ip6-loopback"})

# Protocol selectors.
CHAT = "chat"
RESPONSES = "responses"
_PROTOCOLS = (CHAT, RESPONSES)


class SmokeError(Exception):
    """A sanitized, machine-readable failure. ``code`` is a stable token and
    ``detail`` is an optional already-sanitized short operator-supplied token
    (never provider-controlled text)."""

    def __init__(self, code, detail=None):
        super().__init__(code)
        self.code = code
        self.detail = detail


# --------------------------------------------------------------------------
# Argument / policy validation
# --------------------------------------------------------------------------


class _Target:
    __slots__ = ("scheme", "host", "port", "loopback", "base_path", "cafile")

    def __init__(self, scheme, host, port, loopback, base_path, cafile):
        self.scheme = scheme
        self.host = host
        self.port = port
        self.loopback = loopback
        self.base_path = base_path
        self.cafile = cafile


def _is_loopback(host):
    try:
        return ipaddress.ip_address(host).is_loopback
    except ValueError:
        return host.lower() in _LOOPBACK_NAMES


def guard_url(url, allow_insecure_http=False, cafile=None):
    """Validate an operator-supplied base URL.

    Refuses unsupported schemes, embedded credentials, non-loopback plain HTTP
    (without an explicit opt-out), query/fragment components and a path that
    is not printable ASCII (percent-encode it instead). A URL that the
    stdlib cannot parse at all (for example an unbalanced IPv6 literal) is a
    sanitized ``invalid_url`` error rather than an escaping ``ValueError``.
    Returns a ``_Target`` describing scheme, connect host/port and any base path
    prefix.
    """
    try:
        parts = urllib.parse.urlsplit(url.strip())
    except ValueError:
        # e.g. urlsplit("http://[::1") -> "Invalid IPv6 URL"
        raise SmokeError("invalid_url")
    scheme = parts.scheme.lower()
    if scheme not in ("http", "https"):
        raise SmokeError("unsupported_scheme")
    if parts.username or parts.password:
        raise SmokeError("url_userinfo_not_allowed")
    if parts.query or parts.fragment:
        raise SmokeError("url_query_not_allowed")
    try:
        host = parts.hostname
    except ValueError:
        raise SmokeError("invalid_url")
    if not host:
        raise SmokeError("missing_host")
    try:
        port = parts.port
    except ValueError:
        raise SmokeError("bad_port")
    if port is None:
        port = 443 if scheme == "https" else 80
    if not 1 <= port <= 65535:
        raise SmokeError("bad_port")
    loopback = _is_loopback(host)
    if scheme == "http" and not loopback and not allow_insecure_http:
        raise SmokeError("nonloopback_plain_http_refused")
    base_path = parts.path.rstrip("/")
    if base_path == "/":
        base_path = ""
    if base_path and not _printable_ascii(base_path):
        # http.client can only send an ASCII request line; percent-encode.
        raise SmokeError("invalid_url")
    return _Target(scheme, host, port, loopback, base_path, cafile if scheme == "https" else None)


def _positive_seconds(value, name):
    try:
        seconds = float(value)
    except (TypeError, ValueError):
        raise SmokeError("bad_timeout", name)
    if not math.isfinite(seconds) or seconds <= 0:
        raise SmokeError("bad_timeout", name)
    return seconds


def _valid_model(model):
    if not model or len(model) > MAX_MODEL_LEN:
        return False
    return not any(ord(ch) < 0x20 or ord(ch) == 0x7F for ch in model)


# --------------------------------------------------------------------------
# Secret handling
# --------------------------------------------------------------------------


def _printable_ascii(text):
    """True when every character is printable ASCII other than space
    (0x21-0x7E): safe in an HTTP header or request line."""
    return all(0x21 <= ord(ch) <= 0x7E for ch in text)


def _read_key_file(path):
    """Read an operator-supplied client key file. The value is never logged,
    never returned in an error message and never written to the report. A file
    that is not valid UTF-8 is rejected as ``key_file_invalid`` (an
    uncaught ``UnicodeDecodeError`` would otherwise escape as a crash), and so
    is a key containing anything but printable ASCII: http.client cannot
    encode such a header and would crash after connecting, with the
    offending character in the traceback."""
    try:
        with open(path, "r", encoding="utf-8") as handle:
            data = handle.read(64 << 10)
    except UnicodeDecodeError:
        raise SmokeError("key_file_invalid")
    except OSError:
        raise SmokeError("key_file_unreadable")
    key = data.strip()
    if not key:
        raise SmokeError("key_file_empty")
    if not _printable_ascii(key):
        raise SmokeError("key_file_invalid")
    if len(key) > 8192:
        raise SmokeError("key_file_invalid")
    return key


# --------------------------------------------------------------------------
# HTTP exchange with bounded, deadline-checked reads
# --------------------------------------------------------------------------


def _open_connection(scheme, host, port, connect_timeout, cafile):
    try:
        if scheme == "https":
            context = ssl.create_default_context(cafile=cafile or None)
            return http.client.HTTPSConnection(host, port, timeout=connect_timeout, context=context)
        return http.client.HTTPConnection(host, port, timeout=connect_timeout)
    except FileNotFoundError:
        raise SmokeError("cafile_unreadable")
    except (OSError, ssl.SSLError):
        raise SmokeError("tls_config_error")


def _timeout_error(deadline, code="read_timeout"):
    if deadline - time.monotonic() <= 0:
        return SmokeError("overall_timeout")
    return SmokeError(code)


class _Accumulator:
    """Collects a bounded response body for later structured parsing."""

    def __init__(self):
        self.buf = bytearray()
        self.stop_requested = False

    def feed(self, data):
        self.buf += data


class SSEParser:
    """Incremental SSE parser for a chat-completions or Responses stream.

    Handles events split across arbitrary read boundaries, CRLF, multiple
    ``data:`` lines per event, comment lines, the ``data: [DONE]`` terminator
    (chat) and the ``response.*`` event family (Responses). It never retains the
    whole stream: one line/event is bounded by ``MAX_EVENT_BYTES`` and the caller
    bounds the total.

    It records whether the stream produced genuine text (``text_seen``: some
    non-whitespace character, matching the non-stream check; ``text_chars``
    counts every text character), whether it carried a provider error object
    (``stream_error``), and, for Responses, whether it reached a terminal
    ``response.completed`` (``completed``) or a ``response.failed`` /
    ``response.incomplete`` event.
    """

    def __init__(self, protocol=CHAT):
        self.protocol = protocol
        self._buf = bytearray()
        self._event = None
        self._data = []
        self.events = 0
        self.done = False
        self.completed = False
        self.failed = False
        self.incomplete = False
        self.stream_error = False
        self.text_seen = False
        self.text_chars = 0
        self.usage_seen = False
        self.tokens = None
        self.usage_shape = None
        self.cost_present = False
        self.cost = None
        self.stop_requested = False

    def feed(self, data):
        self._buf += data
        while True:
            index = self._buf.find(b"\n")
            if index < 0:
                if len(self._buf) > MAX_EVENT_BYTES:
                    raise SmokeError("oversize_event")
                break
            line = bytes(self._buf[:index])
            del self._buf[: index + 1]
            if len(line) > MAX_EVENT_BYTES:
                raise SmokeError("oversize_event")
            self._line(line.rstrip(b"\r").decode("utf-8", "replace"))

    def finish(self):
        if self._buf:
            line = bytes(self._buf).rstrip(b"\r")
            self._buf = bytearray()
            self._line(line.decode("utf-8", "replace"))
        if self._data:
            self._dispatch()

    def _line(self, line):
        if line == "":
            self._dispatch()
            return
        if line.startswith(":"):
            return
        field, sep, value = line.partition(":")
        if sep and value.startswith(" "):
            value = value[1:]
        if field == "event":
            self._event = value
        elif field == "data":
            if len(self._data) < 1024:
                self._data.append(value)

    def _dispatch(self):
        data = "\n".join(self._data)
        self._data = []
        event = self._event
        self._event = None
        if data == "":
            return
        self.events += 1
        if self.events > MAX_EVENTS:
            raise SmokeError("too_many_events")
        if data == "[DONE]":
            # Chat terminator. Responses streams have no [DONE]; tolerate a
            # stray one but never require it there.
            self.done = True
            self.stop_requested = True
            return
        try:
            obj = json.loads(data)
        except ValueError:
            raise SmokeError("malformed_sse")
        if not isinstance(obj, dict):
            raise SmokeError("malformed_sse")
        # Any provider error object fails the stream, even if a [DONE] or a
        # terminal event follows. Only the presence is recorded; the provider
        # text is never surfaced.
        if obj.get("error"):
            self.stream_error = True
            self.stop_requested = True
            return
        if self.protocol == RESPONSES:
            self._absorb_responses(obj, event)
        else:
            self._absorb_chat(obj)

    def _absorb_chat(self, obj):
        choices = obj.get("choices")
        if isinstance(choices, list):
            for choice in choices:
                if not isinstance(choice, dict):
                    continue
                delta = choice.get("delta")
                if isinstance(delta, dict):
                    self._absorb_text(delta.get("content"))
        self._absorb_usage(self._usage_of(obj))

    def _absorb_responses(self, obj, event):
        etype = obj.get("type") if isinstance(obj.get("type"), str) else event
        if etype == "response.completed":
            self.completed = True
            self.stop_requested = True
            inner = obj.get("response")
            usage = None
            if isinstance(inner, dict):
                usage = inner.get("usage")
                self._absorb_responses_output(inner)
            if usage is None:
                usage = obj.get("usage")
            self._absorb_usage(usage)
            return
        if etype == "response.failed":
            self.failed = True
            self.stop_requested = True
            return
        if etype == "response.incomplete":
            self.incomplete = True
            self.stop_requested = True
            return
        if etype in ("error", "response.error"):
            self.stream_error = True
            self.stop_requested = True
            return
        if etype == "response.output_text.delta":
            delta = obj.get("delta")
            if isinstance(delta, str) and delta:
                self._absorb_text(delta)
            return
        # Non-terminal bookkeeping events may still carry usage.
        self._absorb_usage(self._usage_of(obj))

    def _absorb_text(self, value):
        if isinstance(value, str):
            self._absorb_str(value)
        elif isinstance(value, list):
            for part in value:
                if isinstance(part, dict):
                    self._absorb_str(part.get("text"))

    def _absorb_str(self, text):
        if isinstance(text, str) and text:
            self.text_chars += len(text)
            if text.strip():
                self.text_seen = True

    def _absorb_responses_output(self, inner):
        # Only used when the completed event's payload carries the text itself
        # (providers that send no genuine deltas). Genuine deltas already count.
        if self.text_seen:
            return
        output = inner.get("output")
        if not isinstance(output, list):
            return
        for item in output:
            if not isinstance(item, dict):
                continue
            self._absorb_text(item.get("content"))

    @staticmethod
    def _usage_of(obj):
        usage = obj.get("usage")
        if usage is None and isinstance(obj.get("response"), dict):
            usage = obj["response"].get("usage")
        return usage

    def _absorb_usage(self, usage):
        if not isinstance(usage, dict):
            return
        self.usage_seen = True
        tokens, shape = _tokens_from_usage(usage)
        if tokens is not None:
            self.tokens, self.usage_shape = tokens, shape
        if "cost" in usage:
            self.cost_present = True
            self.cost = _usable_cost(usage.get("cost"))


def _close(resp, conn):
    for closer in (getattr(resp, "close", None), getattr(conn, "close", None)):
        if closer is not None:
            try:
                closer()
            except Exception:
                pass


def _endpoint(protocol):
    return "/v1/responses" if protocol == RESPONSES else "/v1/chat/completions"


def _chat_body(model, stream):
    body = {
        "model": model,
        "messages": [{"role": "user", "content": SYNTHETIC_PROMPT}],
        "max_tokens": MAX_TOKENS,
        "stream": bool(stream),
    }
    if stream:
        # Ask for usage explicitly; LocalRouter preserves other stream_options
        # and also forces include_usage for streaming chat completions.
        body["stream_options"] = {"include_usage": True}
    return body


def _responses_body(model, stream):
    """Minimal Codex-compatible Responses request.

    Codex drives the Responses API with a message-list ``input`` whose content
    is ``input_text`` parts, an ``max_output_tokens`` cap and ``store: false``.
    """
    return {
        "model": model,
        "input": [
            {
                "type": "message",
                "role": "user",
                "content": [{"type": "input_text", "text": SYNTHETIC_PROMPT}],
            }
        ],
        "max_output_tokens": MAX_TOKENS,
        "store": False,
        "stream": bool(stream),
    }


def _request_body(protocol, model, stream):
    if protocol == RESPONSES:
        return _responses_body(model, stream)
    return _chat_body(model, stream)


def _exchange(ctx, method, path, body, make_sink=None):
    """Perform one request. Returns status, content type, bytes read, whether the
    cap/EOF truncated the body, the sink and latency. Never returns body bytes to
    the caller except through the sink, which the caller controls.

    Every socket timeout is clamped to the time remaining in the overall budget
    so a per-connect or per-read timeout can never let the whole run overrun it.
    """
    target = ctx.target
    remaining = ctx.deadline - time.monotonic()
    if remaining <= 0:
        raise SmokeError("overall_timeout")
    connect_timeout = max(0.05, min(ctx.connect_timeout, remaining))
    conn = _open_connection(target.scheme, target.host, target.port, connect_timeout, target.cafile)
    try:
        try:
            conn.connect()
        except ssl.SSLError:
            raise SmokeError("tls_failed")
        except socket.timeout:
            # TCP connect or TLS handshake: both run under the connect timeout.
            raise _timeout_error(ctx.deadline, "connect_timeout")
        except OSError:
            raise SmokeError("connect_failed")

        headers = {
            "Authorization": "Bearer " + ctx.key,
            "Accept": "application/json",
            "User-Agent": TOOL_NAME,
        }
        payload = None
        if body is not None:
            try:
                payload = json.dumps(body, separators=(",", ":")).encode("utf-8")
            except (TypeError, ValueError):
                raise SmokeError("bad_request_body")
            headers["Content-Type"] = "application/json"

        start = time.monotonic()
        try:
            remaining = ctx.deadline - time.monotonic()
            if remaining <= 0:
                raise SmokeError("overall_timeout")
            if conn.sock is not None:
                # Clamp the header/response wait to the remaining budget too:
                # a stalled header must not get a fresh full read_timeout.
                conn.sock.settimeout(max(0.05, min(ctx.read_timeout, remaining)))
            conn.request(method, path, body=payload, headers=headers)
            resp = conn.getresponse()
        except socket.timeout:
            raise _timeout_error(ctx.deadline)
        except ssl.SSLError:
            raise SmokeError("tls_failed")
        except (http.client.HTTPException, OSError):
            raise SmokeError("connect_failed")

        status = resp.status
        content_type = (resp.getheader("Content-Type") or "").strip().lower()
        # Never follow a redirect: report it and stop, so the bearer key cannot
        # be replayed elsewhere and Location is never read.
        if status in _REDIRECT_STATUSES:
            raise SmokeError("unexpected_redirect")

        # Only a 200 body is handed to the caller's sink. Every check reports a
        # non-200 as http_status without parsing the body, so an error served
        # as text/event-stream must not trip the SSE parser and mask the status.
        sink = make_sink(content_type) if (make_sink and status == 200) else _Accumulator()
        total = 0
        truncated = False
        try:
            while True:
                if getattr(sink, "stop_requested", False):
                    break
                remaining = ctx.deadline - time.monotonic()
                if remaining <= 0:
                    raise SmokeError("overall_timeout")
                if conn.sock is not None:
                    conn.sock.settimeout(max(0.05, min(ctx.read_timeout, remaining)))
                room = MAX_RESPONSE_BYTES - total
                # Read at most ONE byte past the cap so a body of exactly the
                # cap is not misclassified as oversized.
                want = min(_CHUNK, room + 1)
                try:
                    chunk = resp.read(want)
                except socket.timeout:
                    raise _timeout_error(ctx.deadline)
                except http.client.IncompleteRead:
                    truncated = True
                    break
                except (http.client.HTTPException, OSError):
                    raise SmokeError("read_failed")
                if not chunk:
                    break
                before = total
                total += len(chunk)
                if total > MAX_RESPONSE_BYTES:
                    # Keep the sink within the cap; the extra byte is only a
                    # bounded overflow detector and is never retained or
                    # counted in the reported size.
                    allowed = max(0, MAX_RESPONSE_BYTES - before)
                    if allowed:
                        sink.feed(chunk[:allowed])
                    total = MAX_RESPONSE_BYTES
                    truncated = True
                    break
                sink.feed(chunk)
        finally:
            _close(resp, conn)
        latency_ms = int((time.monotonic() - start) * 1000)
        return {
            "status": status,
            "content_type": content_type,
            "bytes_read": total,
            "truncated": truncated,
            "sink": sink,
            "latency_ms": latency_ms,
        }
    except BaseException:
        try:
            conn.close()
        except Exception:
            pass
        raise


# --------------------------------------------------------------------------
# Usage / cost extraction (provider-reported only; never fabricated)
# --------------------------------------------------------------------------


def _as_int(value):
    if isinstance(value, bool) or not isinstance(value, int):
        return 0
    return value


def _nested_int(container, key):
    if isinstance(container, dict):
        return _as_int(container.get(key))
    return 0


def _tokens_from_usage(usage):
    """Return (tokens dict, shape name) or (None, None) when the usage object
    carries no recognized token counts."""
    if not isinstance(usage, dict):
        return None, None
    if "prompt_tokens" in usage or "completion_tokens" in usage:
        return (
            {
                "input": _as_int(usage.get("prompt_tokens")),
                "output": _as_int(usage.get("completion_tokens")),
                "cached_input": _nested_int(usage.get("prompt_tokens_details"), "cached_tokens"),
                "reasoning": _nested_int(usage.get("completion_tokens_details"), "reasoning_tokens"),
            },
            "prompt_tokens/completion_tokens",
        )
    if "input_tokens" in usage or "output_tokens" in usage:
        return (
            {
                "input": _as_int(usage.get("input_tokens")),
                "output": _as_int(usage.get("output_tokens")),
                "cached_input": _nested_int(usage.get("input_tokens_details"), "cached_tokens"),
                "reasoning": _nested_int(usage.get("output_tokens_details"), "reasoning_tokens"),
            },
            "input_tokens/output_tokens",
        )
    return None, None


def _usable_cost(value):
    """A usable provider-reported cost is a finite, non-negative JSON number.
    An explicit zero is valid. Everything else yields None (no cost is ever
    invented to fill the gap)."""
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        return None
    number = float(value)
    if not math.isfinite(number) or number < 0:
        return None
    return number


def _usage_report(usage_seen, tokens, shape, cost, cost_present, warnings):
    if not usage_seen:
        warnings.append("usage missing; no cost reported (nothing fabricated)")
    elif tokens is None:
        warnings.append("usage present but no recognized token counts")
    if cost_present and cost is None:
        warnings.append("usage.cost present but unusable; ignored, not fabricated")
    return {
        "usage": "present" if usage_seen else "missing",
        "usage_shape": shape,
        "tokens": tokens,
        "cost": {"usd": cost, "source": "usage.cost"} if cost is not None else None,
    }


# --------------------------------------------------------------------------
# Generated-text extraction (the generation-proof requirement)
# --------------------------------------------------------------------------


def _pick_text(content):
    """Return the nonempty text carried by a chat ``content`` value, or None."""
    if isinstance(content, str):
        return content if content.strip() else None
    if isinstance(content, list):
        parts = []
        for part in content:
            if isinstance(part, dict):
                text = part.get("text")
                if isinstance(text, str) and text:
                    parts.append(text)
        joined = "".join(parts)
        if joined.strip():
            return joined
    return None


def _chat_text(obj):
    """Genuine nonempty assistant text from a chat completion, or None.

    Rejects an empty ``choices`` list, a non-list ``choices`` value, a
    usage-only object and blank content: token counts are not generation.
    """
    choices = obj.get("choices")
    if not isinstance(choices, list):
        return None
    for choice in choices:
        if not isinstance(choice, dict):
            continue
        message = choice.get("message")
        if isinstance(message, dict):
            text = _pick_text(message.get("content"))
            if text is not None:
                return text
        text = _pick_text(choice.get("text"))
        if text is not None:
            return text
    return None


def _responses_text(obj):
    """Genuine nonempty output text from a Responses object, or None."""
    text = obj.get("output_text")
    if isinstance(text, str) and text.strip():
        return text
    output = obj.get("output")
    if isinstance(output, list):
        parts = []
        for item in output:
            if not isinstance(item, dict):
                continue
            content = item.get("content")
            if isinstance(content, list):
                for part in content:
                    if isinstance(part, dict):
                        value = part.get("text")
                        if isinstance(value, str) and value:
                            parts.append(value)
        joined = "".join(parts)
        if joined.strip():
            return joined
    return None


# --------------------------------------------------------------------------
# Checks
# --------------------------------------------------------------------------


class _Ctx:
    __slots__ = ("target", "key", "model", "base_path", "connect_timeout", "read_timeout", "deadline")

    def __init__(self, target, key, model, connect_timeout, read_timeout, deadline):
        self.target = target
        self.key = key
        self.model = model
        self.base_path = target.base_path
        self.connect_timeout = connect_timeout
        self.read_timeout = read_timeout
        self.deadline = deadline


def check_models(ctx):
    out = _exchange(ctx, "GET", ctx.base_path + "/v1/models", None, make_sink=lambda _ct: _Accumulator())
    result = {
        "status": out["status"],
        "latency_ms": out["latency_ms"],
        "bytes_read": out["bytes_read"],
        "truncated": out["truncated"],
    }
    if out["status"] != 200:
        result.update(ok=False, error="http_status", detail=None)
        return result
    try:
        obj = json.loads(bytes(out["sink"].buf).decode("utf-8", "replace"))
    except ValueError:
        result.update(ok=False, error="malformed_json", detail=None)
        return result
    data = obj.get("data") if isinstance(obj, dict) else None
    if not isinstance(data, list):
        result.update(ok=False, error="unexpected_shape", detail=None)
        return result
    ids = [entry.get("id") for entry in data if isinstance(entry, dict)]
    listed = ctx.model in ids
    # Count and verdict only; the model list itself is not echoed.
    result.update(ok=bool(listed), error=None if listed else "model_not_routed",
                  detail=None, models_count=len(ids), model_listed=bool(listed))
    return result


def _check_nonstream(ctx, protocol):
    out = _exchange(
        ctx, "POST", ctx.base_path + _endpoint(protocol),
        _request_body(protocol, ctx.model, stream=False),
        make_sink=lambda _ct: _Accumulator(),
    )
    result = {
        "status": out["status"],
        "latency_ms": out["latency_ms"],
        "bytes_read": out["bytes_read"],
        "truncated": out["truncated"],
    }
    if out["status"] != 200:
        result.update(ok=False, error="http_status", detail=None)
        return result
    if out["truncated"]:
        result.update(ok=False, error="response_too_large", detail=None)
        return result
    try:
        obj = json.loads(bytes(out["sink"].buf).decode("utf-8", "replace"))
    except ValueError:
        result.update(ok=False, error="malformed_json", detail=None)
        return result
    if not isinstance(obj, dict):
        result.update(ok=False, error="unexpected_shape", detail=None)
        return result
    if obj.get("error"):
        result.update(ok=False, error="error_object", detail=None)
        return result
    if protocol == RESPONSES:
        status = obj.get("status")
        if status == "failed":
            result.update(ok=False, error="response_failed", detail=None)
            return result
        if status == "incomplete":
            result.update(ok=False, error="response_incomplete", detail=None)
            return result
        text = _responses_text(obj)
    else:
        text = _chat_text(obj)
    usage = obj.get("usage") if isinstance(obj.get("usage"), dict) else None
    warnings = []
    tokens, shape = _tokens_from_usage(usage) if usage is not None else (None, None)
    cost = _usable_cost(usage.get("cost")) if (usage is not None and "cost" in usage) else None
    cost_present = usage is not None and "cost" in usage
    if text is None:
        # A 200 with no genuine generated text is a failure, not a pass: a token
        # count (or an empty/renamed choices field) is not a generation.
        result.update(ok=False, error="no_generation", detail=None,
                      generated_chars=0)
        result["_warnings"] = warnings
        return result
    result.update(ok=True, error=None, detail=None, generated_chars=len(text),
                  **_usage_report(usage is not None, tokens, shape, cost, cost_present, warnings))
    result["_warnings"] = warnings
    return result


def _check_stream(ctx, protocol):
    def make_sink(content_type):
        if content_type.startswith("text/event-stream"):
            return SSEParser(protocol)
        return _Accumulator()

    out = _exchange(
        ctx, "POST", ctx.base_path + _endpoint(protocol),
        _request_body(protocol, ctx.model, stream=True), make_sink=make_sink,
    )
    result = {
        "status": out["status"],
        "latency_ms": out["latency_ms"],
        "bytes_read": out["bytes_read"],
        "truncated": out["truncated"],
    }
    if out["status"] != 200:
        # A non-200 body went to an _Accumulator, never the SSE parser, and no
        # provider detail is surfaced.
        result.update(ok=False, error="http_status", detail=None)
        return result
    if not out["content_type"].startswith("text/event-stream"):
        result.update(ok=False, error="not_event_stream", detail=None)
        return result
    parser = out["sink"]
    try:
        parser.finish()
    except SmokeError as exc:
        result.update(ok=False, error=exc.code, detail=exc.detail)
        return result
    if out["truncated"]:
        result.update(ok=False, error="response_too_large", detail=None)
        return result
    if parser.stream_error:
        result.update(ok=False, error="stream_error", detail=None)
        return result
    if protocol == RESPONSES:
        if parser.failed:
            result.update(ok=False, error="response_failed", detail=None)
            return result
        if parser.incomplete:
            result.update(ok=False, error="response_incomplete", detail=None)
            return result
        if not parser.completed:
            result.update(ok=False, error="missing_terminal", detail=None,
                          sse={"events": parser.events, "completed": False})
            return result
        sse_summary = {"events": parser.events, "completed": True}
    else:
        if not parser.done:
            result.update(ok=False, error="missing_done", detail=None,
                          sse={"events": parser.events, "done": False})
            return result
        sse_summary = {"events": parser.events, "done": True}
    if not parser.text_seen:
        # The stream terminated, but produced no genuine text: a lone [DONE] or
        # a completed event with no output is not a generation.
        result.update(ok=False, error="no_generation", detail=None, sse=sse_summary)
        return result
    warnings = []
    result.update(
        ok=True, error=None, detail=None,
        sse=sse_summary,
        generated_chars=parser.text_chars,
        **_usage_report(parser.usage_seen, parser.tokens, parser.usage_shape,
                        parser.cost, parser.cost_present, warnings),
    )
    result["_warnings"] = warnings
    return result


def check_chat_nonstream(ctx):
    return _check_nonstream(ctx, CHAT)


def check_chat_stream(ctx):
    return _check_stream(ctx, CHAT)


def check_responses_nonstream(ctx):
    return _check_nonstream(ctx, RESPONSES)


def check_responses_stream(ctx):
    return _check_stream(ctx, RESPONSES)


# --------------------------------------------------------------------------
# Runner
# --------------------------------------------------------------------------


class _WallClockBudget:
    """Hard wall-clock bound for one check.

    Arms ``SIGALRM`` (main thread only) so that a blocking socket operation --
    a stalled TLS handshake, a header stall, or a body that trickles bytes
    within the per-read window -- is interrupted and turned into
    ``overall_timeout``. It cannot interrupt DNS resolution: the Python-level
    handler runs only after the blocking ``getaddrinfo`` C call returns (and
    the resolver retries when interrupted), so a hung lookup is bounded only by
    the system resolver's own timeouts. Where the alarm is unavailable
    (non-main thread, no SIGALRM) the caller still relies on the clamped socket
    timeouts and in-loop deadline checks. The DNS gap is documented in
    PROVIDER-ACCEPTANCE.md.

    A supervisor child process was considered and rejected: the checks are
    exercised in-process by hermetic tests that monkeypatch ``_open_connection``
    and ``_read_key_file`` and assert a zero-network contract, which a forked
    child would break; and the alarm already provides the hard bound. The key is
    only ever read in-process from the operator-supplied key *file* path -- it is
    never placed on argv or written to stdout.
    """

    def __init__(self, seconds):
        self._seconds = seconds
        self._armed = False
        self._previous = None

    @staticmethod
    def _supported():
        return (
            hasattr(signal, "SIGALRM")
            and hasattr(signal, "setitimer")
            and threading.current_thread() is threading.main_thread()
        )

    def __enter__(self):
        if not self._supported():
            return self
        def _fire(signum, frame):
            raise SmokeError("overall_timeout")
        self._previous = signal.signal(signal.SIGALRM, _fire)
        signal.setitimer(signal.ITIMER_REAL, self._seconds)
        self._armed = True
        return self

    def __exit__(self, *exc):
        if self._armed:
            try:
                signal.setitimer(signal.ITIMER_REAL, 0)
            finally:
                if self._previous is not None:
                    signal.signal(signal.SIGALRM, self._previous)
                self._armed = False
        return False


def _run_check(name, fn, ctx):
    start = time.monotonic()
    remaining = ctx.deadline - start
    if remaining <= 0:
        result = {"ok": False, "error": "overall_timeout", "detail": None}
    else:
        try:
            with _WallClockBudget(remaining):
                result = fn(ctx)
        except SmokeError as exc:
            result = {"ok": False, "error": exc.code, "detail": exc.detail}
    result["name"] = name
    result.setdefault("ok", True)
    result.setdefault("error", None)
    result.setdefault("detail", None)
    result.setdefault("latency_ms", int((time.monotonic() - start) * 1000))
    result.setdefault("bytes_read", 0)
    result.setdefault("truncated", False)
    result.setdefault("status", None)
    return result


def run(args):
    """Validate arguments and (only with ``--live``) perform the checks. Raises
    ``SmokeError`` for argument/consent/policy/key/protocol errors before any
    network call; per-check failures are returned in the report instead."""
    connect_timeout = _positive_seconds(args.connect_timeout, "connect-timeout")
    read_timeout = _positive_seconds(args.read_timeout, "read-timeout")
    overall_timeout = _positive_seconds(args.timeout, "timeout")

    protocol = getattr(args, "protocol", CHAT) or CHAT
    if protocol not in _PROTOCOLS:
        raise SmokeError("bad_protocol")

    if not args.live:
        raise SmokeError("live_required")
    if not args.url:
        raise SmokeError("missing_url")
    if not args.model:
        raise SmokeError("missing_model")
    if not _valid_model(args.model):
        raise SmokeError("bad_model")
    if not args.key_file:
        raise SmokeError("missing_key_file")

    target = guard_url(args.url, args.allow_insecure_http, args.cafile)
    key = _read_key_file(args.key_file)
    deadline = time.monotonic() + overall_timeout
    ctx = _Ctx(target, key, args.model, connect_timeout, read_timeout, deadline)

    checks = []
    if args.check_models:
        checks.append(_run_check("models", check_models, ctx))
    if protocol == RESPONSES:
        checks.append(_run_check("responses_nonstream", check_responses_nonstream, ctx))
        checks.append(_run_check("responses_stream", check_responses_stream, ctx))
    else:
        checks.append(_run_check("chat_nonstream", check_chat_nonstream, ctx))
        checks.append(_run_check("chat_stream", check_chat_stream, ctx))

    warnings = []
    for check in checks:
        for item in check.pop("_warnings", []):
            warnings.append(f"{check['name']}: {item}")
    errors = [f"{check['name']}: {check['error']}" for check in checks if not check["ok"]]

    return {
        "tool": TOOL_NAME,
        "schema": SCHEMA_VERSION,
        "ok": not errors,
        "live": True,
        "protocol": protocol,
        "model": ctx.model,
        "max_tokens": MAX_TOKENS,
        "prompt_bytes": len(SYNTHETIC_PROMPT.encode("utf-8")),
        "target": {"scheme": target.scheme, "host": target.host, "port": target.port,
                   "loopback": target.loopback},
        "checks": checks,
        "warnings": warnings,
        "errors": errors,
    }


# --------------------------------------------------------------------------
# CLI
# --------------------------------------------------------------------------


def _build_parser():
    parser = argparse.ArgumentParser(
        prog=TOOL_NAME,
        description=(
            "Opt-in live acceptance smoke test for a LocalRouter endpoint. "
            "Makes NO network call unless --live is given."
        ),
        epilog=(
            "Live requests spend provider quota, money and network egress. "
            "Use only against an isolated LocalRouter you control. "
            "See docs/PROVIDER-ACCEPTANCE.md."
        ),
    )
    parser.add_argument("--url", default="", help="LocalRouter base URL, e.g. https://router.example.com:8787")
    parser.add_argument("--key-file", dest="key_file", default="",
                        help="path to a client key FILE (never pass a key on the command line)")
    parser.add_argument("--model", default="", help="a model id routed by this LocalRouter")
    parser.add_argument("--protocol", choices=_PROTOCOLS, default=CHAT,
                        help="provider API to exercise: chat (chat-completions, default) "
                             "or responses (the Codex Responses API)")
    parser.add_argument("--live", action="store_true",
                        help="acknowledge real provider requests that spend quota/money/egress")
    parser.add_argument("--allow-insecure-http", dest="allow_insecure_http", action="store_true",
                        help="permit non-loopback plain HTTP (sends the key in cleartext)")
    parser.add_argument("--cafile", default="", help="PEM CA bundle for private-CA HTTPS")
    parser.add_argument("--connect-timeout", dest="connect_timeout", default="5",
                        help="TCP+TLS connect timeout in seconds (default 5)")
    parser.add_argument("--read-timeout", dest="read_timeout", default="30",
                        help="per-read stall timeout in seconds (default 30)")
    parser.add_argument("--timeout", default="90", help="overall execution timeout in seconds (default 90)")
    parser.add_argument("--check-models", dest="check_models", action="store_true",
                        help="also GET /v1/models to confirm the model is routed")
    parser.add_argument("--pretty", action="store_true", help="pretty-print the JSON report")
    return parser


def _emit(report, pretty, stream):
    stream.write(json.dumps(report, indent=2 if pretty else None, sort_keys=True) + "\n")


def _emit_error(stderr, code, detail, live):
    obj = {"tool": TOOL_NAME, "schema": SCHEMA_VERSION, "ok": False, "live": bool(live), "error": code}
    if detail:
        obj["detail"] = detail
    stderr.write(json.dumps(obj, sort_keys=True) + "\n")


def main(argv=None, stdout=None, stderr=None):
    argv = list(sys.argv[1:] if argv is None else argv)
    stdout = sys.stdout if stdout is None else stdout
    stderr = sys.stderr if stderr is None else stderr

    parser = _build_parser()
    if not argv:
        parser.print_help(stderr)
        return 2
    try:
        args = parser.parse_args(argv)
    except SystemExit as exc:
        code = exc.code
        return code if isinstance(code, int) else 2

    try:
        report = run(args)
    except SmokeError as exc:
        _emit_error(stderr, exc.code, exc.detail, args.live)
        return 2

    _emit(report, args.pretty, stdout)
    return 0 if report["ok"] else 1


if __name__ == "__main__":
    sys.exit(main())
