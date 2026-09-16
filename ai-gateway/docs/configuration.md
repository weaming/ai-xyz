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

`routes[].id` 只能包含字母、数字、`.`、`_`、`-`，并且必须唯一。以上配置提供：

- `POST /provider/codex/v1/responses`
- `POST /provider/codex/v1/chat/completions`

`upstream.protocol` 可选 `responses` 或 `chat`。代理优先使用配置中的 `proxy`；未配置时使用 Go 的 `ProxyFromEnvironment`，因此会自动识别 `HTTPS_PROXY`、`HTTP_PROXY`、`ALL_PROXY` 和 `NO_PROXY`。

`upstream.provider` 目前实现 `openai` 和 `deepseek`。DeepSeek 使用 Chat Completions-compatible upstream；配置模型保留 `anthropic` 和 `google`，后续分别接入 Messages、GenerateContent/Interactions adapter，并由 adapter 声明 `capabilities`。

`auth.token` 为空表示不启用入口鉴权；启用后要求 `Authorization: Bearer <token>`。上游鉴权优先使用 `upstream.token`，否则从 `upstream.token_env` 指定的环境变量读取，不会把客户端 token 转发到上游。
