#!/usr/bin/env python3
"""Zen plugin e2e cases. Stdlib only. Every prompt carries the e2e-probe prefix.

Usage:
  python3 cases.py --base http://127.0.0.1:8317 --case 1
  python3 cases.py --base http://127.0.0.1:8317 --case all   # full suite

Exit 0 = PASS, 1 = FAIL. One JSON line on stdout: {"case":N,"ok":bool,"detail":str}.
Retry policy lives in run.sh, not here.
"""
import argparse
import json
import os
import sys
import urllib.request
import urllib.error

POOL_HDR = "X-Zen-Pool-Session"
PROMPT = "e2e-probe: reply with exactly the word PONG and nothing else."
TIMEOUT = 150
API_KEY = os.environ.get("E2E_API_KEY", "")


def post_chat(base, model, prompt=PROMPT, max_tokens=32, stream=False, headers=None):
    body = {
        "model": model,
        "messages": [{"role": "user", "content": prompt}],
        "max_tokens": max_tokens,
        "stream": stream,
    }
    data = json.dumps(body).encode()
    hdrs = {"Content-Type": "application/json", **(headers or {})}
    if API_KEY:
        hdrs["Authorization"] = "Bearer " + API_KEY
    req = urllib.request.Request(
        base.rstrip("/") + "/v1/chat/completions",
        data=data,
        headers=hdrs,
        method="POST",
    )
    try:
        with urllib.request.urlopen(req, timeout=TIMEOUT) as r:
            raw = r.read().decode("utf-8", "replace")
            return r.status, dict(r.headers.items()), raw
    except urllib.error.HTTPError as e:
        return e.code, dict(e.headers.items()), e.read().decode("utf-8", "replace")


def text_of(raw):
    try:
        j = json.loads(raw)
        ch = j.get("choices") or []
        if ch:
            return ((ch[0].get("message") or {}).get("content") or "").strip()
    except Exception:
        pass
    return ""


def sse_text_of(raw):
    out = []
    for line in raw.splitlines():
        line = line.strip()
        if not line.startswith("data:"):
            continue
        payload = line[5:].strip()
        if payload == "[DONE]":
            break
        try:
            j = json.loads(payload)
            for ch in j.get("choices") or []:
                d = ch.get("delta") or {}
                if d.get("content"):
                    out.append(d["content"])
        except Exception:
            continue
    return "".join(out).strip()


def hdr(headers, name):
    for k, v in headers.items():
        if k.lower() == name.lower():
            return v
    return ""


def case1(base):
    """Paid direct: deepseek-flash returns non-empty, no pool session."""
    st, h, raw = post_chat(base, "deepseek-flash")
    t = text_of(raw)
    if st == 200 and t:
        return True, f"200 text={t[:40]!r}"
    return False, f"status={st} text={t[:80]!r} raw={raw[:200]!r}"


def case2(base):
    """Free-first alias: mimo-free answers 200 + text.

    NOTE: host strips custom plugin response headers (proven 2026-09-23:
    plugin stamped X-Zen-Pool-Session on a session-served reply, client saw
    none), so path attribution is opportunistic only. Stickiness is covered
    by unit tests (TestStickyAssignStable, TestInterceptorStampsPoolSession).
    """
    st, h, raw = post_chat(base, "mimo-free")
    t = text_of(raw)
    extra = f" echo={hdr(h, POOL_HDR)!r} fallback={hdr(h, 'X-Zen-Fallback')!r}"
    if st == 200 and t:
        return True, f"200 text={t[:40]!r}{extra}"
    return False, f"status={st} text={t[:80]!r} raw={raw[:200]!r}{extra}"


def case3(base):
    """SystemOne paid: jev needs a native {state, questions} envelope."""
    env = json.dumps({
        "state": "e2e-probe",
        "questions": {"q1": {"type": "noul", "instructions": "e2e-probe: reply PONG"}},
    })
    st, h, raw = post_chat(base, "jev", prompt=env)
    t = text_of(raw)
    if st == 200 and t:
        return True, f"200 text={t[:60]!r}"
    return False, f"status={st} raw={raw[:200]!r}"


def case4(base):
    """jev-free: free-first with paid fallback still answers (envelope shape)."""
    env = json.dumps({
        "state": "e2e-probe",
        "questions": {"q1": {"type": "noul", "instructions": "e2e-probe: reply PONG"}},
    })
    st, h, raw = post_chat(base, "jev-free", prompt=env)
    t = text_of(raw)
    if st == 200 and t:
        return True, f"200 echo={hdr(h, POOL_HDR)!r} text={t[:60]!r}"
    return False, f"status={st} raw={raw[:200]!r}"


def case5(base):
    """muse-free: responses endpoint mapped back to chat completion text."""
    st, h, raw = post_chat(base, "muse-free", stream=True)
    t = sse_text_of(raw) or text_of(raw)
    if st == 200 and t:
        return True, f"200 text={t[:60]!r}"
    return False, f"status={st} raw={raw[:200]!r}"


def case6(base):
    """Isolation: paid deepseek-flash must NOT carry pool session echo."""
    st, h, raw = post_chat(base, "deepseek-flash")
    st2, h2, raw2 = post_chat(base, "deepseek-flash")
    sess = hdr(h, POOL_HDR) or hdr(h2, POOL_HDR)
    if st == 200 and st2 == 200 and not sess:
        return True, "paid clean x2, no pool echo"
    return False, f"status={st}/{st2} echo={sess!r}"


def case7(base):
    """Failure drill: unknown model fails closed (4xx), no panic/restart."""
    st, h, raw = post_chat(base, "e2e-no-such-model")
    if 400 <= st < 500:
        return True, f"fail-closed {st}"
    return False, f"expected 4xx got status={st} raw={raw[:200]!r}"


def case8(base):
    """Stickiness: same thread-pinned request x3.

    Echo headers are stripped by the host (see case2 note), so this asserts
    availability x3 and reports echo values opportunistically: identical
    non-empty echoes prove stickiness when a future host forwards them.
    """
    pinned = {"X-Zen-Pool-Session": "e2e-sticky-probe"}
    got = []
    for _ in range(3):
        st, h, raw = post_chat(base, "mimo-free", headers=pinned)
        if st != 200 or not text_of(raw):
            return False, f"attempt status={st} raw={raw[:200]!r}"
        got.append(hdr(h, POOL_HDR))
    if got[0] and got[0] == got[1] == got[2]:
        return True, f"sticky proven session={got[0]} x3"
    return True, f"3x200 ok, echo unobservable (host strips headers): {got}"


CASES = {1: case1, 2: case2, 3: case3, 4: case4, 5: case5, 6: case6, 7: case7, 8: case8}


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--base", default="http://127.0.0.1:8317")
    ap.add_argument("--case", required=True, help="1..8 or all")
    a = ap.parse_args()
    ids = sorted(CASES) if a.case == "all" else [int(a.case)]
    failed = False
    for i in ids:
        try:
            ok, detail = CASES[i](a.base)
        except Exception as e:  # transport/timeout -> FAIL, never crash the runner
            ok, detail = False, f"exception {type(e).__name__}: {e}"
        print(json.dumps({"case": i, "ok": ok, "detail": detail}))
        failed = failed or not ok
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
