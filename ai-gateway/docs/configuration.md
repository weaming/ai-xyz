# 配置

启动：

```sh
go run . -config config.yaml
```

配置使用 YAML。字符串中的 `${NAME}` 从进程环境变量展开，不读取 `.env` 文件；敏感 token 使用 `token_env` 写环境变量名，由程序启动时读取。

```yaml
server:
  address: 127.0.0.1:8787
  read_timeout: 30s
  write_timeout: 30m

routes:
  - id: codex
    auth:
      token: ${AI_GATEWAY_TOKEN}
    upstream:
      provider: openai
      protocol: responses
      base_url: https://api.openai.com/v1
      token_env: OPENAI_API_KEY
      proxy: http://localhost:7890
      timeout: 30m
    defaults:
      model: gpt-5.6-luna
    conversion:
      mode: preserve
      emit_reasoning_content: true
```

省略 `conversion` 或其中字段时，默认使用 `mode: preserve` 和 `emit_reasoning_content: true`；如需关闭 reasoning content，可显式设置 `emit_reasoning_content: false`。

`routes[].id` 只能包含字母、数字、`.`、`_`、`-`，并且必须唯一。以上配置提供：

- `POST /provider/codex/v1/responses`
- `POST /provider/codex/v1/chat/completions`

OpenAI 和 DeepSeek 的 `upstream.protocol` 可选 `responses` 或 `chat`。代理优先使用配置中的 `proxy`；未配置时使用 Go 的 `ProxyFromEnvironment`，因此会自动识别 `HTTPS_PROXY`、`HTTP_PROXY`、`ALL_PROXY` 和 `NO_PROXY`。

`upstream.provider` 支持 `openai`、`deepseek`、`google` 和 `vertex`。DeepSeek 支持 Chat Completions 和 Responses 两种上游协议。

`google` 和 `vertex` route 使用 `upstream.protocol: generate_content` 调用 Google 的 `generateContent` / `streamGenerateContent` API，同时对客户端提供 Chat Completions、Responses 和 models 路径。`base_url` 可选，用于自定义上游地址。Interactions API 是另一种协议，目前尚未接入。

```yaml
routes:
  - id: gemini
    auth:
      token: ${AI_GATEWAY_TOKEN}
    upstream:
      provider: google
      protocol: generate_content
      token_env: GEMINI_API_KEY
      proxy: http://localhost:7890
      timeout: 30m
      generate_content:
        thinking_level: medium
        model_aliases:
          fast: gemini-3.7-flash
    defaults:
      model: gemini-3.7-flash
```

Vertex AI 使用 Application Default Credentials 鉴权，需填写项目和区域，不配置 `token` 或 `token_env`：

```yaml
routes:
  - id: vertex
    auth:
      token: ${AI_GATEWAY_TOKEN}
    upstream:
      provider: vertex
      protocol: generate_content
      vertex:
        project: my-gcp-project
        location: us-central1
      generate_content:
        thinking_level: medium
    defaults:
      model: gemini-2.5-flash
```

Google API Key route 可使用 `token`、`token_env`，未填写时读取进程环境中的 `GEMINI_API_KEY` 或 `GOOGLE_API_KEY`。两种 provider 共用的 `generate_content.thinking_level` 可选 `low`、`medium`、`high`，`model_aliases` 也放在 `generate_content` 下。`upstream.timeout` 控制单次请求的最长处理时间，默认 30 分钟。入口路径为 `/provider/<id>/v1/chat/completions`、`/provider/<id>/v1/responses`、`/provider/<id>/v1/models`。

客户端请求需提供 `model`，或在 route 设置 `defaults.model`。GenerateContent 适配器会拒绝尚未映射的请求字段和内容类型；图片输入目前只支持 base64 data URL。
`GET /provider/<id>/v1/models` 只列出该 route 配置的默认模型和别名，不查询上游模型目录。

DeepSeek Responses 路由的 `base_url` 使用 `https://api.deepseek.com`，网关会请求上游 `/responses`；OpenAI/Codex 等标准 Responses 路由仍使用带 `/v1` 的 base URL。

`auth.token` 为空表示不启用入口鉴权；启用后要求 `Authorization: Bearer <token>`。上游鉴权优先使用 `upstream.token`，否则从 `upstream.token_env` 指定的环境变量读取，不会把客户端 token 转发到上游。
