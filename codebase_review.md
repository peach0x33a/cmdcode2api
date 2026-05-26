# cmdcode2api Codebase Review

审查日期：2026-05-26

## 范围

本次审查覆盖当前 Go 代码、HTTP 接口、配置/OAuth 流程、使用量持久化、目录结构和本地接口验证。

当前目录结构：

```text
cmd/cmdcode2api/   CLI entrypoint
internal/app/      gateway implementation
```

## 已验证

本地服务运行在 `http://localhost:11434` 时，以下接口已验证通过：

```text
GET     /health
GET     /usage
GET     /v1/models
OPTIONS /v1/chat/completions
POST    /v1/chat/completions  stream=false
POST    /v1/chat/completions  stream=true
```

模型调用使用 `deepseek/deepseek-v4-flash`，非流式返回 `nonstream-ok`，流式返回 `stream-ok` 并正常输出 `[DONE]`。

本地命令验证通过：

```bash
go build -o cmdcode2api ./cmd/cmdcode2api
go test ./...
go vet ./...
```

## 已处理的问题

- 删除 `default_model` 配置，请求缺少 `model` 时返回 `400 invalid_request_error`。
- 首次启动提示改为简洁 CLI 文案。
- 删除未使用的 `openBrowser`，OAuth 只打印授权链接。
- CORS preflight `OPTIONS` 跳过鉴权，预检请求返回 `204`。
- CORS 中间件调整为最外层，鉴权失败的 `401` 也会携带 CORS 响应头。
- `/v1/chat/completions` 缺少 `messages` 时返回 `400 invalid_request_error`。
- `/v1/chat/completions` 请求体增加 50MB 上限。
- SSE `data:` JSON 解析失败时返回错误，不再静默忽略。
- `usage.json` 保存加锁，并改为先写临时文件再 `rename`。
- `.gitignore` 修正，忽略根目录二进制、配置、usage 和 OAuth 临时文件。
- 模型列表抽取为共享 catalog，`/v1/models` 和启动日志使用同一份数据。
- 检查 API key、OAuth state、chat id 生成时的随机数错误。
- 删除无用 `init()`。
- `handleNonStream` 补齐 `string` 和默认分支，避免 text delta 被静默丢弃。
- `contentToCC` 不再忽略 tool call arguments 的 JSON 解析错误。
- OAuth 授权等待增加 10 分钟超时。
- 删除 `EstimatedCredits()`，避免用硬编码价格给出误导性费用估算。
- `findConfig()` 固定使用运行目录的 `config.yaml`，与 `usage.json`、`.oauth_*` 的运行目录策略保持一致。
- 增加最小测试覆盖格式转换、data URL、SSE 解析错误、鉴权/CORS、缺字段校验、模型 catalog、非流式文本拼接和非法 tool arguments。

## 主要发现

### 1. 请求转换测试仍可继续扩展

已经补了最小测试，但 `contentToCC` 的复杂路径还可以继续覆盖：

- multimodal image input
- tool result
- valid tool call

## 安全和本地文件

这些文件是本地运行产物，不应提交：

```text
/cmdcode2api
config.yaml
usage.json
.oauth_state
.oauth_url
```

当前 `.gitignore` 已覆盖它们。仍建议确认历史中没有提交过敏感文件：

```bash
git log --all -- config.yaml .oauth_state .oauth_url
```

## 建议优先级

1. 继续补 `contentToCC` 的图片输入、合法 tool call 和 tool result 测试。
2. 如果将来需要费用展示，重新设计按模型计价的 usage 统计。

## 总结

代码库已经适合作为一个小型本地网关继续迭代。当前报告中发现的主要结构性问题已经处理，剩余工作主要是继续补充更细的转换逻辑测试。
