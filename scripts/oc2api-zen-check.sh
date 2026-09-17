#!/usr/bin/env bash
# oc2api-zen-check.sh — 区分「Zen 上游免费层门禁」与「本地 oc2api 故障」
# 用法: ./oc2api-zen-check.sh
# 退出码: 0=上游正常  10=上游免费层门禁(429/403 FreeTier/FreeUsageLimit)  1=本地故障
set -uo pipefail

SES="ses_$(python3 -c "import secrets,string;print(secrets.token_hex(6)+''.join(secrets.choice(string.ascii_letters+string.digits) for _ in range(10)))")"
UA="opencode/1.18.31 ai-sdk/provider-utils/4.0.40 runtime/bun/1.3.14"

probe() {
  curl -s -m 30 -o /tmp/oczen.json -w "%{http_code}" \
    -X POST "https://opencode.ai/zen/v1/chat/completions" \
    -H "Content-Type: application/json" \
    -H "Authorization: Bearer public" \
    -H "User-Agent: $UA" \
    -H "x-opencode-session: $SES" \
    -H "x-opencode-client: cli" \
    -H "x-opencode-project: global" \
    -d '{"model":"big-pickle","messages":[{"role":"user","content":"ping"}],"max_tokens":5}'
}

CODE=$(probe)
BODY=$(head -c 200 /tmp/oczen.json)

echo "[zen] HTTP $CODE  $BODY"
case "$CODE" in
  200) echo "[zen] OK — 上游免费层可用"; exit 0 ;;
  429|403) echo "[zen] 上游门禁/限流（非本地故障）: FreeTierError/FreeUsageLimitError" ; exit 10 ;;
  *) echo "[zen] 异常状态码，需人工复核"; exit 1 ;;
esac
