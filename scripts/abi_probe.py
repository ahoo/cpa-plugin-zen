#!/usr/bin/env python3
"""Offline native ABI boundary probes. Usage: python3 scripts/abi_probe.py /tmp/zen.so.

Uses synthetic callbacks only; never reads configuration or contacts an upstream.
Run in a subprocess: invalid pointer probes intentionally make unsafe reads fatal.
"""
import base64
import ctypes as C
import json
import sys
import threading


class Buffer(C.Structure):
    _fields_ = [("ptr", C.c_void_p), ("length", C.c_size_t)]


CALL = C.CFUNCTYPE(C.c_int, C.c_void_p, C.c_char_p, C.c_void_p, C.c_size_t, C.POINTER(Buffer))
FREE = C.CFUNCTYPE(None, C.c_void_p, C.c_size_t)


class Host(C.Structure):
    _fields_ = [("version", C.c_uint32), ("ctx", C.c_void_p), ("call", CALL), ("free", FREE)]


class Plugin(C.Structure):
    _fields_ = [("version", C.c_uint32), ("call", C.c_void_p), ("free", C.c_void_p), ("shutdown", C.c_void_p)]


def main(path):
    lib = C.CDLL(path)
    lib.cliproxy_plugin_init.argtypes = [C.POINTER(Host), C.POINTER(Plugin)]
    lib.cliproxy_plugin_init.restype = C.c_int
    lib.ZenPluginCall.argtypes = [C.c_char_p, C.c_void_p, C.c_size_t, C.POINTER(Buffer)]
    lib.ZenPluginCall.restype = C.c_int
    lib.ZenPluginFree.argtypes = [C.c_void_p, C.c_size_t]
    lib.ZenPluginFree.restype = None
    lib.ZenPluginShutdown.argtypes = []
    lib.ZenPluginShutdown.restype = None
    state = {"mode": "valid", "calls": 0, "frees": 0}
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
                    streaming = state["mode"] == "stream"
                    assert req["Method"] == ("POST" if streaming else "GET"), req
                    body = (b'{"answers":{"q":"ok"},"usage":{"input_tokens":7,"output_tokens":3}}'
                            if streaming else b"synthetic")
                    result_body = {"StatusCode": 200, "Body": base64.b64encode(body).decode()}
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

    config = b"paid:\n  api_key: synthetic-key\n"
    status, envelope = invoke("plugin.register", {"config_yaml": base64.b64encode(config).decode()})
    assert status == 0 and envelope["ok"], envelope
    request = {"Method": "GET", "URL": "https://example.invalid/mock"}
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
    if len(sys.argv) != 2:
        raise SystemExit("usage: abi_probe.py /path/to/staged/zen.so")
    main(sys.argv[1])
