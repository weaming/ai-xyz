# ai-models

通过 OpenRouter 抓取模型列表，按编程评分（coding_index）筛选；支持流行度榜和单模型多渠道价格对比。

环境变量：

- `OPENROUTER_API_KEY`（必需）
- `ARTIFICIAL_ANALYSIS_API_KEY`（可选，`-aa` 时用于额外评分验证）

## 用法

```sh
ai-models [选项]
```

示例：

```sh
ai-models                                  # 默认：编程分 >= 50 的全部模型
ai-models -free                            # 只看免费模型
ai-models -trend -limit 30                 # 最近一周 token 用量流行榜
ai-models -sort -limit 10                  # 综合得分倒序前 10
ai-models -model anthropic/claude-opus-5   # 该模型在各 provider 的价格与性能
ai-models -json                            # 输出完整 JSON
```

选项：

```text
-free            只显示免费模型
-paid            只显示收费模型
-min-coding      最低编程评分，默认 50；-trend/-free 下默认 0（不限制）
-limit N         最大输出数量，默认 20，0 表示不限
-sort            按综合得分倒序排序
-json            输出完整 JSON，不裁剪内容
-aa              同时抓取 artificialanalysis.ai 评分
-trend           按 OpenRouter 流行度排序
-model author/slug   查看该模型在各 provider 的价格与性能
```

## 说明

- Artificial Analysis 数据缓存一小时到 `~/.cache/ai-models/aa.json`。
- 综合分统一按每百万 token 价格计算；文本输出依次为 OpenRouter 表格、Artificial Analysis 表格和综合分公式说明。
