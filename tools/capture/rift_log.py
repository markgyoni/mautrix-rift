"""mitmproxy addon: log Rift traffic to captures/live.jsonl, one JSON object per line.

Full request/response bodies and WebSocket frames for hosts matching RIFT_HOST_HINTS;
just a one-line URL record for everything else, so unknown CDN/API hosts are still visible.

Run: mitmdump -q --listen-host 127.0.0.1 -p 8080 -s tools/capture/rift_log.py -w captures/flows.mitm
"""

import base64
import json
import os
import threading
import time

from mitmproxy import http

RIFT_HOST_HINTS = ("rift",)
MAX_BODY = 512 * 1024
OUT_PATH = os.environ.get("RIFT_LOG", os.path.join("captures", "live.jsonl"))

os.makedirs(os.path.dirname(OUT_PATH), exist_ok=True)
_lock = threading.Lock()
_out = open(OUT_PATH, "a", encoding="utf-8")


def _write(rec: dict) -> None:
    rec["ts"] = round(time.time(), 3)
    with _lock:
        _out.write(json.dumps(rec, ensure_ascii=False) + "\n")
        _out.flush()


def _interesting(flow: http.HTTPFlow) -> bool:
    host = flow.request.pretty_host.lower()
    return any(h in host for h in RIFT_HOST_HINTS)


def _body(raw: bytes | None, ctype: str) -> dict:
    if not raw:
        return {}
    truncated = len(raw) > MAX_BODY
    raw = raw[:MAX_BODY]
    out: dict = {"len": len(raw), "truncated": truncated} if truncated else {}
    textual = any(t in ctype for t in ("json", "text", "javascript", "xml", "form"))
    if textual or not ctype:
        try:
            out["text"] = raw.decode("utf-8")
            return out
        except UnicodeDecodeError:
            pass
    out["b64"] = base64.b64encode(raw).decode()
    return out


def response(flow: http.HTTPFlow) -> None:
    req, resp = flow.request, flow.response
    if not _interesting(flow):
        _write({"kind": "other", "method": req.method, "url": req.pretty_url,
                "status": resp.status_code if resp else None})
        return
    _write({
        "kind": "http",
        "id": flow.id,
        "method": req.method,
        "url": req.pretty_url,
        "req_headers": dict(req.headers),
        "req_body": _body(req.get_content(strict=False), req.headers.get("content-type", "")),
        "status": resp.status_code if resp else None,
        "resp_headers": dict(resp.headers) if resp else {},
        "resp_body": _body(resp.get_content(strict=False), resp.headers.get("content-type", "")) if resp else {},
    })


def websocket_start(flow: http.HTTPFlow) -> None:
    if _interesting(flow):
        _write({"kind": "ws_open", "id": flow.id, "url": flow.request.pretty_url,
                "req_headers": dict(flow.request.headers)})


def websocket_message(flow: http.HTTPFlow) -> None:
    if not _interesting(flow) or flow.websocket is None:
        return
    msg = flow.websocket.messages[-1]
    rec = {"kind": "ws", "id": flow.id, "dir": "send" if msg.from_client else "recv"}
    if msg.is_text:
        rec["text"] = msg.text
    else:
        rec["b64"] = base64.b64encode(msg.content).decode()
    _write(rec)


def websocket_end(flow: http.HTTPFlow) -> None:
    if _interesting(flow):
        ws = flow.websocket
        _write({"kind": "ws_close", "id": flow.id,
                "code": ws.close_code if ws else None, "reason": ws.close_reason if ws else None})
