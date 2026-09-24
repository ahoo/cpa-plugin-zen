#!/usr/bin/env python3
"""Assert Host-facing exact-image protocol semantics using only private-network mock URLs."""
import argparse
import datetime
import json
import re
import sys
from pathlib import Path
import time
import urllib.error
import urllib.request

HOST = "http://host:8317"
MOCK = "http://mock:9999"
POLICY_TEXT = "OpenCode's free tier can only be used from within OpenCode"
GEO_TEXT = "This model is not available in your country"
GEO_MESSAGES = {"offline-geo-chat": "Error from provider (Console): " + GEO_TEXT,
                "offline-geo-responses": "Error from provider (Console): " + GEO_TEXT,
                "offline-geo-mixed": "Error from provider (Console): " + GEO_TEXT.upper()}
MIMO_UPSTREAM = "mimo-v2.6-flash-free"
MIMO_PRIVATE_SENTINEL = "private-test-value"  # Synthetic, never copied from a real upstream.
TAIL_STRUCTURE_KEYS = {"complete", "done_count", "tail_frames", "strict_cost_ping_count",
                       "duplicate_done_count", "invalid_json_count", "non_object_count",
                       "duplicate_top_keys_count", "typed_invalid_count", "extra_fields_count",
                       "output_fields_count", "usage_fields_count", "error_fields_count", "other_type_count"}
OPENER = urllib.request.build_opener(urllib.request.ProxyHandler({}))
TOOL = {"name": "probe", "description": "Synthetic tool; never invoked", "input_schema": {"type": "object", "properties": {"x": {"type": "string"}}}}


def request(base, path, payload=None):
    headers = {"Accept": "application/json", "Content-Type": "application/json"}
    if base == HOST:
        headers["Authorization"] = "Bearer offline-client-only"
    body = None if payload is None else json.dumps(payload, ensure_ascii=False).encode()
    req = urllib.request.Request(base + path, data=body, headers=headers,
                                 method="POST" if body is not None else "GET")
    try:
        with OPENER.open(req, timeout=20) as resp:
            return resp.status, dict(resp.headers), resp.read().decode("utf-8")
    except urllib.error.HTTPError as exc:
        return exc.code, dict(exc.headers), exc.read().decode("utf-8")


def assert_that(ok, detail):
    if not ok:
        raise AssertionError(detail)


def calls():
    code, _, raw = request(MOCK, "/__calls")
    assert_that(code == 200, f"mock telemetry {code}")
    return json.loads(raw)


def proxy_calls():
    code, _, raw = request(MOCK, "/__proxy_calls")
    assert_that(code == 200, f"mock proxy telemetry {code}")
    return json.loads(raw)


def clear():
    code, _, _ = request(MOCK, "/__reset", {})
    assert_that(code == 200, f"mock reset {code}")


def stream_events(raw):
    assert_that("data: data:" not in raw and "data:  data:" not in raw, "double SSE data prefix")
    assert_that("�" not in raw, "UTF-8 replacement found")
    result, name, data = [], "", []
    for line in raw.splitlines() + [""]:
        if line.startswith("data:"):
            value = line[5:].strip()
            assert_that(not value.startswith("data:"), "nested SSE data prefix")
            data.append(value)
        elif line.startswith("event:"):
            name = line[6:].strip()
        elif line.startswith(":"):
            continue
        elif line.strip() == "":
            if data:
                value = "\n".join(data)
                result.append((name, "[DONE]" if value == "[DONE]" else json.loads(value)))
            name, data = "", []
        else:
            raise AssertionError(f"invalid SSE framing: {line[:80]!r}")
    assert_that(len(result) >= 2, "stream must contain data and one terminal event")
    return result


def payload_for(route, model, stream, tool=False, prompt="offline protocol test"):
    if route == "chat":
        body = {"model": model, "messages": [{"role": "user", "content": prompt}], "max_tokens": 64, "stream": stream}
        if tool:
            body["tools"] = [{"type": "function", "function": {"name": "probe", "parameters": TOOL["input_schema"]}}]
        if stream:
            body["stream_options"] = {"include_usage": True}
        return "/v1/chat/completions", body
    if route == "responses":
        body = {"model": model, "input": prompt, "max_output_tokens": 64, "stream": stream}
        if tool:
            body["tools"] = [{"type": "function", "name": "probe", "parameters": TOOL["input_schema"]}]
        return "/v1/responses", body
    body = {"model": model, "messages": [{"role": "user", "content": prompt}], "max_tokens": 64, "stream": stream}
    if tool:
        body["tools"] = [TOOL]
    return "/v1/messages", body


def output(route, data, stream):
    if not stream:
        if route == "chat":
            choice = data["choices"][0]
            message = choice["message"]
            return (message.get("content") or ""), message.get("tool_calls") or [], data.get("usage") or {}
        if route == "responses":
            out = data["output"]
            text = "".join(part.get("text", "") for item in out for part in item.get("content", []) if part.get("type") == "output_text")
            tools = [item for item in out if item.get("type") == "function_call"]
            return text, tools, data.get("usage") or {}
        text = "".join(block.get("text", "") for block in data["content"] if block.get("type") == "text")
        tools = [block for block in data["content"] if block.get("type") == "tool_use"]
        return text, tools, data.get("usage") or {}
    events = stream_events(data)
    finish = {"chat": lambda e, d: d == "[DONE]",
              "responses": lambda e, d: e == "response.completed" or isinstance(d, dict) and d.get("type") == "response.completed",
              "messages": lambda e, d: e == "message_stop" or isinstance(d, dict) and d.get("type") == "message_stop"}[route]
    assert_that(sum(bool(finish(e, d)) for e, d in events) == 1, f"{route} requires exactly one success terminal: {events[-3:]}")
    text, tools, usage = "", [], {}
    if route == "chat":
        for _, ev in events:
            if not isinstance(ev, dict):
                continue
            for choice in ev.get("choices") or []:
                delta = choice.get("delta") or {}
                text += delta.get("content") or ""
                tools.extend(delta.get("tool_calls") or [])
            usage = ev.get("usage") or usage
    elif route == "responses":
        seen_deltas = set()
        for event, ev in events:
            if not isinstance(ev, dict):
                continue
            typ = ev.get("type", event)
            if typ == "response.output_text.delta":
                text += ev.get("delta") or ""
                seen_deltas.add((ev.get("output_index"), ev.get("content_index")))
            elif typ == "response.output_text.done" and (ev.get("output_index"), ev.get("content_index")) not in seen_deltas:
                text += ev.get("text") or ""
            if typ in ("response.output_item.added", "response.output_item.done") and (ev.get("item") or {}).get("type") == "function_call":
                tools.append(ev["item"])
            if typ == "response.completed":
                usage = (ev.get("response") or {}).get("usage") or usage
    else:
        for event, ev in events:
            if not isinstance(ev, dict):
                continue
            typ = ev.get("type", event)
            if typ == "content_block_delta":
                delta = ev.get("delta") or {}
                if delta.get("type") == "text_delta":
                    text += delta.get("text") or ""
                elif delta.get("type") == "input_json_delta":
                    tools.append(delta)
            elif typ == "content_block_start" and (ev.get("content_block") or {}).get("type") == "tool_use":
                tools.append(ev["content_block"])
            if typ in ("message_start", "message_delta"):
                usage.update(ev.get("usage") or (ev.get("message") or {}).get("usage") or {})
    return text, tools, usage


def check_tool(route, model, stream, chunks):
    expected_id = "call_mock" if model == "offline-chat-tool" else "fc_mock"
    if route == "chat":
        if stream:
            indices = {chunk.get("index") for chunk in chunks}
            assert_that(indices == {0}, f"chat tool index mismatch {indices}")
            ids = [chunk.get("id") for chunk in chunks if chunk.get("id")]
            names = [(chunk.get("function") or {}).get("name") for chunk in chunks if (chunk.get("function") or {}).get("name")]
            arguments = "".join((chunk.get("function") or {}).get("arguments") or "" for chunk in chunks)
            assert_that(ids == [expected_id] and names == ["probe"], f"chat tool identity lost: {chunks}")
        else:
            assert_that(len(chunks) == 1, f"chat tool count: {chunks}")
            item = chunks[0]
            ids, names = [item.get("id")], [(item.get("function") or {}).get("name")]
            arguments = (item.get("function") or {}).get("arguments")
    elif route == "responses":
        # Host emits added + done; the done item must carry the entire
        # argument string and the original call_id, distinct from item_id.
        completed = [item for item in chunks if item.get("status") == "completed"] if stream else chunks
        assert_that(len(completed) == 1, f"Responses tool completion count: {chunks}")
        item = completed[0]
        ids, names = [item.get("call_id")], [item.get("name")]
        arguments = item.get("arguments")
    else:
        if stream:
            starts = [item for item in chunks if item.get("type") == "tool_use"]
            assert_that(len(starts) == 1, f"Messages tool start count: {chunks}")
            item = starts[0]
            arguments = "".join(part.get("partial_json") or "" for part in chunks if part.get("type") == "input_json_delta")
            if not arguments:
                arguments = item.get("input")
        else:
            assert_that(len(chunks) == 1, f"Messages tool count: {chunks}")
            item = chunks[0]
            arguments = item.get("input")
        ids, names = [item.get("id")], [item.get("name")]
    assert_that(ids == [expected_id] and names == ["probe"], f"{route} tool id/name mismatch: {chunks}")
    parsed = json.loads(arguments) if isinstance(arguments, str) else arguments
    assert_that(parsed == {"x": "ok"}, f"{route} tool arguments mismatch: {parsed}")


def case(route, model, stream, text=None, tool=False, upstream=None, error=False, fallback=False, prompt=None):
    clear()
    path, body = payload_for(route, model, stream, tool, prompt or "offline protocol test")
    status, headers, raw = request(HOST, path, body)
    recorded = calls()
    assert_that(all(c["auth_ok"] and c["method"] == "POST" for c in recorded), f"unauthorized upstream: {recorded}")
    if upstream:
        assert_that(any(c["path"] == upstream for c in recorded), f"upstream path {upstream} absent: {recorded}")
    count = sum(c["model"] == "offline-fallback" for c in recorded)
    assert_that(count == (1 if fallback else 0), f"fallback must run exactly {int(fallback)} time(s), got {count}: {recorded}")
    if error:
        if stream and status == 200:
            events = stream_events(raw)
            assert_that(any(e == "error" or isinstance(d, dict) and d.get("type") in ("error", "response.failed") for e, d in events),
                        f"upstream failure lost in stream: {events[-3:]}")
            assert_that(not any(e in ("message_stop", "response.completed") or d == "[DONE]" for e, d in events), "upstream failure became success")
        else:
            assert_that(status >= 400, f"upstream error turned into status {status}: {raw[:180]}")
        print(f"PASS {route:9s} {'S' if stream else 'N'} {model:22s} expected-error upstream={len(recorded)}")
        return
    assert_that(status == 200, f"{route} {model} stream={stream}: HTTP {status} {raw[:260]!r}")
    content_type = next((value.lower() for key, value in headers.items() if key.lower() == "content-type"), "")
    expected_type = "text/event-stream" if stream else "application/json"
    assert_that(content_type.startswith(expected_type), f"{route} {model} stream={stream}: wrong Content-Type {content_type!r}")
    actual, tools, usage = output(route, raw if stream else json.loads(raw), stream)
    if text is not None:
        assert_that(actual == text, f"{route} {model} stream={stream}: text {actual!r}, wanted {text!r}")
    if tool:
        check_tool(route, model, stream, tools)
    a = usage.get("prompt_tokens", usage.get("input_tokens"))
    b = usage.get("completion_tokens", usage.get("output_tokens"))
    assert_that((a, b) == (7, 3), f"{route} {model} stream={stream}: incorrect usage {usage}; synthetic response tail={raw[-1800:]!r}")
    if fallback:
        assert_that(len(recorded) == 2 and recorded[0]["model"] == "offline-fail", f"unexpected extra upstream attempts: {recorded}")
    if model in ("offline-paid", "jev"):
        expected_model = "offline-jev" if model == "jev" else "offline-systemone"
        assert_that(recorded == [{"path": "/v1/systemone", "model": expected_model, "method": "POST", "stream": False,
                                  "session": "", "auth_ok": True}], f"paid SystemOne route changed: {recorded}")
    if model == "offline-free-systemone":
        assert_that(recorded == [{"path": "/v1/systemone", "model": "offline-free-systemone", "method": "POST", "stream": False,
                                  "session": "", "auth_ok": True}], f"free SystemOne must not need a session: {recorded}")
    print(f"PASS {route:9s} {'S' if stream else 'N'} {model:22s} {'tools' if tool else 'text'} upstream={len(recorded)}")
    return raw


def ready():
    required = ("zen/offline-systemone", "zen/offline-jev", "zen/offline-chat", "zen/offline-responses",
                "zen/offline-free-systemone", "zen/offline-policy", "zen/offline-tail-ping",
                "zen/offline-tail-extra-delta", "zen/offline-geo-chat", "zen/offline-geo-responses",
                "zen/offline-chat-tail-other")
    # The pinned Host may hide the canonical mimo-free alias in /v1/models;
    # each fresh Mimo phase below MUST still route it to the authenticated
    # synthetic Chat mock and meet exact status/one-shot assertions.
    last = "no response"
    for _ in range(50):
        try:
            a, _, _ = request(MOCK, "/__health")
            status, _, raw = request(HOST, "/v1/models")
            if a == 200 and status == 200 and all(model in raw for model in required):
                print("[offline] mock and Host ready; synthetic models registered")
                return
            last = f"mock={a}, Host={status}, missing={[model for model in required if model not in raw]}, models={raw[:320]!r}"
        except (OSError, ValueError) as exc:
            last = f"{type(exc).__name__}: {exc}"
        time.sleep(0.5)
    raise AssertionError(f"Host did not register synthetic offline models within 25s ({last})")


def test():
    suites = (
        ("offline-paid", dict(text='{"q1":"PONG"}', upstream="/v1/systemone")),
        ("offline-chat", dict(text="Hi世界🙂", upstream="/v1/chat/completions")),
        ("offline-chat-tool", dict(tool=True, upstream="/v1/chat/completions")),
        ("offline-responses", dict(text="Hi世界🙂", upstream="/v1/responses")),
        ("offline-responses-done", dict(text="ONLY🙂", upstream="/v1/responses")),
        ("offline-responses-tool", dict(tool=True, upstream="/v1/responses")),
        ("offline-fail", dict(text="FALLBACK🙂", fallback=True, upstream="/v1/chat/completions")),
        ("offline-bad", dict(error=True, upstream="/v1/chat/completions")),
        ("offline-event-error", dict(error=True, fallback=True,
                                     upstream="/v1/responses", prompt="UNRECOVERABLE synthetic stream failure")),
    )
    passed, failures = 0, []
    for route in ("chat", "responses", "messages"):
        for stream in (False, True):
            for model, options in suites:
                label = f"{route} {'S' if stream else 'N'} {model}"
                try:
                    case(route, model, stream, **options)
                except Exception as exc:
                    print(f"FAIL {label}: {type(exc).__name__}: {exc}", file=sys.stderr, flush=True)
                    failures.append(label)
                else:
                    passed += 1
                if model == "offline-fail":
                    # One free 503 intentionally cools the sole synthetic
                    # member/session. Don't contaminate the next case: wait
                    # out config.yaml's real one-second cooldown.
                    time.sleep(1.25)
    total = 3 * 2 * len(suites)
    assert_that(passed + len(failures) == total, "case accounting mismatch")
    print(f"[offline] exact Host matrix: PASS={passed} FAIL={len(failures)} TOTAL={total}", flush=True)
    assert_that(not failures, f"{len(failures)} failing cases: {', '.join(failures)}")


def local_503(route, model, stream, reason, earliest="unknown"):
    clear()
    path, body = payload_for(route, model, stream)
    status, _, raw = request(HOST, path, body)
    assert_that(status == 503, f"{reason} {route} stream={stream} {model}: expected HTTP 503, got {status} {raw[:240]!r}")
    match = re.search(r"earliest_local_retry_at=(unknown|\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d+)?Z)\b", raw)
    assert_that(match is not None, f"{reason} missing local retry reason (Host cache may mask plugin): {raw[:260]!r}")
    retry = match.group(1)
    if earliest == "unknown":
        assert_that(retry == "unknown", f"{reason} should have unknown local retry, got {retry!r}")
    else:
        assert_that(retry != "unknown", f"{reason} must report a UTC retry timestamp")
        parsed = datetime.datetime.fromisoformat(retry.replace("Z", "+00:00"))
        assert_that(parsed.tzinfo == datetime.timezone.utc, f"{reason} retry must be UTC: {retry!r}")
    backend, proxy = calls(), proxy_calls()
    assert_that(not backend and not proxy, f"{reason} made upstream requests: backend={backend} proxy={proxy}")
    print(f"PASS {reason:16s} {route:9s} {'S' if stream else 'N'} {model:22s} HTTP 503 upstream=0")
    return retry


def check(failures, label, fn, *args, **kwargs):
    try:
        return fn(*args, **kwargs)
    except Exception as exc:
        print(f"FAIL {label}: {type(exc).__name__}: {exc}", file=sys.stderr, flush=True)
        failures.append(label)
        return None


def test_no_session(reason):
    failures = []
    for route in ("chat", "responses", "messages"):
        for stream in (False, True):
            for model in ("offline-chat", "offline-responses"):
                check(failures, f"{reason} {route} {stream} {model}", local_503, route, model, stream, reason)
    check(failures, f"{reason} free SystemOne", case, "chat", "offline-free-systemone", False,
          text='{"q1":"PONG"}', upstream="/v1/systemone")
    if reason == "proxy-exhausted":
        check(failures, "proxy free SystemOne", assert_that,
              proxy_calls() == [{"target": "http://mock:9999/v1/systemone", "method": "POST"}],
              "free SystemOne did not exercise member.proxy_url")
    check(failures, f"{reason} paid jev", case, "chat", "jev", False,
          text='{"q1":"PONG"}', upstream="/v1/systemone")
    assert_that(not failures, f"{reason}: {len(failures)} failing isolated checks: {', '.join(failures)}")
    print(f"[offline] {reason}: both free endpoints locally 503, free SystemOne/paid jev remain usable")


def test_no_member():
    failures = []
    for route in ("chat", "responses", "messages"):
        for stream in (False, True):
            for model in ("offline-chat", "offline-responses", "offline-free-systemone"):
                check(failures, f"no-member {route} {stream} {model}", local_503, route, model, stream, "no-member")
    check(failures, "no-member paid jev", case, "chat", "jev", False,
          text='{"q1":"PONG"}', upstream="/v1/systemone")
    assert_that(not failures, f"no-member: {len(failures)} failing isolated checks: {', '.join(failures)}")
    print("[offline] disabled free member: HTTP 503 without any upstream traffic")


def policy_rejection(route, stream):
    clear()
    path, body = payload_for(route, "offline-policy", stream)
    status, _, raw = request(HOST, path, body)
    backend, proxy = calls(), proxy_calls()
    assert_that(status == 403 and POLICY_TEXT in raw,
                f"policy {route} stream={stream}: expected HTTP 403 with canonical wording, got {status}: {raw[:240]!r}")
    assert_that(backend == [{"path": "/v1/chat/completions", "model": "offline-policy", "method": "POST",
                             "stream": True, "session": "offline-session-only", "auth_ok": True}] and not proxy,
                f"policy must stop after exactly one free request (no paid or proxy): {backend}, {proxy}")
    print(f"PASS policy {route:9s} {'S' if stream else 'N'} HTTP 403 with one free request")


def paid_recovery(route, model, stream, first_free=None, first_path="/v1/chat/completions"):
    clear()
    path, body = payload_for(route, model, stream)
    status, headers, raw = request(HOST, path, body)
    recorded, proxy = calls(), proxy_calls()
    label = f"paid recovery {route} {model} stream={stream}"
    assert_that(status == 200, f"{label}: expected paid HTTP 200, got {status}: {raw[:240]!r}")
    # The pinned Host strips executor-only X-Zen-Fallback from downstream HTTP.
    # The native ABI probe asserts this marker on the executor response instead;
    # here assert real paid authorization/count and the translated output.
    marker = next((v for k, v in headers.items() if k.lower() == "x-zen-fallback"), "")
    if marker:
        assert_that(marker == "paid", f"{label}: unexpected fallback marker {marker!r}")
    expected = []
    if first_free:
        expected.append({"path": first_path, "model": first_free, "method": "POST", "stream": True,
                         "session": "offline-session-only", "auth_ok": True})
    expected.append({"path": "/v1/chat/completions", "model": "offline-fallback", "method": "POST", "stream": False,
                     "session": "", "auth_ok": True})
    assert_that(recorded == expected and not proxy, f"{label}: expected one authorized paid sequence: {recorded}, {proxy}")
    content_type = next((v.lower() for k, v in headers.items() if k.lower() == "content-type"), "")
    assert_that(content_type.startswith("text/event-stream" if stream else "application/json"),
                f"{label}: unexpected Content-Type {content_type!r}")
    text, tools, usage = output(route, raw if stream else json.loads(raw), stream)
    assert_that(text == "FALLBACK🙂" and not tools, f"{label}: wrong paid output: {text!r}, {tools}")
    assert_that((usage.get("prompt_tokens", usage.get("input_tokens")),
                 usage.get("completion_tokens", usage.get("output_tokens"))) == (7, 3), f"{label}: usage {usage}")
    print(f"PASS {label} paid_calls=1 free_calls={int(bool(first_free))}")


def test_policy(paid=False):
    failures = []
    for route in ("chat", "responses", "messages"):
        for stream in (False, True):
            if paid:
                check(failures, f"policy-paid {route} {stream}", paid_recovery,
                      route, "offline-policy", stream, first_free="offline-policy")
            else:
                check(failures, f"policy {route} {stream}", policy_rejection, route, stream)
            ordinary = "offline-responses" if route == "responses" else "offline-chat"
            check(failures, f"after-policy {route} {stream}", case, route, ordinary, stream,
                  text="Hi世界🙂", upstream="/v1/responses" if ordinary == "offline-responses" else "/v1/chat/completions")
            check(failures, f"after-policy calls {route} {stream}", assert_that,
                  len(calls()) == 1 and calls()[0]["model"] == ordinary,
                  f"policy rejection cooled a healthy member/session: {calls()}")
    assert_that(not failures, f"policy (paid={paid}): {len(failures)} failing checks: {', '.join(failures)}")


def error_message(raw):
    try:
        body = json.loads(raw)
    except ValueError as exc:
        raise AssertionError(f"expected JSON error envelope, got {raw[:220]!r}") from exc
    if isinstance(body, dict):
        for item in (body.get("error"), body):
            if isinstance(item, dict) and isinstance(item.get("message"), str):
                return item["message"]
    raise AssertionError(f"missing error.message in HTTP response: {raw[:220]!r}")


def synthetic_pool_snapshot():
    # This path exists ONLY in the geo phase's client container and is a
    # read-only directory bind of a freshly generated, never production pool.
    path = Path("/synthetic/zen-sessions.json")
    assert_that(path.is_file(), "synthetic pool snapshot unavailable")
    stat = path.stat()
    assert_that(not path.with_name(path.name + ".tmp").exists(), "synthetic pool left temporary file")
    return path.read_bytes(), stat.st_mtime_ns, stat.st_ino


def geo_rejection(route, model, stream):
    before = synthetic_pool_snapshot()
    clear()
    path, body = payload_for(route, model, stream)
    status, _, raw = request(HOST, path, body)
    label = f"country 403 {route} {model} stream={stream}"
    assert_that(status == 403, f"{label}: expected actual HTTP 403, got {status}: {raw[:240]!r}")
    message = error_message(raw)
    assert_that(GEO_MESSAGES[model] in message, f"{label}: original mixed-case country error body lost: {message!r}")
    assert_that("; tail_structure=" not in message and "; tail_structure_v2=" not in message,
                f"{label}: non-Mimo 403 must not reveal tail diagnostics")
    upstream = "/v1/responses" if model == "offline-geo-responses" else "/v1/chat/completions"
    expected = [{"path": upstream, "model": model, "method": "POST", "stream": True,
                 "session": "offline-session-only", "auth_ok": True}]
    assert_that(calls() == expected and not proxy_calls(), f"{label}: rotated, retried or fell back: {calls()}, {proxy_calls()}")
    assert_that(synthetic_pool_snapshot() == before, f"{label}: mutated synthetic session file")
    print(f"PASS {label} HTTP 403 free=1 paid=0 pool=unchanged")


def geo_paid_recovery(route, model, stream):
    before = synthetic_pool_snapshot()
    upstream = "/v1/responses" if model == "offline-geo-responses" else "/v1/chat/completions"
    paid_recovery(route, model, stream, first_free=model, first_path=upstream)
    assert_that(synthetic_pool_snapshot() == before, f"country paid fallback mutated synthetic session file: {model}")
    print(f"PASS country paid {route:9s} {'S' if stream else 'N'} {model} free=1 paid=1 pool=unchanged")


def test_geo(paid=False):
    failures = []
    for route in ("chat", "responses", "messages"):
        for stream in (False, True):
            for model in GEO_MESSAGES:
                fn = geo_paid_recovery if paid else geo_rejection
                check(failures, f"country paid={paid} {route} {stream} {model}", fn, route, model, stream)
                ordinary = "offline-responses" if model == "offline-geo-responses" else "offline-chat"
                upstream = "/v1/responses" if ordinary == "offline-responses" else "/v1/chat/completions"
                check(failures, f"after-country {route} {stream} {model}", case,
                      route, ordinary, stream, text="Hi世界🙂", upstream=upstream)
                check(failures, f"after-country calls {route} {stream} {model}", assert_that,
                      calls() == [{"path": upstream, "model": ordinary, "method": "POST", "stream": True,
                                   "session": "offline-session-only", "auth_ok": True}] and not proxy_calls(),
                      f"country denial cooled member/session or caused hidden paid/proxy calls: {calls()}, {proxy_calls()}")
    assert_that(not failures, f"country 403 (paid={paid}): {len(failures)} failing checks: {', '.join(failures)}")
    print(f"[offline] exact country 403 health-neutral, paid fallback={paid}; 3 routes × both modes")


def test_exhausted_paid():
    failures = []
    for route in ("chat", "responses", "messages"):
        for stream in (False, True):
            for model in ("offline-chat", "offline-responses"):
                check(failures, f"exhausted-paid {route} {stream} {model}", paid_recovery, route, model, stream)
    check(failures, "exhausted-paid free SystemOne", case, "chat", "offline-free-systemone", False,
          text='{"q1":"PONG"}', upstream="/v1/systemone")
    check(failures, "exhausted-paid jev", case, "chat", "jev", False,
          text='{"q1":"PONG"}', upstream="/v1/systemone")
    assert_that(not failures, f"exhausted-paid: {len(failures)} failing checks: {', '.join(failures)}")


def tail_success(route, model, stream):
    raw = case(route, model, stream, text="Hi世界🙂", tool=True, upstream="/v1/responses")
    recorded, proxy = calls(), proxy_calls()
    assert_that(recorded == [{"path": "/v1/responses", "model": model, "method": "POST", "stream": True,
                             "session": "offline-session-only", "auth_ok": True}] and not proxy,
                f"EOF cost ping retried or fell back: {recorded}, {proxy}")
    if stream:
        events = stream_events(raw)
        if route == "chat":
            uses = [ev["usage"] for _, ev in events if isinstance(ev, dict) and ev.get("usage")]
            assert_that(len(uses) == 1 and events[-1][1] == "[DONE]", f"duplicate chat usage/terminal: {events[-4:]}")
        elif route == "responses":
            completed = [ev for name, ev in events if isinstance(ev, dict) and
                         ev.get("type", name) == "response.completed"]
            usages = [(ev.get("response") or {}).get("usage") for _, ev in events if isinstance(ev, dict) and
                      (ev.get("response") or {}).get("usage")]
            assert_that(len(completed) == 1 and len(usages) == 1 and
                        (usages[0].get("input_tokens"), usages[0].get("output_tokens"),
                         usages[0].get("total_tokens")) == (7, 3, 10),
                        f"duplicate Responses usage/terminal: {events[-4:]}")
        else:
            final = [ev for name, ev in events if isinstance(ev, dict) and ev.get("type", name) == "message_delta"
                     and (ev.get("usage") or {}).get("output_tokens") == 3]
            assert_that(len(final) == 1, f"duplicate Messages output usage: {events[-4:]}")
    print(f"PASS tail EOF {route:9s} {'S' if stream else 'N'} {model} text/tool/usage once")


def tail_rejected(route, model, stream):
    clear()
    path, body = payload_for(route, model, stream, tool=True)
    status, _, raw = request(HOST, path, body)
    recorded, proxy = calls(), proxy_calls()
    assert_that(status == 502, f"{route} {model} stream={stream}: must return HTTP 502, got {status}: {raw[:260]!r}")
    assert_that(recorded == [{"path": "/v1/responses", "model": model, "method": "POST", "stream": True,
                             "session": "offline-session-only", "auth_ok": True}] and not proxy,
                f"invalid post-completion event retried, became cached or paid fallback: {recorded}, {proxy}")
    print(f"PASS tail reject {route:9s} {'S' if stream else 'N'} {model} HTTP 502 free=1 paid=0")


def test_tail():
    positives = ("offline-tail-ping", "offline-tail-null")
    negatives = ("offline-tail-extra-delta", "offline-tail-extra-response", "offline-tail-no-cost",
                 "offline-tail-unknown", "offline-tail-late-output", "offline-tail-late-error", "offline-tail-late-failed")
    failures = []
    for route in ("chat", "responses", "messages"):
        for stream in (False, True):
            for model in positives:
                check(failures, f"tail-success {route} {stream} {model}", tail_success, route, model, stream)
            for model in negatives:
                check(failures, f"tail-reject {route} {stream} {model}", tail_rejected, route, model, stream)
    assert_that(not failures, f"cost-ping tail: {len(failures)} failing checks: {', '.join(failures)}")
    print("[offline] EOF cost-ping positive and strict 502 negatives: 3 entrypoints × both output modes")


def check_legacy_tail_summary(summary):
    assert_that(isinstance(summary, dict) and set(summary) == TAIL_STRUCTURE_KEYS,
                f"Mimo legacy counters have missing/extra fields: {summary!r}")
    assert_that(summary["complete"] is True, f"Mimo tail analyzer did not finish: {summary!r}")
    counts = {key: value for key, value in summary.items() if key != "complete"}
    assert_that(all(type(value) is int and value >= 0 for value in counts.values()),
                f"Mimo tail counters must be nonnegative integers: {summary!r}")
    # The deliberately synthetic tail is ONE non-ping object with a pre-approved
    # structural choices field and an opaque cost field; no raw values may leak.
    expected = {"done_count": 1, "tail_frames": 1, "extra_fields_count": 1,
                "output_fields_count": 1, "other_type_count": 1}
    assert_that(all(counts[key] == expected.get(key, 0) for key in counts),
                f"Mimo post-DONE structural classification changed: {summary!r}")


def check_mimo_summary(message, version):
    assert_that(version in (1, 2), f"unknown diagnostic version {version}")
    marker = "; tail_structure_v2=" if version == 2 else "; tail_structure="
    other = "; tail_structure=" if version == 2 else "; tail_structure_v2="
    assert_that(message.count(marker) == 1 and other not in message,
                f"expected exactly one v{version} marker and no other diagnostic: {message[:260]!r}")
    preamble, raw = message.split(marker, 1)
    assert_that("data after SSE termination" in preamble, f"Mimo lost strict SSE HTTP 502 error: {preamble!r}")
    assert_that(0 < len((marker + raw).encode()) <= 1024,
                f"v{version} diagnostic marker+JSON exceeds 1024 bytes: {len((marker + raw).encode())}")
    for private in (MIMO_PRIVATE_SENTINEL, "Hi世界🙂", "call_mock", '"x":"ok"'):
        assert_that(private not in message, f"v{version} diagnostic leaked synthetic content: {private}")
    decoded = json.loads(raw)  # Entire suffix must be ONE JSON object, with no trailing text.
    assert_that(isinstance(decoded, dict) and raw == json.dumps(decoded, separators=(",", ":"), ensure_ascii=False),
                f"v{version} diagnostic must be compact object JSON: {raw[:220]!r}")
    if version == 1:
        check_legacy_tail_summary(decoded)
        return decoded
    assert_that(set(decoded) == {"schema", "summary", "detail_complete", "frames"} and
                type(decoded["schema"]) is int and decoded["schema"] == 2 and decoded["detail_complete"] is True,
                f"v2 diagnostic envelope/schema changed: {decoded!r}")
    check_legacy_tail_summary(decoded["summary"])
    frames = decoded["frames"]
    assert_that(isinstance(frames, list) and len(frames) == 1 and isinstance(frames[0], dict) and
                set(frames[0]) == {"kind", "fields", "unknown_fields"} and
                type(frames[0]["unknown_fields"]) is int and frames[0] ==
                {"kind": "object", "fields": [["choices", "empty_array"], ["cost", "opaque"]], "unknown_fields": 0},
                f"v2 diagnostic structural tags/order changed or private cost leaked: {frames!r}")
    return decoded


def mimo_failure(route, stream, model="mimo-free", diagnostic_version=0):
    clear()
    path, body = payload_for(route, model, stream, tool=True)
    status, _, raw = request(HOST, path, body)
    upstream_model = MIMO_UPSTREAM if model == "mimo-free" else model
    backend, proxy = calls(), proxy_calls()
    label = f"Chat post-DONE {route} {model} stream={stream} diagnostic_v={diagnostic_version}"
    assert_that(status == 502, f"{label}: expected ACTUAL HTTP 502 (not HTTP 200 error event), got {status}: {raw[:240]!r}")
    assert_that(backend == [{"path": "/v1/chat/completions", "model": upstream_model, "method": "POST",
                             "stream": True, "session": "offline-session-only", "auth_ok": True}] and not proxy,
                f"{label}: retried, fell back or bypassed the free mock: {backend}, {proxy}")
    message = error_message(raw)
    if diagnostic_version:
        check_mimo_summary(message, diagnostic_version)
    else:
        assert_that("data after SSE termination" in message and
                    "; tail_structure=" not in message and "; tail_structure_v2=" not in message,
                    f"{label}: non-Mimo/one-shot error must be bare strict SSE 502: {message!r}")
    assert_that(all(private not in message for private in
                    (MIMO_PRIVATE_SENTINEL, "Hi世界🙂", "call_mock", '"x":"ok"')),
                f"{label}: synthetic cost/output/tool content leaked into HTTP error")
    assert_that(MIMO_PRIVATE_SENTINEL not in raw, f"{label}: raw synthetic cost value leaked into HTTP error")
    print(f"PASS {label} HTTP 502 free=1 paid=0")


def test_mimo_once(route, stream):
    # Fresh Host/Executor PER route×mode. A different Chat alias FIRST must
    # neither receive nor consume the Mimo-only diagnostic; then exactly the
    # first Mimo failure does. Later Mimo and other aliases stay bare 502.
    mimo_failure(route, stream, model="offline-chat-tail-other")
    mimo_failure(route, stream, diagnostic_version=2)
    mimo_failure(route, stream)
    mimo_failure(route, stream, model="offline-chat-tail-other")
    ordinary = case(route, "offline-chat", stream, text="Hi世界🙂", upstream="/v1/chat/completions")
    assert_that(len(calls()) == 1 and not proxy_calls(), f"Mimo parse failure cooled unrelated free Chat: {ordinary[-200:]!r}")
    print(f"[offline] one-shot Mimo diagnostic only: {route} {'S' if stream else 'N'}")


def test_proxy():
    for route, model, stream, target in (("chat", "offline-chat", False, "/v1/chat/completions"),
                                          ("responses", "offline-responses", True, "/v1/responses")):
        case(route, model, stream, text="Hi世界🙂", upstream=target)
        assert_that(proxy_calls() == [{"target": "http://mock:9999" + target, "method": "POST"}],
                    f"free member.proxy_url stdDoer path not observed: {proxy_calls()}")
        assert_that(len(calls()) == 1 and calls()[0]["model"] == model,
                    f"proxy did not reach allowlisted synthetic upstream: {calls()}")
    print("[offline] member.proxy_url used synthetic proxy, not Host callback only")


def test_hot_old():
    # Old 0.2.4 Executor sees the same synthetic Chat post-DONE fixture: one
    # v1 structural summary, then strict bare 502, without consuming it on
    # another Chat alias or changing Responses/paid routes.
    mimo_failure("chat", False, model="offline-chat-tail-other")
    mimo_failure("chat", False, diagnostic_version=1)
    mimo_failure("chat", True)
    tail_success("responses", "offline-tail-ping", False)
    case("chat", "jev", False, text='{"q1":"PONG"}', upstream="/v1/systemone")
    print("[offline] hot-switch old mapping: first Mimo v1, then bare strict HTTP 502")


def test_hot_new():
    # The fresh 0.2.5 Executor sees that IDENTICAL fixture and emits v2 only
    # once; it does not accept any post-DONE Chat event or affect other routes.
    mimo_failure("chat", False, model="offline-chat-tail-other")
    mimo_failure("chat", False, diagnostic_version=2)
    mimo_failure("chat", True)
    tail_success("responses", "offline-tail-ping", True)
    case("chat", "jev", False, text='{"q1":"PONG"}', upstream="/v1/systemone")
    print("[offline] hot-switch new mapping: first Mimo v2, then bare strict HTTP 502; paid jev independent")


def test_cooldown():
    clear()
    path, body = payload_for("chat", "offline-fail", False)
    status, _, raw = request(HOST, path, body)
    assert_that(status == 503, f"initial synthetic upstream 503 became HTTP {status}: {raw[:240]!r}")
    assert_that(calls() == [{"path": "/v1/chat/completions", "model": "offline-fail", "method": "POST",
                             "stream": True, "session": "offline-session-only", "auth_ok": True}] and not proxy_calls(),
                f"initial free 503 must not fallback or retry: {calls()}, {proxy_calls()}")
    retry = local_503("chat", "offline-chat", False, "cooling", earliest="utc")
    case("chat", "jev", False, text='{"q1":"PONG"}', upstream="/v1/systemone")
    until = datetime.datetime.fromisoformat(retry.replace("Z", "+00:00")).timestamp()
    wait = max(0, until - time.time() + 1.25)  # Session timestamps round down to seconds.
    assert_that(wait < 6, f"synthetic 3-second cooldown unexpectedly long: {retry}")
    time.sleep(wait)  # Let the real pool TTL elapse; never rewrite session/Host state.
    case("chat", "offline-chat", False, text="Hi世界🙂", upstream="/v1/chat/completions")
    assert_that(len(calls()) == 1 and calls()[0]["session"] == "offline-session-only",
                f"session did not recover naturally: {calls()}")
    print("[offline] session TTL expired; Host actual cache allowed same free model again")


if __name__ == "__main__":
    args = argparse.ArgumentParser()
    modes = ("ready", "test", "exhausted", "exhausted-paid", "bad", "empty", "missing", "proxy-exhausted",
             "no-member", "policy", "policy-paid", "geo", "geo-paid", "proxy", "cooldown", "tail",
             "hot-old", "hot-new") + tuple(f"mimo-{route}-{mode}" for route in ("chat", "responses", "messages")
                                      for mode in ("n", "s"))
    args.add_argument("--mode", required=True, choices=modes)
    a = args.parse_args()
    try:
        if a.mode == "ready":
            ready()
        elif a.mode == "test":
            test()
        elif a.mode in ("exhausted", "bad", "empty", "missing", "proxy-exhausted"):
            test_no_session(a.mode)
        elif a.mode == "exhausted-paid":
            test_exhausted_paid()
        elif a.mode == "policy-paid":
            test_policy(paid=True)
        elif a.mode in ("geo", "geo-paid"):
            test_geo(paid=a.mode == "geo-paid")
        elif a.mode.startswith("mimo-"):
            _, route, output_mode = a.mode.split("-")
            test_mimo_once(route, stream=output_mode == "s")
        else:
            {"no-member": test_no_member, "policy": test_policy, "proxy": test_proxy,
             "cooldown": test_cooldown, "tail": test_tail,
             "hot-old": test_hot_old, "hot-new": test_hot_new}[a.mode]()
    except Exception as exc:
        print(f"FAIL {type(exc).__name__}: {exc}", file=sys.stderr)
        sys.exit(1)
