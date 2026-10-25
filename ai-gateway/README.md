# ai-gateway

多协议 LLM 网关。Chat Completions、Responses 与 Gemini GenerateContent 三种协议经 IR 中枢两两互转（3×3 组合），Gemini API 与 Vertex AI 走原生 SDK 调用。

配置入口是 YAML；每个 `routes[].id` 暴露为 `/provider/<id>/`，其后的路径遵循目标协议标准路径。

详细设计见 [docs/architecture.md](docs/architecture.md) 和 [docs/configuration.md](docs/configuration.md)。
