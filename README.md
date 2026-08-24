# ai-xyz

AI 相关 CLI 小工具集（Go）。

| 工具 | 说明 |
|---|---|
| [ai-models](ai-models/) | 查询 OpenRouter 模型排行、编程评分与各渠道价格 |
| [ai-sessions](ai-sessions/) | 解析本机 Codex/Claude/Qoder 会话历史 |

## 安装

克隆后执行 `make install NAME=ai-xxx`：本质是进入各目录执行 `go install -trimpath -ldflags '-s -w' .`，安装到 `$(go env GOPATH)/bin`

或者 `go install -trimpath -ldflags '-s -w' github.com/weaming/ai-xyz/ai-xxx@HEAD`
