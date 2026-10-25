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
- `generate_content`：`/provider/<id>/v1/{chat/completions,responses}` 先解析成 IR，再编码成 generateContent 请求交给 Google SDK。

三条路径都支持非流式和 SSE 流式。协议适配器由 `Codec` 组成：`ChatCodec`、`ResponsesCodec`（`convert/`）和 GenerateContent `codec`（`gemini/`），`convert.Hub` 用 IR 把它们两两组合，因此 `chat`、`responses`、`generate_content` 的 3×3 输入/输出组合（含同协议透传）都可转换，见 `gemini/matrix_test.go`。

`google` 与 `vertex` route 共用 GenerateContent 适配器：入口协议解析成 IR，IR 编码成 generateContent 请求体（`contents`、`systemInstruction`、`tools`、`toolConfig`、`generationConfig`），再反序列化成 Google SDK 类型调用上游；不可表达的字段（`store`、`metadata`、未知字段、custom tool 等）在编码阶段明确拒绝。Anthropic Messages 和 Google Interactions 尚未实现。

## 保真策略

只有一种策略，按字段性质分三档：

1. **保真**：目标协议能表达的字段一律原样映射（结构化输出、多模态、custom tool、allowed_tools、`service_tier`、`output_text.annotations` 等）。
2. **丢弃调参/元数据**：不影响内容、只是调优或元信息的字段，目标协议不表达时静默丢弃，并记入 `ir.Request.Dropped` 供日志排查：Chat 侧 `user`、`seed`、`logit_bias`、`prediction`、`prompt_cache_key`、`prompt_cache_retention`、`safety_identifier`；Responses 侧 `include`、`prompt_cache_key`、`prompt_cache_retention`、`prompt_cache_options`、`safety_identifier`；GenerateContent 侧忽略 `store`、`metadata`、`service_tier`、`parallel_tool_calls`、`text.verbosity`、`reasoning.summary`。
3. **报错**：丢失会改变语义时返回 400，并说明字段：`previous_response_id`/`conversation`/`background`/`prompt`/`moderation`/`context_management`/`max_tool_calls`、未知顶层字段、内置工具 item、custom tool 转 Gemini、`output_text.annotations` 转 Chat/Gemini、文件与音频转 Gemini、`n>1`、`logprobs`、音频输出、旧 `functions`/`function_call`。

加密 reasoning 单独处理：`encrypted_content` 只在 Responses 内可回传（IR `Item.Encrypted` + Responses 写出保留），转 Chat/Gemini 时降级为可见 summary 文本，不报错也不伪造密文。

同协议请求/响应/流式是字节透传，不经 IR；同协议聚合优先保留源协议字段（见边界规则 7）。不承诺 Chat 与 Responses 之间任意字段都能无损互换：真正不可表达的 Responses item 不会凭空出现在 Chat 响应中。

## 边界规则

1. 不把 `reasoning`、加密 reasoning、可见 summary 混成一个字符串；分别保存字段。
2. function arguments 在流中按字符串增量保存，结束时再决定是否可解析为 JSON。
3. custom tool 的输入允许字符串或 JSON，不限制为 object。
4. 保持 item 顺序；Chat 没有对应结构时只做明确的 projection。
5. 未知事件可以保留为 opaque，但不得把未知事件当成完成事件。
6. 转换状态只属于一次请求，不能跨请求复用；未来若支持 `previous_response_id`，需要增加可持久化 conversation store。
7. 流聚合必须读到 Chat `[DONE]` 或 Responses `completed`、`failed`、`incomplete` 终态；EOF 不能作为成功完成。同协议聚合走 Codec 自己的实现（`AggregateCodec`），保留 chunk 级 `system_fingerprint`/`service_tier` 与 Responses 终态 `response` 对象的额外字段；跨协议聚合才用 IR 累加器重建。
8. 工具结果按 `call_id` 关联原调用；custom tool 结果通过 `tool_call_type: custom` 保留类型，流式参数优先使用 delta，缺失时从 done item 补齐。
9. 目标协议不表达的调参/元数据字段（`text.verbosity`、`reasoning.summary`、`include`、`prompt_cache_*` 等）丢弃并记入 `Request.Dropped`；会改变语义的字段（会话状态、内置工具 item、`annotations` 等）报错。`service_tier` 双向映射，`system_fingerprint` 只属于 Chat，转出时按元数据丢弃。
