# ai-sessions

解析本机 Codex、Claude、Qoder 会话历史，输出用户输入、工具调用和最终回答，并统计 Token 用量。

## 用法

```sh
ai-sessions [选项]
```

示例：

```sh
ai-sessions                    # 列出今天的会话索引
ai-sessions -d all             # 全部日期的会话索引
ai-sessions -q tantivy         # 按请求或最终响应文本过滤（不区分大小写）
ai-sessions --source claude    # 只看 claude 来源
ai-sessions -i 019abc -t 2     # 指定会话第 2 问完整详情：问题、工具调用输入输出和回答
ai-sessions -i 019abc --think  # 输出该会话的中间思考过程
```

选项：

```text
-i, --session ID    会话 ID，支持唯一前缀或 JSONL 文件路径
-q, --query 文本    按请求或最终响应文本过滤
-t, --turn 序号     配合 --session，输出指定问题的完整详情
--think             配合 --session，输出中间思考过程
-s, --source 来源   all/codex/claude/qoder，默认 all
-d, --date 日期     YYYY-MM-DD 或 all，按 TZ 时区过滤，默认今天
--codex-db 路径     Codex 历史数据库，默认 ~/.codex/thread_history_1.sqlite
--claude-dir 路径   Claude 数据目录，默认 ~/.claude
--qoder-dir 路径    Qoder 数据目录，默认 ~/.qoder-cn
```

Token 统计：Claude/Qoder 从 JSONL 的 usage 字段汇总；Codex 从 `~/.codex/sessions/` 下 rollout JSONL 的 `token_count` 汇总累计用量和请求缓存命中率。
