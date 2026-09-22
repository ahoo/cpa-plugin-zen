#!/usr/bin/env python3
"""Mint trusted Zen sessions via the genuine opencode CLI.

Usage: mint_sessions.py [count] [--pool PATH]
Appends {"id": ses_..., "fails": 0, "cooldown_until": 0} entries (dedupe).
"""
import json
import os
import re
import subprocess
import sys
import time

POOL = os.environ.get(
    "SESSION_POOL", "/home/ubuntu/workspace/cliproxyapi/plugins/zen-sessions.json"
)
COUNT = int(sys.argv[1]) if len(sys.argv) > 1 else 10


def load():
    if os.path.exists(POOL):
        with open(POOL) as f:
            return json.load(f)
    return {"sessions": []}


def save(pool):
    tmp = POOL + ".tmp"
    with open(tmp, "w") as f:
        json.dump(pool, f)
    os.replace(tmp, POOL)


def mint_one():
    p = subprocess.run(
        ["opencode", "run", "hi", "-m", "opencode/mimo-v2.5-free", "--format", "json"],
        capture_output=True,
        text=True,
        timeout=150,
    )
    m = re.search(r'"sessionID":"(ses_[A-Za-z0-9]+)"', p.stdout)
    return m.group(1) if m else None


def main():
    pool = load()
    known = {s["id"] for s in pool.get("sessions", [])}
    minted = 0
    for i in range(1, COUNT + 1):
        try:
            sid = mint_one()
        except Exception as ex:
            print(f"[{i}/{COUNT}] FAILED: {ex}", flush=True)
            continue
        if not sid:
            print(f"[{i}/{COUNT}] FAILED: no session id", flush=True)
            continue
        if sid in known:
            print(f"[{i}/{COUNT}] dup {sid}", flush=True)
            continue
        pool.setdefault("sessions", []).append(
            {"id": sid, "fails": 0, "cooldown_until": 0}
        )
        known.add(sid)
        save(pool)
        minted += 1
        print(f"[{i}/{COUNT}] minted {sid} (pool={len(pool['sessions'])})", flush=True)
        time.sleep(2)
    print(f"minted={minted}")


if __name__ == "__main__":
    main()
