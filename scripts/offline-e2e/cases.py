#!/usr/bin/env python3
"""Assert Host-facing exact-image protocol semantics using only private-network mock URLs."""
import argparse
import json
import sys
import time
import urllib.error
import urllib.request

HOST = "http://host:8317"
MOCK = "http://mock:9999"
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
    if model == "offline-paid":
        assert_that(recorded == [{"path": "/v1/systemone", "model": "offline-systemone", "method": "POST", "stream": False,
                                  "session": "", "auth_ok": True}], f"paid SystemOne route changed: {recorded}")
    print(f"PASS {route:9s} {'S' if stream else 'N'} {model:22s} {'tools' if tool else 'text'} upstream={len(recorded)}")


def ready():
    last = "no response"
    for _ in range(50):
        try:
            a, _, _ = request(MOCK, "/__health")
            status, _, raw = request(HOST, "/v1/models")
            if a == 200 and status == 200 and all(model in raw for model in ("zen/offline-systemone", "zen/offline-chat", "zen/offline-responses")):
                print("[offline] mock and Host ready; synthetic models registered")
                return
            last = f"mock={a}, Host={status}, models={raw[:320]!r}"
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


if __name__ == "__main__":
    args = argparse.ArgumentParser()
    args.add_argument("--mode", required=True, choices=("ready", "test"))
    a = args.parse_args()
    try:
        ready() if a.mode == "ready" else test()
    except Exception as exc:
        print(f"FAIL {type(exc).__name__}: {exc}", file=sys.stderr)
        sys.exit(1)
