#!/usr/bin/env python3
"""Synthetic, allowlisted upstream. Run only on the private offline-e2e network."""
import http.client
import json
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import urlsplit

CALLS = []
PROXY_CALLS = []
LOCK = threading.Lock()


def sse(events, done=True):
    frames = b"".join(b"data: " + json.dumps(e, ensure_ascii=False).encode() + b"\n\n" for e in events)
    return frames + (b"data: [DONE]\n\n" if done else b"")


def chat_chunk(delta=None, finish=None, usage=None):
    out = {"id": "chatcmpl-mock", "object": "chat.completion.chunk", "model": "offline", "choices": []}
    if delta is not None or finish is not None:
        out["choices"] = [{"index": 0, "delta": delta or {}, "finish_reason": finish}]
    if usage is not None:
        out["usage"] = usage
    return out


USAGE = {"prompt_tokens": 7, "completion_tokens": 3, "total_tokens": 10}
TAIL_KINDS = {
    "offline-tail-ping": "ping", "offline-tail-null": "null", "offline-tail-extra-delta": "extra-delta",
    "offline-tail-extra-response": "extra-response", "offline-tail-no-cost": "no-cost",
    "offline-tail-unknown": "unknown", "offline-tail-late-output": "late-output",
    "offline-tail-late-error": "late-error", "offline-tail-late-failed": "late-failed",
}


def chat_text():
    return sse([chat_chunk({"role": "assistant", "content": "Hi"}),
                chat_chunk({"content": "世界🙂"}), chat_chunk(finish="stop"),
                chat_chunk(usage=USAGE)])


def chat_tools():
    return sse([chat_chunk({"role": "assistant", "tool_calls": [{"index": 0, "id": "call_mock", "type": "function", "function": {"name": "probe", "arguments": ""}}]}),
                chat_chunk({"tool_calls": [{"index": 0, "function": {"arguments": '{"x":'}}]}),
                chat_chunk({"tool_calls": [{"index": 0, "function": {"arguments": '"ok"}'}}]}),
                chat_chunk(finish="tool_calls"), chat_chunk(usage=USAGE)])


def chat_post_done():
    """Valid Chat text/tool/usage/finish, [DONE], then a synthetic choices+cost frame."""
    finished = sse([
        chat_chunk({"role": "assistant", "content": "Hi", "tool_calls": [
            {"index": 0, "id": "call_mock", "type": "function", "function": {"name": "probe", "arguments": ""}}]}),
        chat_chunk({"content": "世界🙂", "tool_calls": [{"index": 0, "function": {"arguments": '{"x":'}}]}),
        chat_chunk({"tool_calls": [{"index": 0, "function": {"arguments": '"ok"}'}}]}),
        chat_chunk(finish="tool_calls"), chat_chunk(usage=USAGE),
    ])
    return finished + sse([{"choices": [], "cost": {"sentinel": "private-test-value"}}], done=False)


def responses_text():
    return sse([{"type": "response.output_text.delta", "output_index": 0, "content_index": 0, "item_id": "msg_0", "delta": "Hi"},
                {"type": "response.output_text.done", "output_index": 0, "content_index": 0, "item_id": "msg_0", "text": "Hi"},
                {"type": "response.output_text.delta", "output_index": 1, "content_index": 0, "item_id": "msg_1", "delta": "世界🙂"},
                {"type": "response.output_text.done", "output_index": 1, "content_index": 0, "item_id": "msg_1", "text": "世界🙂"},
                {"type": "response.completed", "response": {"status": "completed", "usage": {"input_tokens": 7, "output_tokens": 3}}}])


def responses_done_only():
    return sse([{"type": "response.output_text.done", "item_id": "msg_0", "output_index": 0, "content_index": 0, "text": "ONLY🙂"},
                {"type": "response.completed", "response": {"status": "completed", "usage": {"input_tokens": 7, "output_tokens": 3}}}])


def responses_tools():
    return sse([{"type": "response.output_item.added", "output_index": 0,
                 "item": {"id": "item_mock", "call_id": "fc_mock", "type": "function_call", "name": "probe", "arguments": ""}},
                {"type": "response.function_call_arguments.delta", "item_id": "item_mock", "output_index": 0, "delta": '{"x":'},
                {"type": "response.function_call_arguments.delta", "item_id": "item_mock", "output_index": 0, "delta": '"ok"}'},
                {"type": "response.output_item.done", "output_index": 0,
                 "item": {"id": "item_mock", "call_id": "fc_mock", "type": "function_call", "name": "probe", "arguments": '{"x":"ok"}'}},
                {"type": "response.completed", "response": {"status": "completed", "usage": {"input_tokens": 7, "output_tokens": 3}}}])


def responses_tail(kind):
    """Completed text+tool+usage, then an EOF-terminated accounting ping."""
    events = [
        {"type": "response.output_text.delta", "output_index": 0, "content_index": 0, "item_id": "msg_0", "delta": "Hi"},
        {"type": "response.output_text.done", "output_index": 0, "content_index": 0, "item_id": "msg_0", "text": "Hi"},
        {"type": "response.output_text.delta", "output_index": 1, "content_index": 0, "item_id": "msg_1", "delta": "世界🙂"},
        {"type": "response.output_text.done", "output_index": 1, "content_index": 0, "item_id": "msg_1", "text": "世界🙂"},
        {"type": "response.output_item.added", "output_index": 2,
         "item": {"id": "item_mock", "call_id": "fc_mock", "type": "function_call", "name": "probe", "arguments": ""}},
        {"type": "response.function_call_arguments.delta", "item_id": "item_mock", "output_index": 2, "delta": '{"x":"ok"}'},
        {"type": "response.output_item.done", "output_index": 2,
         "item": {"id": "item_mock", "call_id": "fc_mock", "type": "function_call", "name": "probe", "arguments": '{"x":"ok"}'}},
        {"type": "response.completed", "response": {"status": "completed", "usage": {"input_tokens": 7, "output_tokens": 3}}},
        {"type": "ping", "cost": {"synthetic": "opaque"}},
    ]
    if kind == "null":
        events[-1]["cost"] = None  # Presence matters, not the cost value/type.
    elif kind == "extra-delta":
        events[-1]["delta"] = "late"
    elif kind == "extra-response":
        events[-1]["response"] = {"output": ["late"]}
    elif kind == "no-cost":
        del events[-1]["cost"]
    elif kind == "unknown":
        events[-1]["type"] = "heartbeat"
    elif kind == "late-output":
        events.append({"type": "response.output_text.delta", "output_index": 3,
                       "content_index": 0, "item_id": "late", "delta": "BAD"})
    elif kind == "late-error":
        events.append({"type": "error", "error": {"message": "synthetic late error"}})
    elif kind == "late-failed":
        events.append({"type": "response.failed", "response": {"status": "failed",
                       "error": {"message": "synthetic late failure"}}})
    assert kind in set(TAIL_KINDS.values())
    return sse(events, done=False)  # EOF, deliberately NO data: [DONE].


def completion(model, content):
    return {"id": "chatcmpl-fallback", "object": "chat.completion", "created": 1, "model": model,
            "choices": [{"index": 0, "message": {"role": "assistant", "content": content}, "finish_reason": "stop"}], "usage": USAGE}


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *args):
        pass  # Never log bearer tokens or input bodies.

    def send(self, status, payload, content_type="application/json"):
        raw = payload if isinstance(payload, bytes) else json.dumps(payload, ensure_ascii=False).encode()
        self.send_response(status)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(raw)))
        self.send_header("Connection", "close")
        self.end_headers()
        self.wfile.write(raw)
        self.close_connection = True

    def do_GET(self):
        if self.path == "/__health":
            return self.send(200, {"ok": True})
        if self.path in ("/__calls", "/__proxy_calls"):
            with LOCK:
                snapshot = list(CALLS if self.path == "/__calls" else PROXY_CALLS)
            return self.send(200, snapshot)
        with LOCK:
            CALLS.append({"path": self.path, "method": self.command, "auth_ok": False})
        self.send(404, {"error": "unknown offline mock endpoint"})

    def do_POST(self):
        if self.path == "/__reset":
            with LOCK:
                CALLS.clear()
                PROXY_CALLS.clear()
            return self.send(200, {"ok": True})
        attempt = {"path": self.path, "model": None, "method": self.command, "stream": False,
                   "session": self.headers.get("X-Opencode-Session", ""), "auth_ok": False}
        with LOCK:
            CALLS.append(attempt)  # Count even rejected/malformed upstream requests.
        try:
            size = int(self.headers.get("Content-Length", "0"))
        except ValueError:
            return self.send(400, {"error": "offline mock invalid body size"})
        if size < 1 or size > 1048576:
            return self.send(413, {"error": "offline mock body size"})
        try:
            body = json.loads(self.rfile.read(size))
        except (ValueError, UnicodeError):
            return self.send(400, {"error": "offline mock invalid JSON"})
        if not isinstance(body, dict) or not isinstance(body.get("model"), str):
            return self.send(400, {"error": "offline mock invalid model"})
        model = body["model"]
        path = self.path
        is_free = model in TAIL_KINDS or model in {"offline-chat", "offline-chat-tool", "offline-responses", "offline-responses-done",
                            "offline-responses-tool", "offline-fail", "offline-bad", "offline-event-error",
                            "offline-policy", "offline-geo-chat", "offline-geo-mixed", "offline-geo-responses",
                            "mimo-v2.6-flash-free", "offline-chat-tail-other", "offline-free-systemone"}
        allowed = {"/v1/chat/completions": {"offline-chat", "offline-chat-tool", "offline-fail", "offline-bad",
                                             "offline-policy", "offline-geo-chat", "offline-geo-mixed",
                                             "mimo-v2.6-flash-free", "offline-chat-tail-other",
                                             "offline-fallback", "offline-fallback-long"},
                   "/v1/responses": {"offline-responses", "offline-responses-done", "offline-responses-tool",
                                      "offline-event-error", "offline-geo-responses"} | set(TAIL_KINDS),
                   "/v1/systemone": {"offline-systemone", "offline-jev", "offline-free-systemone"}}
        authorization = self.headers.get("Authorization", "")
        expected = "Bearer public" if is_free else "Bearer offline-paid-only"
        with LOCK:
            attempt.update(model=model, stream=bool(body.get("stream")), auth_ok=authorization == expected)
        if model not in allowed.get(path, ()):
            return self.send(400, {"error": {"message": "offline mock path/model mismatch"}})
        if authorization != expected:
            return self.send(403, {"error": {"message": "offline mock rejects non-synthetic authorization"}})
        if is_free and model not in ("offline-fail", "offline-bad", "offline-event-error", "offline-free-systemone") and not body.get("stream"):
            return self.send(400, {"error": {"message": "free fixture requires upstream streaming"}})
        if path == "/v1/systemone" and (not body.get("state") or not body.get("questions")):
            return self.send(400, {"error": {"message": "invalid synthetic SystemOne envelope"}})
        if model.startswith("offline-fallback") and (body.get("stream") or body.get("stream_options")):
            return self.send(400, {"error": {"message": "fallback must use non-stream chat JSON"}})
        if model in ("offline-systemone", "offline-jev", "offline-free-systemone"):
            return self.send(200, {"model": model, "answers": {"q1": "PONG"}, "usage": {"input_tokens": 7, "output_tokens": 3}})
        if model == "offline-fallback":
            if any("UNRECOVERABLE" in str(m.get("content", "")) for m in body.get("messages", [])):
                return self.send(503, {"error": {"message": "offline synthetic fallback failure"}})
            return self.send(200, completion(model, "FALLBACK🙂"))
        if model == "offline-fallback-long":
            return self.send(200, completion(model, "a" * 499 + "🙂中"))
        if model == "offline-fail":
            return self.send(503, {"error": {"message": "offline synthetic upstream unavailable"}})
        if model == "offline-bad":
            return self.send(400, {"error": {"message": "offline synthetic bad request"}})
        if model == "offline-policy":
            return self.send(403, {"error": {"message": "Error from provider (Console): OpenCode's free tier can only be used from within OpenCode"}})
        if model in ("offline-geo-chat", "offline-geo-responses", "offline-geo-mixed"):
            wording = ("THIS MODEL IS NOT AVAILABLE IN YOUR COUNTRY" if model == "offline-geo-mixed"
                       else "This model is not available in your country")
            return self.send(403, {"error": {"message": "Error from provider (Console): " + wording}})
        if model in ("mimo-v2.6-flash-free", "offline-chat-tail-other"):
            return self.send(200, chat_post_done(), "text/event-stream")
        if model == "offline-event-error":
            return self.send(200, sse([{"type": "response.failed", "response": {"status": "failed", "error": {"message": "offline synthetic stream failure"}}}]), "text/event-stream")
        if model in TAIL_KINDS:
            return self.send(200, responses_tail(TAIL_KINDS[model]), "text/event-stream")
        fixture = {"offline-chat": chat_text, "offline-chat-tool": chat_tools,
                   "offline-responses": responses_text, "offline-responses-done": responses_done_only,
                   "offline-responses-tool": responses_tools}[model]
        self.send(200, fixture(), "text/event-stream")


class Proxy(Handler):
    """Synthetic HTTP proxy: permit only this process's mock upstream."""

    def do_POST(self):
        with LOCK:
            PROXY_CALLS.append({"target": self.path, "method": self.command})
        target = urlsplit(self.path)
        if (target.scheme, target.netloc) != ("http", "mock:9999") or target.query or target.fragment or target.path not in (
                "/v1/chat/completions", "/v1/responses", "/v1/systemone"):
            return self.send(403, {"error": "offline proxy rejects non-mock target"})
        try:
            size = int(self.headers.get("Content-Length", "0"))
        except ValueError:
            return self.send(400, {"error": "offline proxy invalid body size"})
        if size < 1 or size > 1048576:
            return self.send(413, {"error": "offline proxy body size"})
        raw = self.rfile.read(size)
        conn = http.client.HTTPConnection("127.0.0.1", 9999, timeout=5)
        try:
            conn.request("POST", target.path, raw, headers={
                "Authorization": self.headers.get("Authorization", ""),
                "Content-Type": self.headers.get("Content-Type", ""),
                "X-Opencode-Session": self.headers.get("X-Opencode-Session", ""),
            })
            response = conn.getresponse()
            self.send(response.status, response.read(), response.getheader("Content-Type", "application/json"))
        except OSError:
            self.send(502, {"error": "offline proxy mock upstream unavailable"})
        finally:
            conn.close()

    def do_GET(self):
        with LOCK:
            PROXY_CALLS.append({"target": self.path, "method": self.command})
        self.send(403, {"error": "offline proxy permits mock POST only"})

    def do_CONNECT(self):
        with LOCK:
            PROXY_CALLS.append({"target": self.path, "method": self.command})
        self.send(403, {"error": "offline proxy does not allow CONNECT"})


if __name__ == "__main__":
    proxy = ThreadingHTTPServer(("0.0.0.0", 9998), Proxy)
    threading.Thread(target=proxy.serve_forever, daemon=True).start()
    ThreadingHTTPServer(("0.0.0.0", 9999), Handler).serve_forever()
