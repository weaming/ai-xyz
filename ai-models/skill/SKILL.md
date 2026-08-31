---
name: ai-models
description: 查询 AI 大模型编程/智能评分排行榜、免费模型、流行度榜单，以及单个模型在各 provider（OpenRouter）的价格对比。当用户询问模型选型、编程模型排行、模型价格对比时使用。
allowed-tools: [Bash]
---

# ai-models 技能

使用本地 `ai-models` 命令通过 OpenRouter 查询模型数据，按编程评分（coding_index）筛选。

## 前置条件

需要环境变量 `OPENROUTER_API_KEY`（已配置）。

可用 `-aa` 附加 artificialanalysis.ai 评分验证（需 `ARTIFICIAL_ANALYSIS_API_KEY`，可选）。

## 常用场景

```sh
ai-models                                  # 默认：编程分 >= 50 的全部模型，按编程分倒序
ai-models -free -limit 10                  # 免费模型 TOP
ai-models -paid -limit 10                  # 收费模型 TOP
ai-models -trend -limit 20                 # 最近一周 token 用量流行榜
ai-models -sort -limit 10                  # 综合得分（编程×60%+智能×40%+价格系数）倒序
ai-models -min-coding 70 -limit 10         # 只看编程分 >= 70 的模型
ai-models -model anthropic/claude-opus-5   # 该模型在各 provider 的价格/延迟/吞吐
ai-models -aa -limit 10                    # 附 artificialanalysis 评分
ai-models -json -limit 5                   # 输出完整 JSON（AI 解析推荐）
```

## 常用 flag

| 选项          | 说明                                                       |
| ------------- | ---------------------------------------------------------- |
| `-free`       | 只显示免费模型（默认 min-coding 变为 0）                    |
| `-paid`       | 只显示收费模型                                               |
| `-min-coding` | 最低编程评分，默认 50；`-trend`/`-free` 下默认 0             |
| `-limit N`    | 最大输出数量，默认 20，0 表示不限                            |
| `-sort`       | 按综合得分倒序                                               |
| `-trend`      | 按 OpenRouter 最近一周 token 用量流行度排序                  |
| `-model`      | 查看指定模型（author/slug）在各 provider 的价格与性能        |
| `-aa`         | 同时抓取 artificialanalysis.ai 评分                          |
| `-json`       | 输出完整 JSON，不裁剪任何内容                                 |

## 说明

- 综合分 = (编程分×60% + 智能分×40%) × 价格系数，价格系数随每百万 token 有效价格衰减。
- 回答模型选型问题时优先用 `-json` 拿全量字段（上下文长度、价格、评分），再结合用户预算/场景给出建议。
- 表格列含：排名、模型 ID、名称、上下文长度、价格/百万、编程分、智能分、综合得分、是否免费。
