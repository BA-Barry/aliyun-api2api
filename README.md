# Aliyun API2API

一个面向阿里云百炼（DashScope）的轻量 API 网关：客户端只接触你签发的网关 Key，服务端从多个真实阿里云 API Key 中选择上游，改写鉴权并透明转发请求。

项目参考 [SUB2API](https://github.com/Wei-Shaw/sub2api) 的账号池、调度和故障转移思路，采用单进程 Go 实现，不依赖数据库、Redis 或管理前端，适合个人及可信小团队部署。

## 功能

- OpenAI 兼容路由：`/v1/*`、`/compatible-mode/v1/*`
- Anthropic 兼容路由：`/apps/anthropic/*`、`/anthropic/*`、`/v1/messages*`
- `least_inflight`（默认）与加权 `round_robin` 调度
- 网络错误、HTTP 408、429、5xx 自动切换其他 Key
- 失效、欠费或余额不足的 Key 自动进入较长冷却
- 全局及单 Key 并发硬限制，超限快速返回 HTTP 429
- SSE 流式透传
- `/healthz`、`/readyz`、受管理员 Key 保护的 `/admin/status` 和 `/metrics`
- 强制 HTTPS 上游及阿里云域名白名单
- Docker secrets、非 root、只读文件系统、capabilities 全部移除
- 不记录请求正文、响应正文或真实 API Key

## 默认上游地址

| 协议 | 默认 Base URL |
|---|---|
| OpenAI 兼容 | `https://dashscope.aliyuncs.com/compatible-mode/v1` |
| Anthropic 兼容 | `https://dashscope.aliyuncs.com/apps/anthropic` |

如果 Key 属于业务空间专属地址，可在对应 upstream 中单独设置：

```json
{
  "id": "workspace-key-1",
  "api_key": "${ALIYUN_API_KEY_1}",
  "openai_base_url": "https://{WorkspaceId}.cn-beijing.maas.aliyuncs.com/compatible-mode/v1",
  "anthropic_base_url": "https://{WorkspaceId}.cn-beijing.maas.aliyuncs.com/apps/anthropic",
  "weight": 1
}
```

Key、地域、业务空间、模型权限及计费类型必须与地址匹配。同一池中的 Key 最好支持相同的模型集合。

## Docker 部署

要求 Docker Engine 与 Docker Compose v2。

```bash
git clone https://github.com/BA-Barry/aliyun-api2api.git
cd aliyun-api2api

cp config.example.json config.json
cp .env.example .env
install -d -m 700 secrets
```

创建四个密钥文件。文件中只放 Key 本身，不要写变量名或引号：

```bash
printf 'sk-gw-%s' "$(openssl rand -hex 32)" > secrets/gateway_api_key
printf 'sk-admin-%s' "$(openssl rand -hex 32)" > secrets/admin_api_key

# 用编辑器分别填入真实阿里云 Key
nano secrets/aliyun_api_key_1
nano secrets/aliyun_api_key_2

chmod 700 secrets
chmod 444 secrets/*
chmod 600 .env
```

启动并检查：

```bash
docker compose up -d --build
docker compose ps
curl http://127.0.0.1:8080/healthz
curl http://127.0.0.1:8080/readyz
```

Compose 默认只绑定宿主机 `127.0.0.1:8080`。不要直接把该端口暴露到公网；对外服务请使用 Nginx/Caddy HTTPS 反向代理或加密私网。

## 客户端使用

假设公网域名为 `https://api.example.com`。

OpenAI SDK：

```python
from openai import OpenAI

client = OpenAI(
    api_key="你的网关Key",
    base_url="https://api.example.com/v1",
)

response = client.chat.completions.create(
    model="qwen-plus",
    messages=[{"role": "user", "content": "你好"}],
)
print(response.choices[0].message.content)
```

Anthropic SDK：

```python
from anthropic import Anthropic

client = Anthropic(
    api_key="你的网关Key",
    base_url="https://api.example.com/apps/anthropic",
)

message = client.messages.create(
    model="qwen-plus",
    max_tokens=256,
    messages=[{"role": "user", "content": "你好"}],
)
print(message.content)
```

## 多用户网关 Key

`secrets/gateway_api_key` 可以保存一个 Key，也可以保存英文逗号分隔的多个 Key：

```text
sk-user-a-强随机值,sk-user-b-强随机值,sk-user-c-强随机值
```

修改后重建容器以重新读取 secrets：

```bash
docker compose up -d --force-recreate api2api
```

每位用户只应获得自己的网关 Key。不要向用户提供真实阿里云 Key、管理员 Key、服务器 SSH 私钥或 Docker 管理权限。

## 主要配置

| 配置 | 示例默认值 | 说明 |
|---|---:|---|
| `strategy` | `least_inflight` | 最少并发或 `round_robin` |
| `max_attempts` | `3` | 单次请求最多尝试的不同上游数 |
| `max_concurrent_requests` | `10` | 全局活动请求硬上限 |
| `max_concurrent_per_upstream` | `5` | 单个 Key 的活动请求硬上限 |
| `max_request_body_bytes` | `4194304` | 最大请求体 4 MiB |
| `base_cooldown` | `30s` | 临时错误基础冷却时间 |
| `credential_cooldown` | `30m` | 凭证或余额错误冷却时间 |

增加 Key、为不同 Key 配置不同地址或调整权重时，请编辑 `config.json`，并在 Compose 中增加对应 secret 挂载。

## 管理接口

```bash
ADMIN_KEY="$(cat secrets/admin_api_key)"

curl -H "Authorization: Bearer ${ADMIN_KEY}" \
  http://127.0.0.1:8080/admin/status

curl -H "Authorization: Bearer ${ADMIN_KEY}" \
  http://127.0.0.1:8080/metrics
```

状态计数和冷却状态保存在内存中，进程重启后清零。

## 测试

```bash
go test ./...
go vet ./...
```

## 安全提示

- 任何曾出现在聊天、工单、截图或 Git 历史中的真实 Key 都应立即撤销。
- `secrets/`、`.env`、私钥和生产 `config.json` 已被 `.gitignore` 排除。
- 公网只使用 HTTPS；不要公开 Docker API 或网关内部端口。
- 为阿里云账号配置费用告警、消费上限和最小权限。
- 自动重试存在极小的重复生成或重复计费窗口。
- 本项目不提供按用户额度、计费、审计留存或严格多租户隔离。

## License

[MIT](LICENSE)
