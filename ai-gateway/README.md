# ai-gateway

多协议 LLM 网关。当前实现以流式转换为优先，支持 OpenAI Responses 与 Chat Completions 互转，并为 Anthropic Messages、Google GenAI、OpenAI Responses 兼容实现预留 provider/capability 边界。

配置入口是 YAML；每个 `routes[].id` 暴露为 `/provider/<id>/`，其后的路径遵循目标协议标准路径。

详细设计见 [docs/architecture.md](docs/architecture.md) 和 [docs/configuration.md](docs/configuration.md)。
