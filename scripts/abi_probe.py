#!/usr/bin/env python3
"""Offline native ABI boundary probes. Usage: python3 scripts/abi_probe.py /tmp/zen.so.

Uses synthetic callbacks only; never reads configuration or contacts an upstream.
Run in a subprocess: invalid pointer probes intentionally make unsafe reads fatal.
"""
import base64
import ctypes as C
import json
import os
import sys
import tempfile
import threading


class Buffer(C.Structure):
    _fields_ = [("ptr", C.c_void_p), ("length", C.c_size_t)]


CALL = C.CFUNCTYPE(C.c_int, C.c_void_p, C.c_char_p, C.c_void_p, C.c_size_t, C.POINTER(Buffer))
FREE = C.CFUNCTYPE(None, C.c_void_p, C.c_size_t)


class Host(C.Structure):
    _fields_ = [("version", C.c_uint32), ("ctx", C.c_void_p), ("call", CALL), ("free", FREE)]


class Plugin(C.Structure):
    _fields_ = [("version", C.c_uint32), ("call", C.c_void_p), ("free", C.c_void_p), ("shutdown", C.c_void_p)]


TAIL_POSITIVES = ("ping", "null")
TAIL_NEGATIVES = ("extra-delta", "extra-response", "no-cost", "unknown",
                  "late-output", "late-error", "late-failed")
GEO_TEXT = "This model is not available in your country"
MIMO_UPSTREAM = "mimo-v2.6-flash-free"
MIMO_PRIVATE_SENTINEL = "private-test-value"  # Synthetic, never copied from a real upstream.
TAIL_STRUCTURE_KEYS = {"complete", "done_count", "tail_frames", "strict_cost_ping_count",
                       "duplicate_done_count", "invalid_json_count", "non_object_count",
                       "duplicate_top_keys_count", "typed_invalid_count", "extra_fields_count",
                       "output_fields_count", "usage_fields_count", "error_fields_count", "other_type_count"}


def chat_post_done():
    """Valid Chat text/tool/usage/finish, [DONE], then synthetic choices+cost."""
    events = [
        {"choices": [{"index": 0, "delta": {"role": "assistant", "content": "Hi", "tool_calls": [
            {"index": 0, "id": "call_mock", "type": "function", "function": {"name": "probe", "arguments": ""}}]}}]},
        {"choices": [{"index": 0, "delta": {"content": "世界🙂", "tool_calls": [
            {"index": 0, "function": {"arguments": '{"x":'}}]}}]},
        {"choices": [{"index": 0, "delta": {"tool_calls": [
            {"index": 0, "function": {"arguments": '"ok"}'}}]}}]},
        {"choices": [{"index": 0, "delta": {}, "finish_reason": "tool_calls"}]},
        {"choices": [], "usage": {"prompt_tokens": 7, "completion_tokens": 3, "total_tokens": 10}},
    ]
    def frame(event):
        return b"data: " + json.dumps(event, ensure_ascii=False).encode() + b"\n\n"
    return b"".join(frame(event) for event in events) + b"data: [DONE]\n\n" + frame({"choices": [], "cost": {"sentinel": "private-test-value"}})


def chat_healthy():
    events = [{"choices": [{"index": 0, "delta": {"content": "HEALTHY🙂"}, "finish_reason": None}]},
              {"choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}]},
              {"choices": [], "usage": {"prompt_tokens": 7, "completion_tokens": 3, "total_tokens": 10}}]
    return b"".join(b"data: " + json.dumps(event, ensure_ascii=False).encode() + b"\n\n" for event in events) + b"data: [DONE]\n\n"


def assert_legacy_tail_summary(summary):
    assert isinstance(summary, dict) and set(summary) == TAIL_STRUCTURE_KEYS, summary
    assert summary["complete"] is True, summary
    counts = {key: value for key, value in summary.items() if key != "complete"}
    assert all(type(value) is int and value >= 0 for value in counts.values()), summary
    expected = {"done_count": 1, "tail_frames": 1, "extra_fields_count": 1,
                "output_fields_count": 1, "other_type_count": 1}
    assert all(counts[key] == expected.get(key, 0) for key in counts), summary


def assert_tail_structure(message):
    marker = "; tail_structure_v2="
    assert message.count(marker) == 1 and "; tail_structure=" not in message, message
    preamble, raw = message.split(marker, 1)
    assert "data after SSE termination" in preamble, preamble
    assert 0 < len((marker + raw).encode()) <= 1024, len((marker + raw).encode())
    for private in (MIMO_PRIVATE_SENTINEL, "Hi世界🙂", "call_mock", '"x":"ok"'):
        assert private not in message, (private, message)
    decoded = json.loads(raw)  # One complete compact JSON object; no trailing text.
    assert isinstance(decoded, dict) and raw == json.dumps(decoded, separators=(",", ":"), ensure_ascii=False), raw
    assert set(decoded) == {"schema", "summary", "detail_complete", "frames"}, decoded
    assert type(decoded["schema"]) is int and decoded["schema"] == 2 and decoded["detail_complete"] is True, decoded
    assert_legacy_tail_summary(decoded["summary"])
    frames = decoded["frames"]
    assert isinstance(frames, list) and len(frames) == 1 and isinstance(frames[0], dict), frames
    assert set(frames[0]) == {"kind", "fields", "unknown_fields"} and type(frames[0]["unknown_fields"]) is int, frames
    assert frames[0] == {"kind": "object", "fields": [["choices", "empty_array"], ["cost", "opaque"]],
                         "unknown_fields": 0}, frames


def responses_tail(kind):
    """Synthetic HTTP body: completed text, tool, usage, then ping and EOF (no [DONE])."""
    assert kind in TAIL_POSITIVES + TAIL_NEGATIVES
    events = [
        {"type": "response.output_text.delta", "item_id": "msg_0", "output_index": 0, "content_index": 0, "delta": "Hi"},
        {"type": "response.output_text.done", "item_id": "msg_0", "output_index": 0, "content_index": 0, "text": "Hi"},
        {"type": "response.output_text.delta", "item_id": "msg_1", "output_index": 1, "content_index": 0, "delta": "世界🙂"},
        {"type": "response.output_text.done", "item_id": "msg_1", "output_index": 1, "content_index": 0, "text": "世界🙂"},
        {"type": "response.output_item.added", "output_index": 2, "item":
         {"id": "item_mock", "call_id": "fc_mock", "type": "function_call", "name": "probe", "arguments": ""}},
        {"type": "response.function_call_arguments.delta", "item_id": "item_mock", "output_index": 2, "delta": '{"x":'},
        {"type": "response.output_item.done", "output_index": 2, "item":
         {"id": "item_mock", "call_id": "fc_mock", "type": "function_call", "name": "probe", "arguments": '{"x":"ok"}'}},
        {"type": "response.completed", "response": {"status": "completed", "usage": {"input_tokens": 7, "output_tokens": 3}}},
        {"type": "ping", "cost": {"synthetic": "opaque"}},
    ]
    if kind == "null":
        events[-1]["cost"] = None  # Present cost is opaque, including JSON null.
    elif kind == "extra-delta":
        events[-1]["delta"] = "late"
    elif kind == "extra-response":
        events[-1]["response"] = {"output": ["late"]}
    elif kind == "no-cost":
        del events[-1]["cost"]
    elif kind == "unknown":
        events[-1]["type"] = "heartbeat"
    elif kind == "late-output":
        events.append({"type": "response.output_text.delta", "item_id": "late", "output_index": 3,
                       "content_index": 0, "delta": "BAD"})
    elif kind == "late-error":
        events.append({"type": "error", "error": {"message": "synthetic late error"}})
    elif kind == "late-failed":
        events.append({"type": "response.failed", "response": {"status": "failed",
                       "error": {"message": "synthetic late failure"}}})
    return b"".join(b"data: " + json.dumps(event, ensure_ascii=False).encode() + b"\n\n" for event in events)


def main(path, expected_version=None, first_mimo_mode="nonstream"):
    assert first_mimo_mode in ("nonstream", "stream")
    lib = C.CDLL(path)
    lib.cliproxy_plugin_init.argtypes = [C.POINTER(Host), C.POINTER(Plugin)]
    lib.cliproxy_plugin_init.restype = C.c_int
    lib.ZenPluginCall.argtypes = [C.c_char_p, C.c_void_p, C.c_size_t, C.POINTER(Buffer)]
    lib.ZenPluginCall.restype = C.c_int
    lib.ZenPluginFree.argtypes = [C.c_void_p, C.c_size_t]
    lib.ZenPluginFree.restype = None
    lib.ZenPluginShutdown.argtypes = []
    lib.ZenPluginShutdown.restype = None
    state = {"mode": "valid", "calls": 0, "frees": 0, "paid_calls": 0}
    buffers = {}
    callback_errors = []
    emit_started = threading.Event()
    stream_closed = threading.Event()

    @CALL
    def call(ctx, method, payload, length, result):
        try:
            state["calls"] += 1
            if state["mode"] == "oversize":
                result[0] = Buffer(1, 1 << 31)
            elif state["mode"] == "null":
                result[0] = Buffer(None, 10)
            else:
                req = json.loads(C.string_at(payload, length))
                if method == b"host.stream.emit":
                    emit_started.set()
                    assert stream_closed.wait(5), "shutdown did not close blocked emit"
                    return 1
                if method == b"host.stream.close":
                    stream_closed.set()
                    result_body = {}
                else:
                    assert method == b"host.http.do", method
                    status_code = 200
                    if state["mode"] == "paid-recovery":
                        assert req["Method"] == "POST" and req["URL"] == "http://mock:9999/v1/chat/completions", req
                        assert req["Headers"]["Authorization"] == ["Bearer offline-paid-only"], req["Headers"]
                        posted = json.loads(base64.b64decode(req["Body"]))
                        assert posted["model"] == "offline-fallback" and posted["stream"] is False, posted
                        state["paid_calls"] += 1
                        body = json.dumps({"id": "native-paid", "object": "chat.completion", "model": "offline-fallback",
                                           "choices": [{"message": {"role": "assistant", "content": "FALLBACK🙂"},
                                                        "finish_reason": "stop"}],
                                           "usage": {"prompt_tokens": 7, "completion_tokens": 3, "total_tokens": 10}}).encode()
                    elif state["mode"] == "responses-tail":
                        assert req["Method"] == "POST" and req["URL"] == "http://mock:9999/v1/responses", req
                        assert req["Headers"]["Authorization"] == ["Bearer public"], req["Headers"]
                        assert req["Headers"]["X-Opencode-Session"] == ["offline-abi-session-only"], req["Headers"]
                        posted = json.loads(base64.b64decode(req["Body"]))
                        assert posted["model"] == "offline-responses" and posted["stream"] is True, posted
                        assert posted["input"] and posted["tools"], posted
                        body = responses_tail(state["tail_case"])
                    elif state["mode"] in ("geo-free", "geo-paid", "geo-control"):
                        posted = json.loads(base64.b64decode(req["Body"]))
                        model = posted["model"]
                        assert req["Method"] == "POST", req
                        if model == "offline-fallback":
                            assert state["mode"] == "geo-paid", (state, req)
                            assert req["URL"] == "http://mock:9999/v1/chat/completions", req
                            assert req["Headers"]["Authorization"] == ["Bearer offline-paid-only"], req
                            assert posted["stream"] is False, posted
                            state["paid_calls"] += 1
                            body = json.dumps({"choices": [{"message": {"role": "assistant", "content": "FALLBACK🙂"},
                                                            "finish_reason": "stop"}],
                                               "usage": {"prompt_tokens": 7, "completion_tokens": 3, "total_tokens": 10}}).encode()
                        else:
                            assert req["Headers"]["Authorization"] == ["Bearer public"], req
                            assert req["Headers"]["X-Opencode-Session"] == ["offline-abi-session-only"], req
                            assert posted["stream"] is True, posted
                            if state["mode"] == "geo-control":
                                assert model == "offline-chat" and req["URL"] == "http://mock:9999/v1/chat/completions", req
                                body = chat_healthy()
                            else:
                                assert model == state["geo_model"] and req["URL"] == "http://mock:9999" + state["geo_path"], req
                                wording = GEO_TEXT.upper() if model == "offline-geo-mixed" else GEO_TEXT
                                body = json.dumps({"error": {"message": "Error from provider (Console): " + wording}}).encode()
                                status_code = 403
                    elif state["mode"] == "mimo-tail":
                        assert req["Method"] == "POST" and req["URL"] == "http://mock:9999/v1/chat/completions", req
                        assert req["Headers"]["Authorization"] == ["Bearer public"], req
                        assert req["Headers"]["X-Opencode-Session"] == ["offline-abi-session-only"], req
                        posted = json.loads(base64.b64decode(req["Body"]))
                        assert posted["model"] in (MIMO_UPSTREAM, "offline-chat-tail-other") and posted["stream"] is True, posted
                        body = chat_post_done()
                    else:
                        streaming = state["mode"] == "stream"
                        assert req["Method"] == ("POST" if streaming else "GET"), req
                        body = (b'{"answers":{"q":"ok"},"usage":{"input_tokens":7,"output_tokens":3}}'
                                if streaming else b"synthetic")
                    result_body = {"StatusCode": status_code, "Body": base64.b64encode(body).decode()}
                raw = json.dumps({"ok": True, "result": result_body}).encode()
                buf = C.create_string_buffer(raw)
                ptr = C.addressof(buf)
                buffers[ptr] = buf
                result[0] = Buffer(ptr, len(raw))
            return 0
        except Exception as exc:
            callback_errors.append(str(exc))
            return 1

    @FREE
    def free(ptr, length):
        state["frees"] += 1
        buffers.pop(ptr, None)  # Never dereference the intentional sentinel.

    host = Host(1, None, call, free)
    plugin = Plugin()
    assert lib.cliproxy_plugin_init(C.byref(host), C.byref(plugin)) == 0
    assert plugin.version == 1 and plugin.call and plugin.free and plugin.shutdown
    plugin_call = C.CFUNCTYPE(C.c_int, C.c_char_p, C.c_void_p, C.c_size_t, C.POINTER(Buffer))(plugin.call)

    def invoke(method, payload=None, ptr=None, length=None):
        buf = None
        if payload is not None:
            raw = json.dumps(payload).encode()
            buf = C.create_string_buffer(raw)
            ptr, length = C.addressof(buf), len(raw)
        out = Buffer()
        status = plugin_call(method.encode() if method else None, ptr, length or 0, C.byref(out))
        try:
            assert out.ptr and out.length, "missing error/success envelope"
            return status, json.loads(C.string_at(out.ptr, out.length))
        finally:
            lib.ZenPluginFree(out.ptr, out.length)

    for method in ("executor.execute", "plugin.register", "response.translate", "unknown"):
        for ptr, length in ((None, 1), (1, 1 << 31), (1, C.c_size_t(-1).value)):
            status, envelope = invoke(method, ptr=ptr, length=length)
            assert status != 0 and not envelope["ok"], (method, length, envelope)
    for method in ("request.intercept_before", "request.intercept_after"):
        status, envelope = invoke(method, ptr=1, length=(64 << 20) + 1)
        assert status != 0 and envelope["error"]["code"] == "invalid_request"
    assert invoke(None)[0] != 0
    print("PASS: inbound lengths rejected before pointer access")

    config = b"paid:\n  base_url: http://mock:9999\n  api_key: synthetic-key\n"
    status, envelope = invoke("plugin.register", {"config_yaml": base64.b64encode(config).decode()})
    assert status == 0 and envelope["ok"], envelope
    if expected_version is not None:
        actual = envelope["result"]["metadata"]["Version"]
        assert actual == expected_version, ("staged native plugin metadata version mismatch", actual, expected_version)
    request = {"Method": "GET", "URL": "http://mock:9999/mock"}
    for mode in ("oversize", "null"):
        state["mode"] = mode
        _, envelope = invoke("executor.http_request", request)
        assert not envelope["ok"], envelope
        assert "buffer" in envelope["error"]["message"], envelope
    state["mode"] = "valid"
    _, envelope = invoke("executor.http_request", request)
    assert envelope["ok"] and envelope["result"]["StatusCode"] == 200, envelope
    assert base64.b64decode(envelope["result"]["Body"]) == b"synthetic"
    assert state["calls"] == 3 and state["frees"] == 2, state
    assert not callback_errors and not buffers, callback_errors
    print("PASS: host buffer bounds, freeing and raw HTTP method round-trip")

    # Native dispatch, without an HTTP server: missing synthetic pool must be
    # rejected before any host.http.do callback for chat and Responses, in
    # both output modes. All configured URLs are mock-only; no real credentials.
    free_config = b"""paid:
  base_url: http://mock:9999
  api_key: offline-paid-only
free:
  enabled: true
  session_pool: /offline/nonexistent/zen-sessions.json
  members:
    - key: public
  quota:
    failover_paid: false
  models:
    - {alias: offline-chat, name: offline-chat, endpoint: chat}
    - {alias: offline-responses, name: offline-responses, endpoint: responses}
"""
    status, envelope = invoke("plugin.register", {"config_yaml": base64.b64encode(free_config).decode()})
    assert status == 0 and envelope["ok"], envelope
    baseline = (state["calls"], state["frees"])
    for model in ("offline-chat", "offline-responses"):
        for method in ("executor.execute", "executor.execute_stream"):
            payload = {"model": model, "messages": [{"role": "user", "content": "synthetic"}], "stream": method.endswith("stream")}
            status, envelope = invoke(method, {
                "Model": model, "Payload": base64.b64encode(json.dumps(payload).encode()).decode(),
                "stream_id": "offline-native-guard",
            })
            error = envelope.get("error") or {}
            # The current ABI returns non-stream executor errors in an OK
            # transport call; execute_stream returns a nonzero transport code.
            assert status == (1 if method.endswith("stream") else 0) and envelope["ok"] is False, (method, model, status, envelope)
            assert error.get("http_status") == 503 and "earliest_local_retry_at=unknown" in error.get("message", ""), envelope
            assert (state["calls"], state["frees"]) == baseline, "local 503 invoked host callback"
    assert not callback_errors and not buffers, callback_errors
    print("PASS: native ABI chat/Responses local HTTP 503 reason and zero host callbacks")

    # The pinned Host does not forward executor-only response headers. Inspect
    # the actual native envelope for the paid marker, while HTTP fixtures
    # independently verify paid authorization/count and rendered output.
    recovery_config = free_config.replace(b"    failover_paid: false\n",
                                          b"    failover_paid: true\n    paid_fallback: offline-fallback\n")
    assert recovery_config != free_config
    status, envelope = invoke("plugin.register", {"config_yaml": base64.b64encode(recovery_config).decode()})
    assert status == 0 and envelope["ok"], envelope
    state["mode"] = "paid-recovery"
    for method in ("executor.execute", "executor.execute_stream"):
        payload = {"model": "offline-chat", "messages": [{"role": "user", "content": "synthetic"}],
                   "stream": method.endswith("stream")}
        status, envelope = invoke(method, {"Model": "offline-chat", "Payload": base64.b64encode(json.dumps(payload).encode()).decode()})
        assert status == 0 and envelope["ok"], (method, status, envelope)
        result = envelope["result"]
        # Non-stream ExecutorResponse uses exported Go field names; the ABI
        # stream wrapper explicitly tags lowercase headers/chunks in JSON.
        headers = {name.lower(): values for name, values in result.get("headers", result.get("Headers", {})).items()}
        assert headers.get("x-zen-fallback") == ["paid"], (method, headers)
        if method.endswith("stream"):
            chunks = b"".join(base64.b64decode(chunk["Payload"]) for chunk in result["chunks"])
            assert b"FALLBACK" in chunks and b"finish_reason" in chunks, chunks
        else:
            assert json.loads(base64.b64decode(result["Payload"]))["choices"][0]["message"]["content"] == "FALLBACK🙂"
    assert state["paid_calls"] == 2 and state["calls"] == baseline[0] + 2, state
    assert not callback_errors and not buffers, callback_errors
    print("PASS: native ABI explicit paid fallback marker, two isolated one-call executions")

    # The native HTTP callback is the ONLY upstream: give the registered pool
    # one synthetic healthy session, return Responses SSE directly, then verify
    # the complete text/tool/usage/finish or exact HTTP 502 error envelope.
    # This also checks that an accepted ping does not hide a later bad event.
    with tempfile.TemporaryDirectory(prefix="zen-native-tail-") as directory:
        pool_path = os.path.join(directory, "zen-sessions.json")
        with open(pool_path, "w", encoding="utf-8") as pool:
            json.dump({"sessions": [{"id": "offline-abi-session-only", "fails": 0,
                                     "cooldown_until": 0}]}, pool)
        tail_config = free_config.replace(b"/offline/nonexistent/zen-sessions.json", os.fsencode(pool_path))
        assert tail_config != free_config
        status, envelope = invoke("plugin.register", {"config_yaml": base64.b64encode(tail_config).decode()})
        assert status == 0 and envelope["ok"], envelope
        state["mode"] = "responses-tail"
        payload = {"model": "offline-responses", "stream": False, "max_tokens": 64,
                   "messages": [{"role": "user", "content": "synthetic tail protocol test"}],
                   "tools": [{"type": "function", "function": {"name": "probe", "parameters":
                              {"type": "object", "properties": {"x": {"type": "string"}}}}}]}

        def check_tail_success(result, streaming):
            if streaming:
                pieces = [base64.b64decode(chunk["Payload"]) for chunk in result["chunks"]]
                assert pieces and all(not chunk.get("Err") for chunk in result["chunks"]), result
                assert all(piece.startswith(b"{") and b"[DONE]" not in piece for piece in pieces), pieces
                events = [json.loads(piece) for piece in pieces]
                choices = [event["choices"][0] for event in events if event.get("choices")]
                content = "".join((choice.get("delta") or {}).get("content") or "" for choice in choices)
                tools = [tool for choice in choices for tool in (choice.get("delta") or {}).get("tool_calls") or []]
                assert content == "Hi世界🙂", (content, events)
                assert [tool.get("id") for tool in tools if tool.get("id")] == ["fc_mock"], tools
                assert [(tool.get("function") or {}).get("name") for tool in tools
                        if (tool.get("function") or {}).get("name")] == ["probe"], tools
                args = "".join((tool.get("function") or {}).get("arguments") or "" for tool in tools)
                assert json.loads(args) == {"x": "ok"}, tools
                assert [choice.get("finish_reason") for choice in choices if choice.get("finish_reason")] == ["tool_calls"], events
                usages = [event["usage"] for event in events if event.get("usage")]
                assert len(usages) == 1, events
                usage = usages[0]
            else:
                completion = json.loads(base64.b64decode(result["Payload"]))
                assert completion["model"] == "offline-responses", completion
                choice = completion["choices"][0]
                assert choice["message"]["content"] == "Hi世界🙂" and choice["finish_reason"] == "tool_calls", choice
                tools = choice["message"]["tool_calls"]
                assert len(tools) == 1 and tools[0]["id"] == "fc_mock" and tools[0]["function"]["name"] == "probe", tools
                assert json.loads(tools[0]["function"]["arguments"]) == {"x": "ok"}, tools
                usage = completion["usage"]
            assert (usage["prompt_tokens"], usage["completion_tokens"], usage["total_tokens"]) == (7, 3, 10), usage

        for kind in TAIL_POSITIVES + TAIL_NEGATIVES:
            state["tail_case"] = kind
            for method in ("executor.execute", "executor.execute_stream"):
                payload["stream"] = method.endswith("stream")
                before = (state["calls"], state["frees"], state["paid_calls"])
                status, envelope = invoke(method, {
                    "Model": "offline-responses", "Payload": base64.b64encode(json.dumps(payload).encode()).decode(),
                })
                if kind in TAIL_POSITIVES:
                    assert status == 0 and envelope["ok"] is True, (kind, method, status, envelope)
                    check_tail_success(envelope["result"], payload["stream"])
                else:
                    error = envelope.get("error") or {}
                    assert status == (1 if payload["stream"] else 0) and envelope["ok"] is False, (kind, method, status, envelope)
                    assert error.get("http_status") == 502, (kind, method, envelope)
                assert (state["calls"], state["frees"], state["paid_calls"]) == (before[0] + 1, before[1] + 1, before[2]), (kind, method, state)
                assert not callback_errors and not buffers, (kind, method, callback_errors)
        print("PASS: native ABI Responses EOF cost ping text/tool/usage and strict post-completion HTTP 502 (both modes)")

    # Fresh native pool for two cases in both output modes: exact country 403
    # must preserve its body/status, never penalize disk or in-memory health,
    # and only an explicitly enabled paid fallback may issue a paid call.
    with tempfile.TemporaryDirectory(prefix="zen-native-geo-mimo-") as directory:
        pool_path = os.path.join(directory, "zen-sessions.json")
        with open(pool_path, "w", encoding="utf-8") as pool:
            json.dump({"sessions": [{"id": "offline-abi-session-only", "fails": 0,
                                     "cooldown_until": 0}]}, pool)
        geo_config = free_config.replace(b"/offline/nonexistent/zen-sessions.json", os.fsencode(pool_path)).replace(
            b"    - {alias: offline-responses, name: offline-responses, endpoint: responses}\n",
            b"    - {alias: offline-responses, name: offline-responses, endpoint: responses}\n"
            b"    - {alias: offline-geo-chat, name: offline-geo-chat, endpoint: chat}\n"
            b"    - {alias: offline-geo-mixed, name: offline-geo-mixed, endpoint: chat}\n"
            b"    - {alias: offline-geo-responses, name: offline-geo-responses, endpoint: responses}\n"
            b"    - {alias: mimo-free, name: mimo-v2.6-flash-free, endpoint: chat}\n"
            b"    - {alias: offline-chat-tail-other, name: offline-chat-tail-other, endpoint: chat}\n")
        assert geo_config != free_config and b"alias: mimo-free" in geo_config

        def register_synthetic(raw):
            status, envelope = invoke("plugin.register", {"config_yaml": base64.b64encode(raw).decode()})
            assert status == 0 and envelope["ok"], envelope
            if expected_version is not None:
                assert envelope["result"]["metadata"]["Version"] == expected_version, envelope

        def native_request(model, method):
            payload = {"model": model, "stream": method.endswith("stream"), "max_tokens": 64,
                       "messages": [{"role": "user", "content": "offline synthetic test"}],
                       "tools": [{"type": "function", "function": {"name": "probe", "parameters":
                                  {"type": "object", "properties": {"x": {"type": "string"}}}}}]}
            return invoke(method, {"Model": model, "Payload": base64.b64encode(json.dumps(payload).encode()).decode()})

        def snapshot_pool():
            stat = os.stat(pool_path)
            with open(pool_path, "rb") as pool:
                contents = pool.read()
            assert not os.path.exists(pool_path + ".tmp"), "synthetic pool left a temporary file"
            return contents, stat.st_ino, stat.st_mtime_ns

        def check_single_callback(before, calls=1, paid=0):
            assert (state["calls"], state["frees"], state["paid_calls"]) == (before[0] + calls, before[1] + calls,
                                                                              before[2] + paid), (before, state)
            assert not callback_errors and not buffers, callback_errors

        register_synthetic(geo_config)
        geo_models = ("offline-geo-chat", "offline-geo-responses", "offline-geo-mixed")
        for paid in (False, True):
            if paid:
                paid_config = geo_config.replace(b"    failover_paid: false\n",
                                                 b"    failover_paid: true\n    paid_fallback: offline-fallback\n")
                assert paid_config != geo_config
                register_synthetic(paid_config)
            for model in geo_models:
                for method in ("executor.execute", "executor.execute_stream"):
                    state["mode"] = "geo-paid" if paid else "geo-free"
                    state["geo_model"] = model
                    state["geo_path"] = "/v1/responses" if model == "offline-geo-responses" else "/v1/chat/completions"
                    before, pool_before = (state["calls"], state["frees"], state["paid_calls"]), snapshot_pool()
                    status, envelope = native_request(model, method)
                    if paid:
                        assert status == 0 and envelope["ok"] is True, (model, method, status, envelope)
                        result = envelope["result"]
                        headers = {k.lower(): v for k, v in result.get("headers", result.get("Headers", {})).items()}
                        assert headers.get("x-zen-fallback") == ["paid"], (model, method, headers)
                        raw = (b"".join(base64.b64decode(chunk["Payload"]) for chunk in result["chunks"])
                               if method.endswith("stream") else base64.b64decode(result["Payload"]))
                        assert b"FALLBACK" in raw and b"finish_reason" in raw, (model, method, raw)
                    else:
                        error = envelope.get("error") or {}
                        assert status == (1 if method.endswith("stream") else 0) and envelope["ok"] is False, (model, method, status, envelope)
                        original = "Error from provider (Console): " + (GEO_TEXT.upper() if model == "offline-geo-mixed" else GEO_TEXT)
                        assert error.get("http_status") == 403 and original in error.get("message", ""), (model, method, envelope)
                        assert "; tail_structure=" not in error["message"] and "; tail_structure_v2=" not in error["message"], (model, method, envelope)
                    check_single_callback(before, calls=2 if paid else 1, paid=int(paid))
                    assert snapshot_pool() == pool_before, (model, method, "country denial mutated synthetic pool")
                    # The only member/session must work immediately after the
                    # policy denial, proving no hidden in-memory cooldown.
                    state["mode"] = "geo-control"
                    before = (state["calls"], state["frees"], state["paid_calls"])
                    status, envelope = native_request("offline-chat", method)
                    assert status == 0 and envelope["ok"] is True, (model, method, envelope)
                    raw = (b"".join(base64.b64decode(chunk["Payload"]) for chunk in envelope["result"]["chunks"])
                           if method.endswith("stream") else base64.b64decode(envelope["result"]["Payload"]))
                    assert b"HEALTHY" in raw and b"finish_reason" in raw, (model, method, raw)
                    check_single_callback(before)
        print("PASS: native ABI country 403 fail-fast and unchanged pool/health; explicit paid fallback only (both modes)")

        # A single Executor emits the bounded observer once for the canonical
        # Mimo alias. Its upstream-name lookup resolves the SAME free entry and
        # cannot receive a second diagnostic. A different Chat alias may not
        # receive one either. Two independent native processes swap first mode.
        register_synthetic(geo_config)
        state["mode"] = "mimo-tail"
        first = "executor.execute_stream" if first_mimo_mode == "stream" else "executor.execute"
        other = "executor.execute" if first.endswith("stream") else "executor.execute_stream"
        for index, (model, method) in enumerate((("offline-chat-tail-other", first), ("mimo-free", first),
                                                   (MIMO_UPSTREAM, other), ("offline-chat-tail-other", first))):
            before = (state["calls"], state["frees"], state["paid_calls"])
            status, envelope = native_request(model, method)
            error = envelope.get("error") or {}
            assert status == (1 if method.endswith("stream") else 0) and envelope["ok"] is False, (model, method, status, envelope)
            assert error.get("http_status") == 502, (model, method, envelope)
            message = error.get("message", "")
            if index == 1:
                assert_tail_structure(message)
            else:
                assert ("data after SSE termination" in message and "; tail_structure=" not in message and
                        "; tail_structure_v2=" not in message), (model, method, message)
            assert all(private not in message for private in
                       (MIMO_PRIVATE_SENTINEL, "Hi世界🙂", "call_mock", '"x":"ok"')), (model, method, message)
            assert MIMO_PRIVATE_SENTINEL not in json.dumps(envelope, ensure_ascii=False), (model, method, envelope)
            check_single_callback(before)
        print(f"PASS: native ABI Mimo-only once-per-Executor strict HTTP 502 v2 diagnostic (first={first_mimo_mode})")

    status, envelope = invoke("plugin.register", {"config_yaml": base64.b64encode(config).decode()})
    assert status == 0 and envelope["ok"], envelope
    state["mode"] = "stream"
    chat = {"model": "jev", "stream": True, "messages": [{"role": "user", "content":
        json.dumps({"state": "synthetic", "questions": {"q": {"type": "noul", "instructions": "test"}}})}]}
    _, envelope = invoke("executor.execute_stream", {
        "Model": "jev", "Payload": base64.b64encode(json.dumps(chat).encode()).decode(), "stream_id": "native-test"
    })
    assert envelope["ok"], envelope
    assert emit_started.wait(5), "native stream never emitted"
    shutdown = threading.Thread(target=lib.ZenPluginShutdown, daemon=True)
    shutdown.start()
    shutdown.join(5)
    assert not shutdown.is_alive(), "shutdown failed to drain blocked callback"
    assert stream_closed.is_set() and not callback_errors and not buffers, callback_errors
    print("PASS: shutdown cancels blocked native emit and drains callback buffers")
    print("PASS: native ABI probes (no external requests)")


if __name__ == "__main__":
    if len(sys.argv) not in (2, 4):
        raise SystemExit("usage: abi_probe.py /path/to/staged/zen.so [expected-version nonstream|stream]")
    main(sys.argv[1], sys.argv[2] if len(sys.argv) == 4 else None,
         sys.argv[3] if len(sys.argv) == 4 else "nonstream")
