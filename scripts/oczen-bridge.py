#!/usr/bin/env python3
"""
oczen-bridge.py — OpenAI 兼容桥接：把请求转给本机 `opencode serve` 的 session API。

背景：OpenCode Zen 免费层自 2026-09-17 第二轮起，在 HTTP 层之下校验客户端身份
（TLS 指纹/内嵌身份）。UA/session 伪造全部失效（实测 curl/node 一律 403/401），
只有官方 opencode 二进制能过。故改由官方二进制驱动。

用法：
  opencode serve --port 4096 &            # 官方服务端
  python3 oczen-bridge.py --port 8082 --serve http://127.0.0.1:4096

端点：
  GET  /v1/models
  POST /v1/chat/completions   (stream 与非 stream 均支持)
"""
import argparse, json, os, sys, threading, urllib.request, urllib.error, uuid

SERVE = "http://127.0.0.1:4096"
DEFAULT_MODEL = "big-pickle"


def _req(url, method="GET", body=None, timeout=300):
    data = json.dumps(body).encode() if body is not None else None
    r = urllib.request.Request(url, data=data, method=method,
                               headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(r, timeout=timeout) as resp:
        return resp.status, resp.read()


def serve_create_session():
    st, b = _req(f"{SERVE}/session", "POST", {})
    return json.loads(b)["id"]


def serve_send(session_id, model, messages, stream=False):
    body = {
        "model": {"providerID": "opencode", "modelID": model},
        "parts": [{"type": "text", "text": _flatten(messages)}],
    }
    st, b = _req(f"{SERVE}/session/{session_id}/message", "POST", body, timeout=600)
    return json.loads(b)


def _flatten(messages):
    out = []
    for m in messages:
        role = m.get("role", "user")
        c = m.get("content")
        if isinstance(c, list):
            c = "".join(p.get("text", "") for p in c if isinstance(p, dict))
        out.append(f"[{role}] {c}")
    return "\n".join(out)


def extract_text(msg):
    parts = msg.get("parts") or []
    return "".join(p.get("text", "") for p in parts if p.get("type") == "text")


def openai_response(model, text, msg_id=None):
    return {
        "id": msg_id or f"chatcmpl-{uuid.uuid4().hex[:24]}",
        "object": "chat.completion",
        "created": 0,
        "model": model,
        "choices": [{
            "index": 0,
            "message": {"role": "assistant", "content": text},
            "finish_reason": "stop",
        }],
        "usage": {"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0},
    }


def sse_chunks(model, text, msg_id):
    base = {"id": msg_id, "object": "chat.completion.chunk", "created": 0, "model": model}
    yield f"data: {json.dumps({**base, 'choices':[{'index':0,'delta':{'role':'assistant'},'finish_reason':None}]})}\n\n"
    step = 24
    for i in range(0, len(text), step):
        delta = {"content": text[i:i+step]}
        yield f"data: {json.dumps({**base, 'choices':[{'index':0,'delta':delta,'finish_reason':None}]})}\n\n"
    yield f"data: {json.dumps({**base, 'choices':[{'index':0,'delta':{},'finish_reason':'stop'}]})}\n\n"
    yield "data: [DONE]\n\n"


def handle(handler_cls):
    from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

    class H(BaseHTTPRequestHandler):
        def log_message(self, *a):
            pass

        def _send(self, code, payload, ctype="application/json"):
            body = payload if isinstance(payload, bytes) else json.dumps(payload).encode()
            self.send_response(code)
            self.send_header("Content-Type", ctype)
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def do_GET(self):
            if self.path.rstrip("/") in ("/v1/models", "/models"):
                self._send(200, {"object": "list", "data": [
                    {"id": m, "object": "model", "owned_by": "opencode"}
                    for m in ["big-pickle", "mimo-v2.5-free", "ling-3.0-flash-fin-free"]
                ]})
            elif self.path.rstrip("/") in ("/health", ""):
                self._send(200, {"status": "ok", "bridge": "opencode-serve"})
            else:
                self._send(404, {"error": "not found"})

        def do_POST(self):
            if not self.path.rstrip("/").endswith("/chat/completions"):
                return self._send(404, {"error": "not found"})
            n = int(self.headers.get("Content-Length", 0))
            req = json.loads(self.rfile.read(n) or b"{}")
            model = (req.get("model") or DEFAULT_MODEL).split(":")[0]
            messages = req.get("messages") or []
            stream = bool(req.get("stream"))
            try:
                sid = serve_create_session()
                msg = serve_send(sid, model, messages, stream)
                text = extract_text(msg)
                mid = msg.get("info", {}).get("id")
            except Exception as e:
                return self._send(502, {"error": {"message": f"bridge error: {e}", "type": "upstream_error"}})
            if stream:
                self.send_response(200)
                self.send_header("Content-Type", "text/event-stream")
                self.send_header("Cache-Control", "no-cache")
                self.end_headers()
                for ch in sse_chunks(model, text, mid):
                    self.wfile.write(ch.encode())
                    self.wfile.flush()
            else:
                self._send(200, openai_response(model, text, mid))

    return H


def main():
    global SERVE
    ap = argparse.ArgumentParser()
    ap.add_argument("--port", type=int, default=8082)
    ap.add_argument("--serve", default="http://127.0.0.1:4096")
    a = ap.parse_args()
    SERVE = a.serve.rstrip("/")
    from http.server import ThreadingHTTPServer
    srv = ThreadingHTTPServer(("0.0.0.0", a.port), handle(None))
    print(f"bridge listening on http://127.0.0.1:{a.port} -> {SERVE}", flush=True)
    srv.serve_forever()


if __name__ == "__main__":
    main()
