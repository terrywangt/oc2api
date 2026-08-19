# oc2api

纯 Go 实现的 OpenCode API 代理服务，支持 SSE 流式响应。

## 配置

编辑 `config.yaml`：

| 配置项          | 默认值            | 说明               |
|--------------|----------------|------------------|
| `port`       | `8080`         | 监听端口             |
| `api-key`    | 空              | API 密钥（不设置则匿名访问） |
| `debug`      | `false`        | 调试日志             |
| `timeout-ms` | `300000` (5分钟) | 上游请求超时时间         |

## 本地部署

```bash
go build -ldflags="-s -w" -trimpath -o main main.go
./main
```

## Docker 部署

```bash
docker compose up -d
```

## Serverless 部署

本地/Docker 部署出口 IP 固定，建议部署到阿里云函数计算、腾讯云函数 等 Serverless 环境实现多出口 IP 轮询，规避 IP 限制。

```bash
GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -trimpath -o main main.go
# 将 main + config.yaml 上传至云函数代码空间
# chmod +x ./main
# 启动命令: ./main
# 监听端口: 8080
```

## API

接口与父项目完全一致，详见 [父项目 README](https://github.com/zhuweiyou/oc2api/#api)。
