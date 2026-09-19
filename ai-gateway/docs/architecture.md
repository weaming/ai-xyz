# 多协议转换设计

## 目标

网关在保持协议外观的同时，尽量保留输入、输出 item、流式事件、工具调用、reasoning、usage 以及未知字段。转换必须明确区分：

- `Core`：多个协议共有、可以安全迁移的语义。
- `Native`：某个协议或厂商特有、但已经被网关理解的能力。
- `Opaque`：未知或无法迁移的数据，携带 provider、原始 JSON 和顺序位置。

因此本项目不是把某个厂商的 JSON 作为“标准 JSON”，而是使用有序 item IR 加上有状态流式事件 IR：

```text
客户端协议 → 协议适配器 → Core IR + Native IR + Opaque IR → 协议适配器 → 上游协议
```

适配器数量从双边的 `N × N` 降为每个协议一进一出。参考 LLM-Rosetta 的 hub-and-spoke 和转换上下文，但响应层不采用 Chat 的 `choices` 作为 IR 根；参考 Open Responses 2026-04-24 的 item、状态和语义事件模型。

## IR 分层

### 请求

`Request` 由以下部分组成：

- `Model`、`Instructions`、`Input`、`Tools`、`ToolChoice`、生成参数、reasoning、文本格式、stream。
- `Input` 是有序 item：message、function call、function output、reasoning、引用、opaque。
- `Extensions` 保存已知协议扩展；`Unknown` 保存没有被理解的顶层字段。

`Tool` 不只定义 function。类型包含 `function`、`custom`、MCP 和 vendor-specific tool，并保留完整原始 payload，避免把能力错误降级成普通 function。

### 响应

`Response` 的核心是有序 `Output []Item`，而不是 `choices`。Chat Completions 的 `choices` 只是一个向 Chat 客户端输出时的投影。

每个 item 保留 `id`、`status`、`phase`、`raw` 和位置。不能映射到目标协议的 item 不丢弃，而是进入 `OpaqueItem`；在目标协议支持时按原位置合并，否则返回可观察的降级结果。

### 流

流转换使用状态机，而不是独立地转换每个 chunk。状态至少包括：

- response id、model、created、sequence number；
- output index、content index、item id；
- `item_id ↔ call_id` 和工具参数缓冲区；
- 当前 message/reasoning block；
- usage、finish reason、错误和 terminal 状态。

事件分为生命周期、内容增量、工具增量、usage、完成/失败和 opaque。目标协议适配器可以合成目标协议要求的生命周期事件，但不能伪造源协议没有的语义字段。

## 当前落地范围

配置的 upstream provider 决定“哪一端是原生上游”：

- `responses`：`/provider/<id>/v1/responses` 直连，`/provider/<id>/v1/chat/completions` 转成 Responses。
- `chat`：`/provider/<id>/v1/chat/completions` 直连，`/provider/<id>/v1/responses` 转成 Chat Completions。

两条路径都支持非流式和 SSE 流式。未实现的厂商不会被配置文件假装成已支持；后续通过独立 adapter 实现 Anthropic Messages、Google GenerateContent/Interactions 等协议。

## 保真策略

请求和响应都有三种策略：

- `portable`：只发公共语义，遇到不支持的能力报 warning 或拒绝。
- `preserve`：尽量透传原始字段和 opaque item，适合同协议或兼容协议。
- `strict`：发现会改变语义的降级立即返回错误。

当前默认是 `preserve`。这不承诺 Chat 与 Responses 之间任意字段都能无损互换；真正不可表达的 Responses item 不可能凭空出现在 Chat 响应中。

## 边界规则

1. 不把 `reasoning`、加密 reasoning、可见 summary 混成一个字符串；分别保存字段。
2. function arguments 在流中按字符串增量保存，结束时再决定是否可解析为 JSON。
3. custom tool 的输入允许字符串或 JSON，不限制为 object。
4. 保持 item 顺序；Chat 没有对应结构时只做明确的 projection。
5. 未知事件可以保留为 opaque，但不得把未知事件当成完成事件。
6. 转换状态只属于一次请求，不能跨请求复用；未来若支持 `previous_response_id`，需要增加可持久化 conversation store。
7. 流聚合必须读到 Chat `[DONE]` 或 Responses `completed`、`failed`、`incomplete` 终态；EOF 不能作为成功完成。
8. 工具结果按 `call_id` 关联原调用；custom tool 结果通过 `tool_call_type: custom` 保留类型，流式参数优先使用 delta，缺失时从 done item 补齐。
