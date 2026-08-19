# oc2api

OpenCode API 代理，部署在 Vercel，支持 SSE 流式响应。

如需本地部署或部署到其他云平台，参见 [server/](./server/) 目录。

## 部署

### 一键部署

[![Deploy with Vercel](https://vercel.com/button)](https://vercel.com/new/clone?repository-url=https%3A%2F%2Fgithub.com%2Fzhuweiyou%2Foc2api&env=API_KEY%2CDEBUG&envDefaults=%7B%22API_KEY%22%3A%22sk-zhu%22%2C%22DEBUG%22%3A%22true%22%7D&envDescription=API_KEY%EF%BC%9AAPI%20%E5%AF%86%E9%92%A5%EF%BC%88%E7%95%99%E7%A9%BA%E5%88%99%E5%8C%BF%E5%90%8D%E8%AE%BF%E9%97%AE%EF%BC%89%EF%BC%9BDEBUG%EF%BC%9A%E8%AE%BE%E4%B8%BA%20true%20%E5%BC%80%E5%90%AF%E8%B0%83%E8%AF%95%E6%97%A5%E5%BF%97&envLink=https%3A%2F%2Fgithub.com%2Fzhuweiyou%2Foc2api%23%E9%83%A8%E7%BD%B2)

### 手动部署

1. Fork 本仓库到你的 GitHub
2. 打开 [Vercel Dashboard](https://vercel.com)，点击 **Add New > Project**
3. 选择你 Fork 的仓库，点击 **Import**
4. 在 **Environment Variables** 中添加：
    - `API_KEY` — API 密钥（留空则匿名访问）
    - `DEBUG` — 设为 `true` 开启调试日志（可选）
5. 点击 **Deploy**，等待部署完成

部署完成后会得到一个 `https://<项目名>.vercel.app` 的域名。

你可以 Fork 后部署多个 Vercel Project，以创建多个出口 IP 不同的项目，然后在 [router-for-me/CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI/blob/main/README_CN.md#%E5%8A%9F%E8%83%BD%E7%89%B9%E6%80%A7)、[Wei-Shaw/sub2api](https://github.com/Wei-Shaw/sub2api/blob/main/README_CN.md#%E9%83%A8%E7%BD%B2%E6%96%B9%E5%BC%8F)、[QuantumNous/new-api](https://github.com/QuantumNous/new-api/blob/main/README.zh_CN.md#-%E5%BF%AB%E9%80%9F%E5%BC%80%E5%A7%8B) 等工具中配置多个域名实现轮询，规避 IP 限制。

### Cloudflare Worker 部署

同一份代码也可以部署到 Cloudflare Worker（免费计划：10 万请求/天），出口走 Cloudflare 网络，与 Vercel（AWS）IP 段不同，可进一步扩大 IP 池多样性：

1. 安装 wrangler：`npm i -g wrangler`（或直接用 `npx wrangler`）
2. 认证：`wrangler login`，或用环境变量 `CLOUDFLARE_API_KEY` + `CLOUDFLARE_EMAIL`（Global API Key）
3. 设置 API Key 为 Worker Secret（**不要写进 `wrangler.toml`**，避免密钥进入仓库泄露），然后部署：

```bash
echo "你的-api-key" | npx wrangler secret put API_KEY --name oc2api-worker-1
echo "你的-api-key" | npx wrangler secret put API_KEY --name oc2api-worker-2
npx wrangler deploy --name oc2api-worker-1   # 第一个 Worker
npx wrangler deploy --name oc2api-worker-2   # 第二个 Worker（同样代码重复部署）
```

4. 部署完成得到 `https://<name>.<你的子域>.workers.dev`，即可当作 OpenAI 兼容端点使用（同样支持下面的 HTTP 代理接口）

> 注意：Cloudflare Workers 出站走 Cloudflare 共享 IP 池，多个 Worker 的出口 IP 可能相同，适合作为「补充出口」，扩容主力仍是 Vercel 多项目。

## API

兼容 OpenAI API 格式，路径均支持带 `/v1` 前缀或不带：

| 路径                                           | 方法   | 说明                            |
|----------------------------------------------|------|-------------------------------|
| `/v1/chat/completions` 或 `/chat/completions` | POST | Chat 补全（支持 `stream: true` 流式） |
| `/v1/models` 或 `/models`                     | GET  | 模型列表                          |
| `/` 或 `/health`                             | GET  | 健康检查                          |
| `/ip`                                        | GET  | 查询出口 IP                       |
| `/proxy`                                     | 任意 | HTTP 代理接口（见下文）              |

携带 API Key（如已配置）：

```
Authorization: Bearer <api-key>
```

## HTTP 代理接口

提供与 AI 接口**同一套认证**的正向 HTTP 代理，目标请求从部署平台出口 IP 发出（Vercel / Cloudflare Worker 均可）。

支持三种调用方式：

```bash
# 1. query 参数指定目标（推荐，最可靠）
curl "https://<你的域名>/proxy?url=https%3A%2F%2Fapi.ipquery.io%2F" \
  -H "Authorization: Bearer <api-key>"

# 2. 路径中直接携带目标 URL（/proxy/ 后接完整 URL）
curl "https://<你的域名>/proxy/https://api.ipquery.io/" \
  -H "Authorization: Bearer <api-key>"

# 3. 标准 HTTP 代理 absolute-form（curl -x 直达，目标 host 非本域名时自动识别）
curl -x "https://<你的域名>:443" -U "user:<api-key>" https://api.ipquery.io/
```

特性：

- **任意 HTTP 方法**（GET / POST / PUT / DELETE…），请求体与响应体完整透传（流式）
- **认证**：`Authorization: Bearer <api-key>`、`X-API-Key`、`Proxy-Authorization: Basic`（密码等于 API_KEY）均可；未配置 `API_KEY` 时匿名
- **安全**：仅允许 `http://` / `https://` 目标；自动拦截内网/本机地址（127.x、10.x、192.168.x、172.16-31.x、localhost 等），防止 SSRF
- **限制**：平台不支持 TCP 隧道（CONNECT），因此仅代理 HTTP(S) 请求，不能当 VPN/SSH 隧道用

## 免费模型限制

代理仅放行免费模型（`big-pickle` 及所有以 `-free` 结尾的模型），以 `deepseek-v4-flash-free` 为例：

```json
{
  "id": "deepseek-v4-flash-free",
  "limit": {
    "context": 200000,
    "output": 128000
  }
}
```

- `context`：最大上下文窗口，**200,000** tokens
- `output`：最大单次输出长度，**128,000** tokens

以上限制数据来源于接口 [https://models.opencode.ai/api.json](https://models.opencode.ai/api.json)（`opencode` key 下对应模型的 `limit` 字段），可自行查看核实，以实际使用为准。

## 推理强度（reasoning_effort）

目前 `reasoning_effort` 仅对 DeepSeek 模型生效：DeepSeek 只接受 `high` / `max`，两者原样透传，其余值（包括未指定）会被强制为 `high`；其他模型不处理该参数，保持默认值。

## 图片请求

DeepSeek 仅支持文本输入。当**最近一条 `user` 消息**包含图片（`type` 为 `image_url` 或 `image` 的 content part）时，代理会把该请求路由到带图模型 `mimo-v2.5-free` 处理，并在响应中把 `model` 字段改写为您请求的 DeepSeek 模型，对客户端透明。

- 路由只由**最近一条 user 消息**决定：历史中残留的图片不会再次触发回退。
- 因此「发图提问 → 拿到结果后纯文字追问」的下一轮会**自动回到 DeepSeek**，不会一直走 `mimo-v2.5-free`。
