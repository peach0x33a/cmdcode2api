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

然后完成 Command Code OAuth：

```bash
./cmdcode2api --oauth
```

OAuth 成功后，Command Code API Key 会写入运行目录下的 `config.yaml`。

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

## 配置

`config.yaml` 位于程序运行目录，示例：

```yaml
api_key: ccgw-generated-local-client-key
commandcode:
  api_key: your-command-code-api-key
  base_url: https://api.commandcode.ai
host: localhost
port: 11434
```

字段说明：

- `api_key`：本地网关的 Bearer Token，客户端请求本服务时使用。
- `commandcode.api_key`：通过 `--oauth` 获取的 Command Code API Key。
- `commandcode.base_url`：Command Code API 地址。
- `host`：HTTP 监听地址，默认 `localhost`。需要对外监听时设置为 `0.0.0.0`。
- `port`：HTTP 监听端口，默认 `11434`。

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

请求里的 `model` 必须使用 `/v1/models` 返回的 ID。

例如：

```text
deepseek/deepseek-v4-flash
```

不要写成：

```text
deepseek-v4-flash
deepseek-ai/deepseek-v4-flash
```

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

## 本地运行产物

以下文件不应该提交到 Git：

```text
cmdcode2api
config.yaml
usage.json
.oauth_state
.oauth_url
```
