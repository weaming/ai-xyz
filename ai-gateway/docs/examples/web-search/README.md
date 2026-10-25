# 官方托管 web_search 样例流量

2026-10-26 抓自本机 ai-gateway（`127.0.0.1:8787`）的 `codex` 路由：上游是 Responses 协议，并开启 `forward_builtin_tools`。

| 文件 | 说明 |
| --- | --- |
| `responses-websearch.request.json` | Responses 请求，`tools: [{"type": "web_search"}]` |
| `responses-websearch.stream.sse` | 对应 SSE 原始事件流（335 个事件） |
| `chat-websearch.request.json` | 同一问题改走 Chat Completions（非流式） |
| `chat-websearch.response.json` | 响应的 `message.annotations` 是 4 条 `url_citation` 投影 |
| `chat-websearch-stream.request.json` | 同上改为流式 |
| `chat-websearch-stream.response.sse` | 流式无法表达 annotation，正文尾部追加 `Sources:` 列表 |

## 可核对的事实

1. 搜索在云端执行：SSE 里有 4 组 `response.web_search_call.in_progress/searching/completed`，4 个 `web_search_call` item 携带真实查询词（`action.queries`，如 `site:reuters.com "October 25, 2026" AI`），正文附 5 条 `url_citation`，链接带 `utm_source=openai`。
2. 本地不搜索：`convert/responses.go` 只对 `web_search*` 做类型分类（丢弃、记 `tools.<type>`、或原样转发），没有搜索客户端；转发过程中本机进程唯一出站连接是 `127.0.0.1` 的 HTTP 代理。
3. Chat 客户端同样用云端搜索：工具经 `forward_builtin_tools` 转发，`web_search_call` item 因 Chat 无法表达被跳过，进度事件丢弃而不中断流转。
4. 上游不托管搜索时（Chat 协议上游，抓包未保留）没有本地兜底：工具被丢弃并记 `tools.web_search`，模型只能回答无法联网。
