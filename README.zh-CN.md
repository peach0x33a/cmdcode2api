# cmdcode2api 中文说明

[English](README.md)

`cmdcode2api` 是一个 OpenAI 兼容的小型网关，面向 [Command Code](https://commandcode.ai/)。OpenAI 风格的客户端调用熟悉的 `/v1/chat/completions` 与 `/v1/models`，由网关转发到 Command Code——在多个账号间轮换、统计用量，并内置管理 WebUI。

## 功能

- OpenAI 兼容的 `POST /v1/chat/completions`（流式与非流式）与 `GET /v1/models`
- 多 Command Code 账号轮询使用，`401`/`403`/`429`/`5xx` 自动故障转移，含账号级 429 冷却
- 多客户端密钥，每把密钥独立统计请求与 token 用量
- Command Code 额度仪表盘：5 小时 / 本周 / 按月估算进度条、余额、套餐与账期，后台每 5 分钟刷新
- 内嵌单文件 WebUI：用量总览、账号/密钥管理、模型开放编辑器、在线设置、日志查看
- 浏览器 OAuth 助手获取 Command Code API Key（CLI 或 WebUI），每次授权添加一个账号
- 客户端 Bearer Token 鉴权，WebUI 使用独立管理密码
- 用量计数（全局、按账号、按密钥）与额度快照持久化在 `usage.json`
- 多模态图片输入：支持 base64 `image_url`、字符串形式 `image_url`、`input_image` 以及 Anthropic `image` 块，并转换为 Command Code 图片块；为本地 UI 客户端开启 CORS
- `GET /health` 与 `GET /usage` 端点

## 快速开始

```bash
go build -o cmdcode2api ./cmd/cmdcode2api
./cmdcode2api
```

首次启动会在工作目录生成 `config.yaml`，并把自动生成的客户端密钥与 WebUI 管理密码打印一次。添加一个 Command Code 账号（见下节），然后把任意 OpenAI 客户端指向网关：

```bash
curl http://localhost:11434/v1/chat/completions \
  -H "Authorization: Bearer <local-api-key>" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "deepseek/deepseek-v4-flash",
    "messages": [
      {"role": "user", "content": "Hello"}
    ],
    "stream": false
  }'
```

没有任何账号时服务也会照常启动——WebUI、客户端密钥、设置均可用，chat 请求返回 `503 no_accounts`，直到添加账号为止；从 WebUI 添加首个账号时会立即拉取模型目录。

## Docker

CI 会在每次 master 推送（`latest` 标签）和 `v*` 标签时自动发布多架构镜像到 GHCR。`config.yaml` 和 `usage.json` 放在 `/data` 数据卷中：

```bash
docker run -d --name cmdcode2api -p 11434:11434 -v cmdcode2api-data:/data ghcr.io/peach0x33a/cmdcode2api:latest
```

开箱即用的 Compose 文件见 `docker-compose.example.yml`：

```bash
cp docker-compose.example.yml docker-compose.yml
docker compose up -d
```

数据目录为空时，首次启动会生成 `config.yaml`，客户端密钥和管理密码打印一次（`docker compose logs` 查看），随后进入服务状态。本地构建用 `docker build -t cmdcode2api .`；无法访问 proxy.golang.org 的网络可加 `--build-arg GOPROXY=https://goproxy.cn,direct`。

## 添加 Command Code 账号

### WebUI（最简单）

打开 `/webui`，在「账号」页粘贴 Command Code API Key，不受容器网络或 SSH 访问限制。

「账号」页也可以发起 OAuth。默认使用服务器本地的 `127.0.0.1:5959-5968` 回调端口，浏览器与服务器同机时开箱即用。当浏览器访问不到该端口时（远程 / 容器部署），Command Code 页面无法自动把凭据交回来，此时页面会停在一个把凭据放在查询参数里的地址（`?apiKey=…&state=…`）：把地址栏里这条完整链接粘贴到**回调链接**输入框并提交即可。网关只在本地解析这条链接（不会抓取该 URL），且一次性 state token 仍需与进行中的授权匹配，因此伪造链接无法注入账号。

### CLI OAuth

```bash
./cmdcode2api --oauth
```

OAuth 回调服务器始终只绑定运行程序那台机器的 `127.0.0.1:5959-5968`。每次授权成功会向 `config.yaml` 追加一个账号；重复执行 `--oauth`（例如换一个浏览器账号）可以继续添加账号，对已存在的 Key 重复授权不会产生重复账号。

**浏览器在同一台机器** —— 直接打开命令打印的授权链接。

**浏览器在另一台机器** —— 先用 SSH 把回调端口转发到本地，再显式指定回调地址：

```bash
# 本地机器
ssh -L 5959:127.0.0.1:5959 user@server

# 服务器
./cmdcode2api --oauth --oauth-callback http://localhost:5959/callback
```

### 容器内 OAuth

回调服务器监听的是**容器内**的 `127.0.0.1:5959-5968`，因此浏览器必须能访问到容器网络命名空间里的这个端口：

```bash
# 浏览器和 Docker 在同一台机器：共享宿主机网络
docker compose run --rm --network host cmdcode2api --oauth

# 浏览器在另一台机器：先用 SSH 转发端口
ssh -L 5959:127.0.0.1:5959 user@server
docker compose run --rm --network host cmdcode2api --oauth \
  --oauth-callback http://localhost:5959/callback

# 不用 Compose 时
docker run --rm -it --network host -v cmdcode2api-data:/data \
  ghcr.io/peach0x33a/cmdcode2api:latest --oauth
```

授权完成后账号会自动追加到 `/data/config.yaml`；如果网关还没启动，再 `docker compose up -d` 即可。

## 命令行旗标

| 旗标 | 说明 |
| --- | --- |
| `--oauth` | 通过浏览器 OAuth 获取 Command Code API Key |
| `--oauth-callback URL` | `--oauth` 的显式回调地址，例如 `http://localhost:5959/callback` |
| `--host HOST` | 覆盖 HTTP 监听地址，例如 `0.0.0.0` |
| `--port PORT` | 覆盖 HTTP 监听端口 |
| `--debug` | 向 stderr 打印请求体与全部上游 SSE 事件 |
| `--version` | 打印版本与 Go 运行时版本后退出 |

## 配置

`config.yaml` 位于程序运行目录，自动生成且被 git 忽略。示例：

```yaml
api_key: ccgw-generated-local-client-key
api_keys:
  - name: default
    key: ccgw-generated-local-client-key
  - name: my-agent
    key: ccgw-another-client-key
admin_password: kR7vBn2xQm9T
webui: true
commandcode:
  base_url: https://api.commandcode.ai
  accounts:
    - name: main
      api_key: your-command-code-api-key
      enabled: true
    - name: backup
      api_key: another-command-code-api-key
host: localhost
port: 11434
exclude_models:
  - gpt-
  - claude-
  - gemini-
```

字段说明：

- `api_key`：旧版单客户端密钥字段；加载时自动迁移进 `api_keys`，列表非空后保存时清除。
- `api_keys`：调用本网关的客户端密钥列表，每把密钥独立统计请求与 token 用量。可在 WebUI 中管理，改动即时生效并写回本文件。
- `admin_password`：WebUI 管理 API 的密码；为空时首次启动自动生成并打印一次。
- `webui`：设为 `false` 可完全不托管内嵌 WebUI 与管理 API。
- `commandcode.accounts`：Command Code 账号列表，请求在其间轮换（见下）。旧的 `commandcode.api_key` 单 Key 写法仍然识别，加载时自动迁移为单账号列表。
- `commandcode.base_url`：Command Code API 地址。
- `host`：HTTP 监听地址，默认 `localhost`；对外监听设置为 `0.0.0.0`。
- `port`：HTTP 监听端口，默认 `11434`。
- `exclude_models`：从 `/v1/models` 隐藏、并在 `/v1/chat/completions` 中拒绝调用的模型 ID 前缀。在 WebUI「模型」页以复选框方式维护。

新生成的配置默认排除 `gpt-`、`claude-`、`gemini-` 前缀。匹配时会同时支持普通模型 ID（例如 `gpt-4`）和带 provider 的 ID（例如 `openai/gpt-4`，会匹配最后一个 `/` 后面的 `gpt-4`）。需要开放所有模型时，删除这些条目或设置 `exclude_models: []`。

## 多账号轮换

每次 chat 请求按轮询方式使用下一个启用的账号。账号返回 `401`、`403`、`429` 或 5xx 时自动换下一个账号重试：

- `429`：按上游 `Retry-After` 冷却该账号（缺省 60 秒），冷却期内跳过；全部账号都在冷却时向客户端返回 `429 rate_limit_error` 和最早恢复时间。
- `400` / `422`（请求本身有问题）与客户端主动取消不重试。
- 故障转移只发生在向客户端写出任何字节之前；流式响应一旦开始不会在另一个账号上重放。
- 每个账号的请求 / token 计数持久化在 `usage.json`；错误信息、冷却窗口等运行时状态可在 WebUI 中查看。

## 客户端密钥

`api_keys` 是客户端调用本网关的 Bearer 密钥列表，不同客户端各用一把，互不影响：

- 在 WebUI「密钥」页新建——密钥值一律由服务端生成（`ccgw-` 前缀），不接受外部指定；支持启用/禁用、复制、删除。
- 每把密钥独立统计请求数与 token 用量，持久化在 `usage.json` 的 `client_keys` 字段，也在 `/usage` 中可见。
- 列表中密钥默认打码，可按需显示/复制——完整值仅在创建时展示一次，之后可经 reveal 端点获取。
- 旧的单一 `api_key` 字段继续有效，加载时自动迁移为名为 `default` 的一把密钥。

## WebUI

`webui` 启用（默认）时，二进制会在 `/webui` 路径托管内嵌的单文件管理界面（根路径留给 API，不提供页面）：

```text
http://localhost:11434/webui
```

使用服务器地址 + `admin_password` 登录。`internal/web/index.html` 也可以直接用浏览器打开，填任意运行实例的地址使用。

各页签：

- **概览**：版本、运行时长、监听地址、用量统计、账号/密钥/模型概览、额度同步汇总（已同步 / 超限 / 低余额账号数、最近刷新时间）
- **账号**：添加（粘贴 Key；或走 OAuth，浏览器访问不到服务器时粘贴跳转链接）、编辑名称/Key、启用/禁用、连通性测试、刷新额度、删除；展示每账号请求数、tokens、错误、冷却状态、最近错误与额度。OAuth 添加的账号按登录账号名自动命名
- **模型**：上游模型复选框列表，勾选 = 对外提供（`/v1/models` 可见、可调用），取消勾选 = 隐藏并拒绝调用；本页即 exclude_models 的可视化编辑器，改动即时生效
- **密钥**：新建客户端 API Key、启用/禁用、复制、删除；每把密钥独立用量统计（见[客户端密钥](#客户端密钥)）
- **设置**：`base_url`（即时生效）、`host`/`port`/`webui`（写盘后重启生效）、修改管理密码（需提供原密码，成功后踢出所有已登录管理会话）
- **日志**：内存日志环形缓冲（最近 500 行）实时查看

账号与设置的修改会立即写回 `config.yaml`，无需重启。

安全机制：管理接口按来源 IP 限速（10 分钟内失败 5 次锁定 15 分钟）、响应携带安全头（CSP、`X-Frame-Options: DENY`、`nosniff`、`Referrer-Policy: no-referrer`）且禁用缓存、登录表单兼容 Bitwarden 等密码管理器；「记住密码」存于 localStorage，取消勾选则仅存 sessionStorage（关标签页即失效）。

### 额度仪表盘

「账号」页展示每个账号的 Command Code 额度，使用同一个 API Key 读取未公开的 `/alpha/*` 接口（`whoami`、`billing/credits`、`billing/subscriptions`、`usage/summary`）：

- **5 小时**、**本周**进度条直接来自上游 `windowLimits`；已用 ≥50% 显示黄色、≥75% 深黄、≥90% 红色。
- **月度**为推算值：接口没有月度窗口对象，上限来自社区 CLI 的套餐映射（`individual-pro` → 30、`individual-pro-v1` → 80 等），已用 = 上限 − 剩余月度额度，界面上标注"按套餐估算"；未知套餐仅显示余额。
- 余额（月度剩余 / 充值 / 免费）、套餐名称与状态、账期结束时间、账期用量统计。
- 账号页提供单账号「刷新额度」与「刷新全部额度」按钮（后者立即返回，后台刷新）。

额度会在启动后不久自动刷新一次，之后每 5 分钟刷新一次，快照缓存在 `usage.json` 中，重启后仍然保留。查询失败时保留上一次成功的数据，只更新错误与查询时间。接口路径取自
[commandcode-usage](https://github.com/MAXeaglet/commandcode-usage)，属未公开接口，解析层兼容字段漂移（camelCase / snake_case、秒 / 毫秒 / ISO 时间、顶层或 `data` 嵌套）。

### 管理 API

UI 通过 `/admin/api/*` 访问管理接口，鉴权方式为 `Authorization: Bearer <admin_password>`，脚本 / curl 同样可用：

```text
GET    /admin/api/overview
GET    /admin/api/accounts
POST   /admin/api/accounts             {"name": "...", "api_key": "..."}
PATCH  /admin/api/accounts/{id}        {"enabled": true} 或 {"name": "..."}
DELETE /admin/api/accounts/{id}
POST   /admin/api/accounts/{id}/test
POST   /admin/api/accounts/{id}/quota/refresh
POST   /admin/api/quotas/refresh       202 立即返回并在后台刷新全部账号；带 {"id": "..."} 时同步刷新单个账号
GET    /admin/api/models
PUT    /admin/api/models               {"exposed": ["model-id", ...]}
GET    /admin/api/keys
POST   /admin/api/keys                 {"name": "..."} — 密钥值一律由服务端生成，不接受指定
GET    /admin/api/keys/{id}/reveal
PATCH  /admin/api/keys/{id}            {"enabled": true} 或 {"name": "..."}
DELETE /admin/api/keys/{id}
GET    /admin/api/settings
PUT    /admin/api/settings
GET    /admin/api/logs?after=SEQ
POST   /admin/api/oauth/start
GET    /admin/api/oauth/status
POST   /admin/api/oauth/cancel
```

`GET /admin/api/accounts` 每个账号带嵌套 `quota` 对象；`GET /admin/api/overview` 带 `quotas` 汇总（`synced`、`exceeded`、`low_balance`、`last_checked_at`）。

## HTTP API

### `GET /health`

无需鉴权。

```json
{"status":"ok"}
```

### `GET /usage`

无需鉴权。返回本地累计用量计数，以及按账号、按客户端密钥的明细：

```json
{
  "total_requests": 1,
  "prompt_tokens": 7527,
  "completion_tokens": 55,
  "cache_read_tokens": 7424,
  "cache_write_tokens": 0,
  "accounts": {
    "a1b2c3d4": {"requests": 1, "prompt_tokens": 7527, "completion_tokens": 55, "cache_read_tokens": 7424, "cache_write_tokens": 0}
  },
  "client_keys": {
    "k9f8e7d6c": {"requests": 1, "prompt_tokens": 7527, "completion_tokens": 55, "cache_read_tokens": 7424, "cache_write_tokens": 0}
  }
}
```

用量持久化在 `usage.json`（git 忽略）。缓存的额度快照也存在该文件中，但不会出现在本端点。

### `GET /v1/models`

返回应用 `exclude_models` 过滤后的模型列表，被排除的模型不会出现。上游提供时，每个条目附带 `context_window` 字段（上下文窗口大小）。

### `POST /v1/chat/completions`

接受 OpenAI 风格的 chat completion 请求并转发到 Command Code。命中 `exclude_models` 的请求返回 `404` 和 OpenAI 兼容的错误 JSON。

**模型 ID** 必须与 `/v1/models` 返回的完全一致，包含 provider 前缀：

```text
deepseek/deepseek-v4-flash          ✓
deepseek-v4-flash                   ✗ 缺少 provider 前缀
deepseek-ai/deepseek-v4-flash       ✗ provider 前缀错误
```

**支持的请求形式**：纯文本消息、多模态 content 数组、`stream: true` 的 SSE 流式响应、`stream: false` 的 JSON 响应。

**图片**：网关接受多种图片形态，并转换为 Command Code 图片块：

- `{"type":"image_url","image_url":{"url":"data:image/...;base64,..."}}` —— 标准 OpenAI
- `{"type":"image_url","image_url":"data:image/...;base64,..."}` —— 字符串形式
- `{"type":"input_image","image_url":"data:image/...;base64,..."}`
- `{"type":"image","source":{"type":"base64","media_type":"...","data":"..."}}` —— Anthropic 风格

图片数据必须是内联 base64（`data:image/...;base64,...`）。远程 HTTP(S) 图片地址及其它 URL scheme 返回 `400 invalid_request_error`，服务不会主动下载远程图片。

上游 `inputTokenDetails.cacheReadTokens` 在响应中对应 `usage.prompt_tokens_details.cached_tokens`。`cacheWriteTokens` 可在 `/usage` 查看，但不出现在 Chat Completions 响应中——OpenAI 的标准 usage 结构没有 cache-write 字段。

## 反向代理（nginx）

如果 nginx 与 cmdcode2api 在同一台主机，请继续转发 Cloudflare 的 `CF-Connecting-IP` 和 `X-Forwarded-For`。服务端只接受来自本机回环代理连接的这些请求头，并将解析后的地址用于 HTTP 日志和管理登录限流；直连请求即使携带伪造请求头，也仍使用 TCP 对端地址。

`X-Forwarded-For` 只取最右侧（由追加代理写入）的地址：左侧条目客户端可以任意伪造，伪造者换个 IP 就能绕过登录限流。因此请勿让 nginx 用 `proxy_add_x_forwarded_for` 保留客户端自带的该请求头，并建议在 nginx 层只放行 Cloudflare 官方公布的代理网段，防止绕过 CF 直连源站伪造 `CF-Connecting-IP`。若还需要让 nginx 自身的 `$remote_addr` 表示最终用户，请在 nginx 中配置 `real_ip_header CF-Connecting-IP`，并填写 Cloudflare 官方公布的代理网段。

客户端 Bearer Token 使用 `config.yaml` 里 `api_keys` 列表中的任意一把密钥。

## 项目结构

```text
cmd/cmdcode2api/   CLI 入口
internal/app/      网关实现
internal/web/      内嵌单文件 WebUI（index.html）
```

## 本地运行产物

以下文件不应提交到 Git：

```text
cmdcode2api
cc-gateway
config.yaml
usage.json
*.exe
.oauth_state
.oauth_url
```

## 说明

这是个人使用的工具型网关，目前针对开发期间观察到的 Command Code API 形态。若 Command Code 调整内部 API，适配层可能需要更新。项目原名 `cc-gateway`，为避免与 Claude Code 常见的 `cc` 缩写混淆而更名。
