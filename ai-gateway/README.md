# ai-gateway

多协议 LLM 网关。支持 OpenAI Responses 与 Chat Completions 互转，以及 Gemini API 和 Vertex AI 的原生调用。

配置入口是 YAML；每个 `routes[].id` 暴露为 `/provider/<id>/`，其后的路径遵循目标协议标准路径。

详细设计见 [docs/architecture.md](docs/architecture.md) 和 [docs/configuration.md](docs/configuration.md)。
