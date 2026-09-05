# cmdcode2api 中文说明

`cmdcode2api` 是一个本地 OpenAI 兼容网关，用来把 OpenAI 风格的请求转发到 Command Code。

## 构建

```bash
go build -o cmdcode2api ./cmd/cmdcode2api
```

## 首次运行

先运行一次生成配置：

```bash
./cmdcode2api
```

生成的 `config.yaml` 会包含本地客户端 Key 和 WebUI 管理密码，两者都会打印一次。

然后完成 Command Code OAuth：

```bash
./cmdcode2api --oauth
```

OAuth 成功后，Command Code API Key 会作为**一个账号**追加写入 `config.yaml`。
重复执行 `--oauth`（例如换一个浏览器账号）可以继续添加账号；对已存在的 Key
重复授权不会产生重复账号。

## 远程服务器 OAuth

OAuth callback server 始终只监听服务器本机的 `127.0.0.1`，不会绑定公网地址。

如果程序跑在远程服务器、浏览器在本地机器，先在本地机器建立 SSH 隧道：

```bash
ssh -L 5959:127.0.0.1:5959 root@your-server
```

然后在服务器上运行：

```bash
./cmdcode2api --oauth --oauth-callback http://localhost:5959/callback
```

把程序打印出的授权链接复制到本地浏览器打开即可。

## 多账号轮换

`commandcode.accounts` 支持配置多个 Command Code 账号。每次请求按轮询方式
使用下一个启用的账号，遇到账号级错误自动切换：

- `429`：按上游 `Retry-After` 冷却该账号（缺省 60 秒），冷却期内跳过；全部
  账号都在冷却时向客户端返回 `429 rate_limit_error` 和最早恢复时间。
- `401 / 403 / 5xx`：自动换下一个账号重试，直到全部尝试完毕。
- `400 / 422`（请求本身有问题）与客户端主动取消不重试。
- 故障转移只发生在向客户端写出任何字节之前；流式响应一旦开始不会在另一个
  账号上重放。
- 每个账号的请求 / token 计数持久化在 `usage.json`；错误信息、冷却窗口等
  运行时状态可在 WebUI 中查看。

## 多客户端密钥

`api_keys` 支持配置多把客户端密钥，不同客户端各用一把，互不影响：

- 可在 WebUI「密钥」页新建（留空密钥值则自动生成 `ccgw-` 密钥）、复制、启用/禁用、删除；
- 每把密钥独立统计请求数与 token 用量，持久化在 `usage.json` 的 `client_keys` 字段，也在 `/usage` 中可见；
- 禁用立即生效；删除最后一把密钥会被拒绝，避免把自己锁在外面；
- 旧的单一 `api_key` 字段继续有效，加载时自动迁移为名为 `default` 的一把密钥。

## WebUI 管理台

`webui` 启用（默认）时，二进制会在 `/webui` 路径托管内嵌的单文件管理界面
（根路径留给 API，不提供页面）：

```text
http://localhost:11434/webui
```

使用服务器地址 + `admin_password` 登录。`internal/web/index.html` 也可以
直接用浏览器打开，填任意运行实例的地址使用。

功能：

- **概览**：版本、运行时长、监听地址、用量统计、账号与模型概览
- **账号**：添加（粘贴 Key 或 OAuth）、编辑名称/Key、启用/禁用、连通性测试、删除；展示每账号请求数、tokens、错误、冷却状态、最近错误。OAuth 添加的账号按登录账号名自动命名
- **模型**：上游模型复选框列表，勾选 = 对外提供（`/v1/models` 可见、可调用），取消勾选 = 隐藏并拒绝调用；本页即 exclude_models 的可视化编辑器，改动即时生效
- **密钥**：新建调用本网关的客户端 API Key（自动生成或自定义值）、复制、启用/禁用、删除；每把密钥独立的请求与 token 统计
- **设置**：`base_url` 与 `exclude_models`（即时生效）、`host`/`port`/`webui`（写盘后重启生效）、修改管理密码（即时生效）
- **日志**：内存日志环形缓冲（最近 500 行）实时查看

账号与设置的修改会立即写回 `config.yaml`，无需重启。

### 管理 API

UI 通过 `/admin/api/*` 访问管理接口，鉴权方式为
`Authorization: Bearer <admin_password>`，脚本 / curl 同样可用：

```text
GET    /admin/api/overview
GET    /admin/api/accounts
POST   /admin/api/accounts             {"name": "...", "api_key": "..."}
PATCH  /admin/api/accounts/{id}        {"enabled": true} 或 {"name": "..."}
DELETE /admin/api/accounts/{id}
POST   /admin/api/accounts/{id}/test
GET    /admin/api/keys
POST   /admin/api/keys                 {"name": "...", "key": "ccgw-...（可选，留空自动生成）"}
PATCH  /admin/api/keys/{id}            {"enabled": true} 或 {"name": "..."}
DELETE /admin/api/keys/{id}            （最后一把密钥不可删除）
GET    /admin/api/settings
PUT    /admin/api/settings
GET    /admin/api/logs?after=SEQ
POST   /admin/api/oauth/start
GET    /admin/api/oauth/status
POST   /admin/api/oauth/cancel
```

WebUI 内的 OAuth 流程要求浏览器能访问服务器的 `127.0.0.1:5959-5968` 回调
端口（本机运行开箱即用；远程服务器请用 SSH 隧道或 CLI `--oauth`）。

## 配置

`config.yaml` 位于程序运行目录，示例：

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
- `api_keys`：调用本网关的客户端密钥列表，每个密钥独立统计请求与 token 用量。可在 WebUI「密钥」页新建（自动生成或自定义）、启停、删除，改动即时生效并写回本文件。
- `admin_password`：WebUI 管理 API 的密码；为空时首次启动自动生成并打印一次。
- `webui`：设为 `false` 可完全不托管内嵌 WebUI 与管理 API。
- `commandcode.accounts`：Command Code 账号列表，请求在其间轮换。旧的
  `commandcode.api_key` 单 Key 写法仍然识别，加载时自动迁移为单账号列表。
- `commandcode.base_url`：Command Code API 地址。
- `host`：HTTP 监听地址，默认 `localhost`。需要对外监听时设置为 `0.0.0.0`。
- `port`：HTTP 监听端口，默认 `11434`。
- `exclude_models`：要从 `/v1/models` 隐藏、并在 `/v1/chat/completions` 中拒绝调用的模型 ID 前缀。

新生成的配置默认排除 `gpt-`、`claude-`、`gemini-` 前缀。匹配时会同时支持普通模型 ID（例如 `gpt-4`）和带 provider 的 ID（例如 `openai/gpt-4`，会匹配最后一个 `/` 后面的 `gpt-4`）。

如果需要开放所有模型，删除这些条目，或显式设置为空列表：

```yaml
exclude_models: []
```

## 启动服务

默认只监听本机：

```bash
./cmdcode2api
```

远程服务器需要对外提供服务时：

```bash
./cmdcode2api --host 0.0.0.0
```

或在 `config.yaml` 中设置：

```yaml
host: 0.0.0.0
port: 11434
```

## 客户端使用

OpenAI 兼容 base URL：

```text
http://localhost:11434/v1
```

如果经过反向代理，例如：

```text
https://example.com/ai/v1
```

客户端 Bearer Token 使用 `config.yaml` 里的 `api_key`。

## 模型 ID

请求里的 `model` 必须使用 `/v1/models` 返回的 ID。`/v1/models` 会先应用 `exclude_models` 过滤，因此被排除的模型不会出现在列表里。

例如：

```text
deepseek/deepseek-v4-flash
```

不要写成：

```text
deepseek-v4-flash
deepseek-ai/deepseek-v4-flash
```

如果 `/v1/chat/completions` 请求命中 `exclude_models`，服务会返回 `404` 和 OpenAI 兼容的错误 JSON，表示该模型不可用。

## 测试请求

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

多模态请求中的 `image_url` 必须使用
`data:image/...;base64,...` 形式。远程 HTTP(S) 图片地址会返回
`400 invalid_request_error`，服务不会主动下载远程图片。

## 本地运行产物

以下文件不应该提交到 Git：

```text
cmdcode2api
config.yaml
usage.json
.oauth_state
.oauth_url
```

