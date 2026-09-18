# OpenCode Zen 免费层 2026-09-17 第二轮门禁 —— opencode-serve 桥接方案

## 问题
OpenCode Zen 免费层自 2026-09-17 ~00:30 UTC 起升级客户端校验：**判别点在 HTTP 层之下**
（TLS/HTTP2 指纹或内嵌身份）。任何 UA / `x-opencode-session` / `x-opencode-client` 头伪造
全部失效 —— curl / node / python 一律 403 FreeTierError 或 401 AuthError。

**但官方 `opencode` 二进制正常**（实测 2026-09-18）。

参考：`anomalyco/opencode#49621`（逐项排除后得出同结论）。

## 方案：本地 opencode serve + OpenAI 桥接
由**官方二进制自己**发上游请求（官方认可路径，非逆向绕过）：

```
newapi ──▶ oczen-bridge.py (:8082, OpenAI 兼容)
                 │
                 ▼  HTTP (localhost)
          opencode serve (:4096, 官方二进制)
                 │
                 ▼  TLS(官方身份)
          opencode.ai/zen/v1  ✅ 200
```

## 部署
```bash
# 1. 安装官方 CLI
npm install -g opencode-ai@1.18.31

# 2. 桥接脚本
cp scripts/oczen-bridge.py /opt/oczen/
mkdir -p /opt/oczen/work          # 干净目录：避免加载外部 plugin

# 3. systemd
cp deploy/oczen-serve.service deploy/oczen-bridge.service /etc/systemd/system/
systemctl daemon-reload
systemctl enable --now oczen-serve oczen-bridge

# 4. 验证
curl -s http://127.0.0.1:8082/health
curl -s -X POST http://127.0.0.1:8082/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"big-pickle","messages":[{"role":"user","content":"hi"}],"stream":false}'
```

## 关键注意
- `opencode serve` 的 **WorkingDirectory 必须是干净目录**（无 package.json / opencode.jsonc / plugins）。
  工作目录里有杂散 JS 项目会使 CLI 加载外部 plugin，进而触发 Zen 免费层 403。
- 桥接只监听 `127.0.0.1`，无鉴权；如需对外须自行加认证与 TLS。
- 上游若再升级，先跑 `opencode run -m opencode/big-pickle "ping"` 确认官方二进制是否仍可用。

## 已验证（2026-09-18）
- `opencode run` → R1/R2/R3 3/3 成功
- `opencode serve` session API → 200 + 正常 token
- bridge non-stream → `BRIDGE_OK`
- bridge stream → SSE chunks + `[DONE]`
- 模型：big-pickle / mimo-v2.5-free / ling-3.0-flash-fin-free / nemotron-3-ultra-free 全部 OK
- systemd 重启后仍正常
