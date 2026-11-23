"""Print captured flows from captures/live.jsonl with secrets redacted.

Usage: python -I tools/capture/show.py [--log FILE] [--method M] [--max N] [--ws] [--headers] PATH_REGEX
  PATH_REGEX is matched against the URL path; --ws prints WebSocket frames instead (regex matched on frame text).
"""

import argparse
import json
import re
import sys
import urllib.parse

JWT = re.compile(r"eyJ[\w-]+\.[\w-]+\.[\w-]+")
SECRET_KEYS = re.compile(r"(password|token|secret|ticket|authorization|cookie|private|seed|mnemonic|otp|code)", re.I)
NOISY_HEADERS = {"accept", "accept-encoding", "accept-language", "sec-ch-ua", "sec-ch-ua-mobile",
                 "sec-ch-ua-platform", "sec-fetch-dest", "sec-fetch-mode", "sec-fetch-site", "priority",
                 "connection", "content-length", "date", "vary", "server", "cf-ray", "alt-svc", "nel",
                 "report-to", "strict-transport-security", "x-content-type-options", "referer"}


def redact(v):
    if isinstance(v, dict):
        return {k: ("<redacted>" if SECRET_KEYS.search(k) and isinstance(val, str) else redact(val))
                for k, val in v.items()}
    if isinstance(v, list):
        return [redact(x) for x in v]
    if isinstance(v, str):
        return JWT.sub("<jwt>", v)
    return v


def body(b, limit):
    if not b:
        return None
    if "text" in b:
        try:
            return redact(json.loads(b["text"]))
        except ValueError:
            return JWT.sub("<jwt>", b["text"][:limit])
    return f"<binary {len(b.get('b64', '')) * 3 // 4} bytes>"


def headers(h):
    return {k: ("<redacted>" if SECRET_KEYS.search(k) else JWT.sub("<jwt>", v))
            for k, v in h.items() if k.lower() not in NOISY_HEADERS}


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("pattern")
    ap.add_argument("--log", default="captures/live.jsonl")
    ap.add_argument("--method")
    ap.add_argument("--max", type=int, default=5)
    ap.add_argument("--ws", action="store_true")
    ap.add_argument("--headers", action="store_true")
    ap.add_argument("--limit", type=int, default=4000)
    a = ap.parse_args()
    sys.stdout.reconfigure(encoding="utf-8")
    rx = re.compile(a.pattern)
    n = 0
    for line in open(a.log, encoding="utf-8"):
        r = json.loads(line)
        if a.ws:
            if r["kind"] != "ws" or not rx.search(r.get("text", "")):
                continue
            t = r.get("text")
            try:
                t = redact(json.loads(t))
            except (TypeError, ValueError):
                t = t or f"<binary {len(r['b64']) * 3 // 4} bytes>"
            out = {"ts": r["ts"], "dir": r["dir"], "frame": t}
        else:
            if r["kind"] != "http" or r["method"] == "OPTIONS":
                continue
            if a.method and r["method"] != a.method:
                continue
            p = urllib.parse.urlsplit(r["url"])
            if not rx.search(p.path):
                continue
            out = {"ts": r["ts"], "req": f"{r['method']} {p.path}" + (f"?{JWT.sub('<jwt>', p.query)}" if p.query else ""),
                   "status": r["status"], "req_body": body(r["req_body"], a.limit),
                   "resp_body": body(r["resp_body"], a.limit)}
            if a.headers:
                out["req_headers"] = headers(r["req_headers"])
                out["resp_headers"] = headers(r["resp_headers"])
        s = json.dumps(out, ensure_ascii=False, indent=1)
        print(s[: a.limit] + ("\n...<truncated>" if len(s) > a.limit else ""))
        n += 1
        if n >= a.max:
            break
    print(f"[{n} shown]", file=sys.stderr)


if __name__ == "__main__":
    main()
