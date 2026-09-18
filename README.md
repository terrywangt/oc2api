# oc2api

> ⚠️ **2026-08-21 提醒**：`deepseek-v4-flash-free` 模型已官方下线，不再提供免费额度。如需使用请更换其他模型，如 `big-pickle`、`mimo-v2.5-free`、`hy3-free` 等。

OpenCode API 代理，支持 SSE 流式响应，可部署到 Vercel / Railway / Render / Cloudflare Worker。

如需本地部署或部署到其他云平台，参见 [server/](./server/) 目录（Go 版）。

---

## 部署

### Vercel 部署（推荐起步）

[![Deploy with Vercel](https://vercel.com/button)](https://vercel.com/new/clone?repository-url=https%3A%2F%2Fgithub.com%2Fzhuweiyou%2Foc2api&env=API_KEY%2CDEBUG&envDefaults=%7B%22API_KEY%22%3A%22sk-zhu%22%2C%22DEBUG%22%3A%22true%22%7D&envDescription=API_KEY%EF%BC%9AAPI%20%E5%AF%86%E9%92%A5%EF%BC%88%E7%95%99%E7%A9%BA%E5%88%99%E5%8C%BF%E5%90%8D%E8%AE%BF%E9%97%AE%EF%BC%89%EF%BC%9BDEBUG%EF%BC%9A%E8%AE%BE%E4%B8%BA%20true%20%E5%BC%80%E5%90%AF%E8%B0%83%E8%AF%95%E6%97%A5%E5%BF%97&envLink=https%3A%2F%2Fgithub.com%2Fzhuweiyou%2Foc2api%23%E9%83%A8%E7%BD%B2)

#### 手动部署

1. Fork 本仓库到你的 GitHub
2. 打开 [Vercel Dashboard](https://vercel.com)，点击 **Add New > Project**
3. 选择你 Fork 的仓库，点击 **Import**
4. 在 **Environment Variables** 中添加：
    - `API_KEY` — API 密钥（留空则匿名访问）
    - `DEBUG` — 设为 `true` 开启调试日志（可选）
    - `SHOW_REASONING` — 控制是否将模型思考内容（`reasoning_content`）转发给客户端。**默认留空 = 省流量模式（不转发思考内容）**，设为 `true` 则透传思考内容（可选）
    - `REASONING_EFFORT` — 覆盖上游推理强度，可选 `low` / `medium` / `high` / `max`，**默认 `high`**（可选。设为 `low` 可让模型少想、减少上游 token 生成量与响应字节）
5. 点击 **Deploy**，等待部署完成

部署完成后会得到一个 `https://<项目名>.vercel.app` 的域名。

#### Vercel 额度说明（Hobby 免费档）

| 资源 | 免费额度 | 说明 |
|------|---------|------|
| **Fast Origin Transfer** | **10 GB/月** | ⚠️ 关键瓶颈。Function 与 CDN 之间的数据传输（含请求体 + 响应体），对于 SSE 流式代理，**每次聊天的全部输出都计入此处** |
| **Fast Data Transfer** | 100 GB/月 | CDN 到终端用户的下载流量 |
| **Edge Requests** | 100 万/月 | CDN 处理的请求总数 |
| **Function Duration** | 300s（5 分钟） | 单次函数执行时长上限 |
| **Build 次数** | 100 次/天 | 每次 push 触发的构建次数 |
| **部署数** | 无硬限制 | 但超过合理范围会被限流 |

**⚠️ Origin Transfer 详解：**

Vercel 将每次函数调用的 **请求 + 响应** 数据都计入 Fast Origin Transfer，具体计算方式为：

- **incoming**：CDN → Function 的 HTTP 请求（POST body 大小）
- **outgoing**：Function → CDN 的 HTTP 响应（**包括完整的 SSE 流式输出**）

对于 DeepSeek 推理模型：
- 128K tokens 输出 ≈ **0.8~1 MB** Origin Transfer（含 SSE 开销）
- 32K tokens 输出 ≈ **~150 KB** Origin Transfer
- **10 GB / 月 ≈ 10,000 次满输出**，或 **65,000 次重推理输出**

超出 10 GB 后 **函数调用直接返回 503 停服**，无法追加购买。Hobby 档不可升级 Origin Transfer 额度。

#### 如何节省 Origin Transfer（重点）

SSE 流式输出是 Origin Transfer（Function → CDN）的大头，而推理模型的 `reasoning_content`（思考内容）往往占比巨大。**本项目默认开启省流量模式，不再把思考内容转发给客户端**，实测可将单次对话的出站体积普遍压低约 60~80%（思考 tokens 常占一次输出的绝大部分）。

两个开关（部署时在 Vercel Environment Variables 配置）：

| 环境变量 | 默认 | 取值 | 作用 |
|---------|------|------|------|
| `SHOW_REASONING` | 留空（关闭） | `true` 时开启 | 是否把 `reasoning_content` 思考内容转发给客户端。**默认关闭 = 省流量**；设为 `true` 则前端可看到思考过程，但出站流量回到原状 |
| `REASONING_EFFORT` | `high` | `low`/`medium`/`high`/`max` | 控制上游模型思考强度。设为 `low` 让模型少想，进一步减少上游 token 生成量与响应字节（注意：这影响的是上游生成，不影响 Vercel 计费，但与关思考叠加省得最多） |

**说明：**
- 关闭思考转发省的是 **Vercel → 客户端** 的那段（Fast Origin Transfer 计费点），这是立竿见影的。
- 上游（opencode.ai → Vercel）这段思考字节**无法省**：模型在生成时已产生，且无法"内部思考但不落字节"。`REASONING_EFFORT=low` 只能降低上游生成量、加快响应。
- 已保留 `extractThinkBlocks` 逻辑：当 `SHOW_REASONING=true` 且全量响应时，仍会把 `content` 里的 `</think>` 思考抽回 `reasoning_content`，保证展示友好。

#### Vercel 多项目 IP 扩展

你可以 Fork 后部署多个 Vercel Project，以创建多个出口 IP 不同的项目，然后在 [router-for-me/CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI/blob/main/README_CN.md#%E5%8A%9F%E8%83%BD%E7%89%B9%E6%80%A7)、[Wei-Shaw/sub2api](https://github.com/Wei-Shaw/sub2api/blob/main/README_CN.md#%E9%83%A8%E7%BD%B2%E6%96%B9%E5%BC%8F)、[QuantumNous/new-api](https://github.com/QuantumNous/new-api/blob/main/README.zh_CN.md#-%E5%BF%AB%E9%80%9F%E5%BC%80%E5%A7%8B) 等工具中配置多个域名实现轮询，规避 IP 限制。每个 Hobby 项目各有独立的 10 GB Origin Transfer 额度。

---

### Railway 部署（独立 IP，流量宽裕）

[![Deploy on Railway](https://railway.com/button)](https://railway.com/new?template=https%3A%2F%2Fgithub.com%2Fterrywangt%2Foc2api)

#### 手动部署

1. 注册 [Railway](https://railway.com) 账号，用 GitHub 登录
2. 点击 **New Project > Deploy from GitHub Repo**
3. 选择 `terrywangt/oc2api`
4. Railway 自动识别 `Dockerfile` 并构建
5. 在 **Variables** 中添加：
    - `API_KEY` — API 密钥（留空则匿名访问）
    - `DEBUG` — 设为 `true` 开启调试日志（可选）
6. 等待构建完成后，点击 **Settings > Networking > Generate Domain** 获取公网地址

#### Railway 额度说明

| 计划 | 月费 | 流量 | 内存 | 说明 |
|------|------|------|------|------|
| **Trial** | $5 一次性试用 | 包含 | 512 MB | 新账号免费获得 $5 额度 |
| **Hobby（$0/月）** | $0 | 500 GB/月 出站 | 512 MB | ⚡ 无 Origin Transfer 类限制，流量直接从实例出站 |
| **Pro（$5/月）** | $5+ 用量 | $0.0004/MB | 按需升级 | 超出后按量计费 |

**✅ 相比 Vercel 的核心优势：**
- **没有 Origin Transfer 概念**：Railway 是常驻容器，不区分 CDN → Function 边界，所有出站流量统一计入带宽
- **500 GB/月免费**（Hobby $0/月），比 Vercel 10 GB Origin Transfer 宽 **50 倍**
- **每个实例独立 IP**，比 Vercel 共享段更干净，更适合代理用途
- **无冷启动**：实例常驻，不休眠
- **无请求超时**：单次连接可持续数小时，SSE 长连接不受 5 分钟限制

---

### Render 部署（免费容器，750 小时/月）

[![Deploy to Render](https://render.com/images/deploy-to-render-button.svg)](https://render.com/deploy?repo=https://github.com/terrywangt/oc2api)

#### 手动部署

1. 注册 [Render](https://render.com) 账号，用 GitHub 登录
2. 点击 **New > Web Service**
3. 选择 `terrywangt/oc2api` 仓库
4. 设置：
    - **Runtime**：Docker
    - **Instance Type**：Free
5. 在 **Environment** 中添加：
    - `API_KEY` — API 密钥（留空则匿名访问）
    - `DEBUG` — 设为 `true` 开启调试日志（可选）
6. 点击 **Create Web Service**，等待构建完成

#### Render 额度说明

| 计划 | 月费 | 流量 | 运行时长 | 说明 |
|------|------|------|---------|------|
| **Free** | $0 | 包含在计划内 | 750 小时/月 | ⚠️ 免费实例 15 分钟无请求会休眠，首次访问有冷启动 |
| **Starter** | $7/月 | 包含在计划内 | 持续运行 | 无休眠，低延迟 |
| **Standard** | $25/月 | 包含在计划内 | 持续运行 | 更高资源配额 |

**✅ Render 特点：**
- 免费 750 小时/月 ≈ 31 天全月可用（单实例）
- 带宽包含在计划内，**不按流量额外计费**
- **独立静态 IP**，出口 IP 固定
- Docker 构建，与 Railway 部署同一份 Dockerfile
- ⚠️ Free 计划实例不活跃 15 分钟会休眠，首次请求有冷启动延迟

---

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

#### Cloudflare Worker 额度说明

| 计划 | 月费 | 请求数 | CPU 时间 | 带宽 |
|------|------|-------|---------|------|
| **Free** | $0 | 10 万/天 | 10ms/请求 | **无限制（含在计划）** |
| **Paid（$5/月）** | $5 | 1000 万/天 | 30M ms/月 | **无限制** |

> ⚠️ Cloudflare Workers 出站走 Cloudflare 共享 IP 池，多个 Worker 的出口 IP 可能相同，适合作为「补充出口」，扩容主力仍是 Vercel/Railway 多项目。部分区域（如中国大陆 IP）可能受限。

---

### 各平台额度对比

| 平台 | 月费 | 有效带宽/月 | 出口 IP | 冷启动 | SSE 长连接 | 适合场景 |
|------|------|------------|--------|--------|-----------|---------|
| **Vercel Hobby** | $0 | 10 GB Origin Transfer | 共享 AWS 段 | 有 | 5 分钟超时 | 轻量测试，多 IP 轮询 |
| **Railway Hobby** | $0 | **500 GB** | 独立实例 IP | 无 | 无限制 | ⭐ **代理主力**，流量大 |
| **Render Free** | $0 | 包含 | 独立静态 IP | 15 分钟休眠 | 无限制 | 低频备用 |
| **CF Worker Free** | $0 | 无限制 | 共享 CF 池 | 无 | 无限制 | 补充出口 IP |
| **Vercel Pro** | $20 | 按量 $0.06/GB | 共享 AWS 段 | 有 | 5 分钟超时 | 付费保底 |
| **Railway Pro** | $5+ | 按量 $0.0004/MB | 独立实例 IP | 无 | 无限制 | 大规模流量 |

---

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

提供与 AI 接口**同一套认证**的正向 HTTP 代理，目标请求从部署平台出口 IP 发出（Vercel / Railway / Render / Cloudflare Worker 均可）。

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

代理仅放行免费模型（`big-pickle` 及所有以 `-free` 结尾的模型），以 `big-pickle` 为例：

```json
{
  "id": "big-pickle",
  "limit": {
    "context": 200000,
    "output": 32000
  }
}
```

- `context`：最大上下文窗口，**200,000** tokens
- `output`：最大单次输出长度，**32,000** tokens

以上限制数据来源于接口 [https://models.opencode.ai/api.json](https://models.opencode.ai/api.json)（`opencode` key 下对应模型的 `limit` 字段），可自行查看核实，以实际使用为准。
