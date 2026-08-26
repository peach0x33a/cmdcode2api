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

然后给要接入的 Command Code 账号起个名字，完成 OAuth：

```bash
./cmdcode2api --oauth --account personal
```

使用 `--oauth` 时必须带上 `--account <name>`。OAuth 成功后，Command Code API Key 会写入运行目录下 `config.yaml` 里对应名字的账号下。用不同的 `--account` 名字重复执行同一条命令即可添加更多账号，网关会在多个账号之间自动轮询，详见下方[账号管理](#账号管理)。

## 远程服务器 OAuth

OAuth callback server 始终只监听服务器本机的 `127.0.0.1`，不会绑定公网地址。

如果程序跑在远程服务器、浏览器在本地机器，先在本地机器建立 SSH 隧道：

```bash
ssh -L 5959:127.0.0.1:5959 root@your-server
```

然后在服务器上运行：

```bash
./cmdcode2api --oauth --account personal --oauth-callback http://localhost:5959/callback
```

把程序打印出的授权链接复制到本地浏览器打开即可。

## 账号管理

Command Code 账号按名字存放在 `config.yaml` 里，可以添加任意多个：

```bash
./cmdcode2api --oauth --account personal
./cmdcode2api --oauth --account work
```

对已存在的名字再次执行 `--oauth --account <name>` 会覆盖该账号的 Key。账号失效（stale）后重新授权，用的也是这条命令。

不启动服务，只查看当前各账号的实时状态：

```bash
./cmdcode2api --list-accounts
```

删除某个账号：

```bash
./cmdcode2api --remove-account work
```

### 轮询与故障转移

服务运行时，`/v1/chat/completions` 请求会在所有未被标记为 stale 的账号之间轮询。如果某个账号被 Command Code 返回 `401` 或 `403`，网关会把它标记为 stale，后续请求转发给其余账号。后台探测任务大约每 10 分钟对每个账号做一次轻量级鉴权请求，即使没有实际流量也能及时发现失效的 Key：探测返回 `200` 会清除 stale 标记，超时、5xx 等临时性失败只会被记录下来，不会改变账号状态。

Command Code API Key 没有刷新机制，账号一旦 stale，只能重新登录，重启服务没有用：

```bash
./cmdcode2api --oauth --account <name>
```

随时可以用 `--list-accounts` 或 `GET /accounts` 查看当前状态。

## 配置

`config.yaml` 位于程序运行目录，示例：

```yaml
api_key: ccgw-generated-local-client-key
accounts:
  - name: personal
    api_key: your-command-code-api-key
    base_url: https://api.commandcode.ai
host: localhost
port: 11434
exclude_models:
  - gpt-
  - claude-
  - gemini-
```

字段说明：

- `api_key`：本地网关的 Bearer Token，客户端请求本服务时使用。
- `accounts`：Command Code 账号列表，每项包含 `name`、通过 `--oauth --account <name>` 获取的 `api_key`，以及 `base_url`。
- `host`：HTTP 监听地址，默认 `localhost`。需要对外监听时设置为 `0.0.0.0`。
- `port`：HTTP 监听端口，默认 `11434`。
- `exclude_models`：要从 `/v1/models` 隐藏、并在 `/v1/chat/completions` 中拒绝调用的模型 ID 前缀。`/v1/responses` 共用同一套派发逻辑，因此同样受此设置约束。

如果你的 `config.yaml` 是多账号支持之前生成的，里面单独的 `commandcode: {api_key, base_url}` 会在下次加载时自动迁移成 `accounts` 列表，账号名为 `default`。

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

如果 `/v1/chat/completions` 或 `/v1/responses` 请求命中 `exclude_models`，服务会返回 `404` 和 OpenAI 兼容的错误 JSON，表示该模型不可用。

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

## 其他接口

### `POST /v1/responses`

实现了 OpenAI 的 Responses API 协议，供只支持该协议、不支持 Chat Completions
的客户端使用（典型例子是 Codex CLI）。鉴权与 `/v1/chat/completions` 相同，都
需要 Bearer Token，底层也共用同一套 Command Code 派发逻辑，因此模型排除、账号
轮询与故障转移行为完全一致。

网关是无状态的：不支持 `previous_response_id`，因为服务端并不保存历史会话。
每次请求都要在 `input` 里带上完整对话。

```bash
curl http://localhost:11434/v1/responses \
  -H "Authorization: Bearer <local-api-key>" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "deepseek/deepseek-v4-flash",
    "input": "Hello",
    "stream": false
  }'
```

### `GET /usage` 与 `GET /admin/usage`

`GET /usage` 无需鉴权，只返回全局累计用量（`total_requests`、
`prompt_tokens` 等），不做任何改动，也不会按账号拆分。

`GET /admin/usage` 是新增的管理接口，只回应来自本机的请求：判定方式与
`/accounts`、`/ui` 相同（TCP 对端必须是回环地址，`Host` 必须是
`localhost`/`127.0.0.1`/`[::1]`，若带了 `Origin` 头则必须与之匹配），不接受
远程调用，无论是否带了 Bearer Token。返回内容是 `GET /usage` 的全局总计，外
加按账号拆分的明细：

```json
{
  "total_requests": 12,
  "prompt_tokens": 41200,
  "completion_tokens": 980,
  "cache_read_tokens": 39000,
  "cache_write_tokens": 0,
  "accounts": [
    {
      "account": "personal",
      "total_requests": 7,
      "prompt_tokens": 25000,
      "completion_tokens": 600,
      "cache_read_tokens": 24000,
      "cache_write_tokens": 0
    }
  ]
}
```

```bash
curl http://localhost:11434/admin/usage
```

（必须在网关所在机器上执行，远程请求会被拒绝。）

## 本地运行产物

以下文件不应该提交到 Git：

```text
cmdcode2api
config.yaml
usage.json
.oauth_state
.oauth_url
```
