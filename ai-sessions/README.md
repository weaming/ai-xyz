# ai-sessions

解析本机 Codex、Claude、Qoder 会话历史，输出用户输入、工具调用和最终回答，并统计 Token 用量。

## 用法

```sh
ai-sessions [选项]
```

示例：

```sh
ai-sessions                    # 列出今天的会话索引
ai-sessions -d yesterday       # 昨天的会话
ai-sessions -d all             # 全部日期的会话索引
ai-sessions -q tantivy         # 按请求或最终响应文本过滤（不区分大小写）
ai-sessions -archived          # Codex 列表包含已归档会话
ai-sessions -plan              # 只看关联了 plan 文件的会话
ai-sessions --source claude    # 只看 claude 来源
ai-sessions -i 019abc -t 2     # 指定会话第 2 问完整详情：问题、工具调用输入输出和回答
ai-sessions -i 019abc --think  # 输出该会话的中间思考过程
ai-sessions -i 019abc --transcript             # 导出纯净 user/assistant 对话全文（JSONL，喂给 AI）
ai-sessions -i 019abc --transcript --format md # 或者 Markdown 分节格式
```

选项：

```text
-i, --session ID    会话 ID，支持唯一前缀或 JSONL 文件路径
-q, --query 文本    按请求或最终响应文本过滤
-t, --turn 序号     配合 --session，输出指定问题的完整详情
--think             配合 --session，输出中间思考过程
--transcript        配合 --session，只输出纯净的 user/assistant 对话全文（不含工具调用等）
--plan              只显示关联了 plan 文件的会话
--archived          列出 Codex 会话时包含已归档会话
-s, --source 来源   all/codex/claude/qoder/qoder-app，默认 all
-d, --date 日期     YYYY-MM-DD、yesterday 或 all，按 TZ 时区过滤，默认今天
-f, --format 格式   -stat 用 table/csv；-transcript 用 jsonl（默认）/md
--codex-db 路径     Codex 历史数据库，默认 ~/.codex/thread_history_1.sqlite
--claude-dir 路径   Claude 数据目录，默认 ~/.claude
--qoder-dir 路径    Qoder 数据目录，默认 ~/.qoder-cn
--qoder-app-dir 路径 新版 Qoder 应用会话目录，默认 ~/.qoder-cn/cache/projects
```

## 输出

每个会话先输出来源、ID、时间和元信息，再按轮次列出问答：

```text
[codex] ID 019fe038-0d31-7931-a890-ce3506e88612
Path: ~/.codex/archived_sessions/rollout-2026-08-08T15-13-17-019fe038-0d31-7931-a890-ce3506e88612.jsonl
Time: 2026-08-08 15:14 ~ 2026-08-09 00:06 (TZ=Asia/Hong_Kong) [Archived=YES]
CWD: /Users/garden/src/google-news
Models: gpt-5.6-luna
Tokens: 214.9M 输入 | 825.3k 输出 | 缓存: 211.4M 命中 (98.40%)
Timing: 12 轮 | 总耗时 22m10s | avg 1m50s | median 1m58s | max 3m47s (Q4) | min <1s (Q7)
```

`Path` 行输出会话对应的源文件路径（相对主目录缩写为 `~`）：
Claude/Qoder/Qoder App 为 JSONL 文件，Codex 为完整对话历史的 rollout JSONL
（`~/.codex/sessions/` 或 `~/.codex/archived_sessions/`，缺失时回退历史库路径）。

## 导出对话（--transcript）

配合 `-i` 从会话源文件提取完整 user/assistant 文本消息序列，剔除工具调用、
工具结果、思考过程、系统注入的 AGENTS.md/环境上下文等一切非对话内容，
适合直接把会话历史喂给另一个 AI 了解上下文：

```sh
ai-sessions -i 019abc --transcript              # 每行 {"role":"user"|"assistant","content":"..."}
ai-sessions -i 019abc --transcript --format md  # ## user / ## assistant 分节
```

输出顺序与原始消息一致（含被中断的 `[Request interrupted by user]` 占位）。

归档标记仅 Codex 提供，其余来源的时间行不带 `[Archived=]`。

`Models` 按首次出现顺序列出会话用过的模型（去重）：Claude/Qoder CLI 取
助手消息的 `model` 字段，Codex 取 rollout 中 `turn_context` 的 `model`；
Qoder App 数据中没有模型信息，不输出该行。

轮次用时按每轮内首末活动时间戳计算，两轮之间用户的空闲等待不计入；
仅有一轮时只输出总耗时。完整视图（单会话/-q 匹配）额外列出每轮用时
`Durations: Q1 1m05s | Q2 30s | ...`，`-t` 详情视图输出该轮的 `Duration`。
Qoder App 会话正文无时间戳，时间取自应用状态库（QoderCN globalStorage/state.vscdb）：
会话起止用任务快照的创建/结束时间与提问历史；每轮开始时间按问题文本与提问历史
逐条匹配，用时为近似值（本轮开始 → 下一匹配轮开始，末轮 → 会话结束），
未匹配的轮次不给用时。

## 数据来源

- Codex：内容读 `thread_history_1.sqlite`；归档状态与 CWD 读自动探测的最新 `~/.codex/state_*.sqlite`；Token 从 `~/.codex/sessions/`、`~/.codex/archived_sessions/` 下 rollout JSONL 的 `token_count` 汇总累计用量与缓存命中率。
- Claude：`~/.claude/projects/`、`~/.claude/transcripts/` 的 JSONL，plan 按 slug 关联 `~/.claude/plans/`。
- Qoder（CLI）：`~/.qoder-cn/projects/` 的 JSONL，plan 按 JSONL 中 `planFilePath` 精确关联 `~/.qoder-cn/plans/`。
- Qoder App（应用）：`~/.qoder-cn/cache/projects`。
