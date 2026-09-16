# ai-xyz

AI 相关 CLI 小工具集（Go）。

| 工具                                              | 说明                                           | 语言 | 安装方式                           |
| ------------------------------------------------- | ---------------------------------------------- | ---- | ---------------------------------- |
| [ai-models](ai-models/)                           | 查询 OpenRouter 模型排行、编程评分与各渠道价格 | Go   | `make install-go NAME=ai-models`   |
| [ai-sessions](ai-sessions/)                       | 解析本机 Codex/Claude/Qoder 会话历史           | Go   | `make install-go NAME=ai-sessions` |
| [ai-gateway](ai-gateway/)                         | 多协议 LLM 请求与流式响应转换网关              | Go   | `make install-go NAME=ai-gateway`  |
| [codex-mcp](codex/mcp/)                           | codex apply_patch 文件编辑工具                 | Rust | `make install-codex-mcp`           |
| [codex-responses-api](codex/codex-responses-api/) | codex response api 代理                        | Rust | `make install-codex-responses-api` |

## 安装

### Go

任选其一：

1. 克隆后执行 `make install NAME=ai-xxx`
2. `go install -trimpath -ldflags '-s -w' github.com/weaming/ai-xyz/ai-xxx@HEAD`
