---
name: ai-sessions
description: 解析本机 Codex、Claude、Qoder 会话历史，查询过往 AI 对话、工具调用、Token 用量。当用户想回顾之前用 AI 做过什么、找某次会话的内容、统计 AI 使用量时使用。
allowed-tools: [Bash]
---

# ai-sessions 技能

使用本地 `ai-sessions` 二进制解析本机 Codex/Claude/Qoder 会话历史，输出用户输入、
工具调用和最终回答，并统计 Token 用量。适合回顾过去用 AI 做过什么、找回某次会话内容。

## 常用场景

```sh
ai-sessions                         # 列出今天的会话索引
ai-sessions -d yesterday            # 昨天的会话
ai-sessions -d 2026-08-30           # 指定日期
ai-sessions -d all                  # 全部日期的会话索引
ai-sessions -q tantivy              # 按请求或最终响应文本过滤（不区分大小写）
ai-sessions -s claude               # 只看 claude 来源
ai-sessions --plan                  # 只看关联了 plan 文件的会话
ai-sessions --archived              # Codex 列表包含已归档会话
ai-sessions -i 019abc --transcript  # AI优先用来了解会话
```

## 查看会话详情

```sh
ai-sessions -i 019abc                           # 会话完整内容（ID 可只给唯一前缀）
ai-sessions -i 019abc -t 2                      # 第 2 问完整详情：问题、工具调用输入输出和回答
ai-sessions -i 019abc --think                   # 输出该会话的中间思考过程
ai-sessions -i 019abc --transcript              # 导出纯净 user/assistant 对话全文（JSONL，喂给 AI）
ai-sessions -i 019abc --transcript --format md  # Markdown 分节格式
```

## 指定会话（-i）

`-i` 接受三种形式，无需完整 ID：

| 形式 | 示例 | 说明 |
| ---- | ---- | ---- |
| 完整 ID | `-i 019f0e6b-3d3c-7ddc-a96a-6b6d6e2cbf01` | 各来源原始 ID（Codex/Claude/Qoder 的 UUID，Claude CLI 的 `ses_` 前缀 ID） |
| 唯一前缀 | `-i 019f0e6b`、`-i ses_419c17e3` | 前缀命中多个会话时按 ID 排序并报错，需加长前缀直到唯一 |
| 文件路径 | `-i ~/.claude/projects/x/y.jsonl` | 直接指向 JSONL 文件，跳过查找 |

省略 `-s` 时自动探测来源；查不到或歧义时报错到 stderr 并退出（退出码 1）。

## 统计 Token 用量

```sh
ai-sessions -stat                  # 只输出元信息表格（不输出问答内容）
ai-sessions -stat --format csv     # CSV 格式，便于进一步处理
```

统计列：`SOURCE | ID | START_TIME | END_TIME | TURNS | MODELS | TOKENS_IN | TOKENS_OUT | CACHE_HIT | CACHE_HIT%`

`TOKENS_IN` 为含缓存命中的总输入；`CACHE_HIT` 为命中的输入 token 数，
`CACHE_HIT%` 为缓存命中率。会话详情头部输出 `Path:` 行给出源文件路径
（相对主目录缩写为 `~`），Claude/Qoder/Qoder App 为 JSONL 文件，
Codex 为完整对话历史的 rollout JSONL。

## 常用 flag

| flag | 简写 | 说明 |
| ---- | ---- | ---- |
| `--session` | `-i` | 会话 ID：完整 ID、唯一前缀或 JSONL 文件路径均可 |
| `--query` | `-q` | 按请求或最终响应文本过滤（不区分大小写） |
| `--turn` | `-t` | 配合 `-i` 输出指定问题（从 1 起）的完整详情 |
| `--think` | — | 配合 `-i` 输出中间思考过程 |
| `--transcript` | — | 配合 `-i` 只输出纯净的 user/assistant 对话全文（剔除工具调用、思考、系统注入内容），默认 JSONL，`--format md` 输出 Markdown |
| `--stat` | — | 只输出元信息，`--format table/csv` 控制格式 |
| `--plan` | — | 只显示关联了 plan 文件的会话 |
| `--archived` | — | 列出 Codex 会话时包含已归档会话 |
| `--source` | `-s` | `all/codex/claude/qoder/qoder-app`，默认 all |
| `--date` | `-d` | `YYYY-MM-DD`、`yesterday` 或 `all`，按 TZ 过滤，默认今天 |
| `--format` | `-f` | `-stat` 用 `table/csv`；`-transcript` 用 `jsonl`（默认）/`md` |
| `--codex-db` | — | Codex 历史数据库，默认 `~/.codex/thread_history_1.sqlite` |
| `--claude-dir` | — | Claude 数据目录，默认 `~/.claude` |
| `--qoder-dir` | — | Qoder 数据目录，默认 `~/.qoder-cn` |
| `--qoder-app-dir` | — | Qoder 应用会话目录，默认 `~/.qoder-cn/cache/projects` |

## 数据来源

- Codex：内容读 `thread_history_1.sqlite`；Token 从 `~/.codex/sessions/`、`~/.codex/archived_sessions/` 下 rollout JSONL 的 `token_count` 汇总；详情头部的 `Path:` 给出该 rollout JSONL 路径。
- Claude：`~/.claude/projects/`、`~/.claude/transcripts/` 的 JSONL，plan 按 slug 关联 `~/.claude/plans/`。
- Qoder（CLI）：`~/.qoder-cn/projects/` 的 JSONL。
- Qoder App：`~/.qoder-cn/cache/projects`，时间戳取自应用状态库，轮次用时为近似值。
