#!/usr/bin/env python3
"""Synthetic, allowlisted upstream. Run only on the private offline-e2e network."""
import json
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

CALLS = []
LOCK = threading.Lock()


def sse(events):
    return b"".join(b"data: " + json.dumps(e, ensure_ascii=False).encode() + b"\n\n" for e in events) + b"data: [DONE]\n\n"


def chat_chunk(delta=None, finish=None, usage=None):
    out = {"id": "chatcmpl-mock", "object": "chat.completion.chunk", "model": "offline", "choices": []}
    if delta is not None or finish is not None:
        out["choices"] = [{"index": 0, "delta": delta or {}, "finish_reason": finish}]
    if usage is not None:
        out["usage"] = usage
    return out


USAGE = {"prompt_tokens": 7, "completion_tokens": 3, "total_tokens": 10}


def chat_text():
    return sse([chat_chunk({"role": "assistant", "content": "Hi"}),
                chat_chunk({"content": "世界🙂"}), chat_chunk(finish="stop"),
                chat_chunk(usage=USAGE)])


def chat_tools():
    return sse([chat_chunk({"role": "assistant", "tool_calls": [{"index": 0, "id": "call_mock", "type": "function", "function": {"name": "probe", "arguments": ""}}]}),
                chat_chunk({"tool_calls": [{"index": 0, "function": {"arguments": '{"x":'}}]}),
                chat_chunk({"tool_calls": [{"index": 0, "function": {"arguments": '"ok"}'}}]}),
                chat_chunk(finish="tool_calls"), chat_chunk(usage=USAGE)])


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
        if self.path == "/__calls":
            with LOCK:
                snapshot = list(CALLS)
            return self.send(200, snapshot)
        self.send(404, {"error": "unknown offline mock endpoint"})

    def do_POST(self):
        if self.path == "/__reset":
            with LOCK:
                CALLS.clear()
            return self.send(200, {"ok": True})
        size = int(self.headers.get("Content-Length", "0"))
        if size < 1 or size > 1048576:
            return self.send(413, {"error": "offline mock body size"})
        try:
            body = json.loads(self.rfile.read(size))
        except (ValueError, UnicodeError):
            return self.send(400, {"error": "offline mock invalid JSON"})
        model = body.get("model")
        path = self.path
        is_free = model in {"offline-chat", "offline-chat-tool", "offline-responses", "offline-responses-done",
                            "offline-responses-tool", "offline-fail", "offline-bad", "offline-event-error"}
        allowed = {"/v1/chat/completions": {"offline-chat", "offline-chat-tool", "offline-fail", "offline-bad", "offline-fallback", "offline-fallback-long"},
                   "/v1/responses": {"offline-responses", "offline-responses-done", "offline-responses-tool", "offline-event-error"},
                   "/v1/systemone": {"offline-systemone"}}
        if model not in allowed.get(path, ()):
            return self.send(400, {"error": {"message": "offline mock path/model mismatch"}})
        authorization = self.headers.get("Authorization", "")
        expected = "Bearer public" if is_free else "Bearer offline-paid-only"
        if authorization != expected:
            return self.send(403, {"error": {"message": "offline mock rejects non-synthetic authorization"}})
        if is_free and model not in ("offline-fail", "offline-bad", "offline-event-error") and not body.get("stream"):
            return self.send(400, {"error": {"message": "free fixture requires upstream streaming"}})
        if path == "/v1/systemone" and (not body.get("state") or not body.get("questions")):
            return self.send(400, {"error": {"message": "invalid synthetic SystemOne envelope"}})
        if model.startswith("offline-fallback") and (body.get("stream") or body.get("stream_options")):
            return self.send(400, {"error": {"message": "fallback must use non-stream chat JSON"}})
        with LOCK:
            CALLS.append({"path": path, "model": model, "method": self.command, "stream": bool(body.get("stream")),
                          "session": self.headers.get("X-Opencode-Session", ""), "auth_ok": True})
        if model == "offline-systemone":
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
        if model == "offline-event-error":
            return self.send(200, sse([{"type": "response.failed", "response": {"status": "failed", "error": {"message": "offline synthetic stream failure"}}}]), "text/event-stream")
        fixture = {"offline-chat": chat_text, "offline-chat-tool": chat_tools,
                   "offline-responses": responses_text, "offline-responses-done": responses_done_only,
                   "offline-responses-tool": responses_tools}[model]
        self.send(200, fixture(), "text/event-stream")


if __name__ == "__main__":
    ThreadingHTTPServer(("0.0.0.0", 9999), Handler).serve_forever()
