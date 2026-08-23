package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
)

func main() {
	var (
		filterFree = flag.Bool("free", false, "只显示免费模型")
		filterPaid = flag.Bool("paid", false, "只显示收费模型")
		minCoding  = flag.Float64("min-coding", -1, "最低编程评分（coding_index），默认 50；-trend 或 -free 模式下默认 0（不限制）")
		limit      = flag.Int("limit", 20, "最大输出数量，0 表示不限")
		sortScore  = flag.Bool("sort", false, "按综合得分倒序排序")
		asJSON     = flag.Bool("json", false, "输出完整 JSON，不裁剪任何内容")
		useAA      = flag.Bool("aa", false, "同时抓取 artificialanalysis.ai 免费端点评分（需 ARTIFICIAL_ANALYSIS_API_KEY）")
		popular    = flag.Bool("trend", false, "按 OpenRouter 流行度排序（最近一周 token 用量倒序，保留服务端返回顺序）")
		modelID    = flag.String("model", "", "查看指定模型（author/slug）在各 provider 的价格与性能，如 anthropic/claude-opus-5")
	)
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "用法：%s [选项]\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "通过 openrouter 抓取模型列表，按编程评分（coding_index）筛选。\n")
		fmt.Fprintf(os.Stderr, "使用 -trend 可按 OpenRouter 最近一周 token 用量查看流行模型榜。\n")
		fmt.Fprintf(os.Stderr, "使用 -model author/slug 可查看该模型在各 provider 的价格与性能。\n")
		fmt.Fprintf(os.Stderr, "环境变量：OPENROUTER_API_KEY（必需），ARTIFICIAL_ANALYSIS_API_KEY（可选，用于额外评分验证）\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	if *limit < 0 {
		fmt.Fprintln(os.Stderr, "错误：--limit 不能小于 0")
		os.Exit(2)
	}
	if *minCoding < 0 {
		if *popular || *filterFree {
			*minCoding = 0
		} else {
			*minCoding = 50
		}
	}

	mode := "all"
	if *filterFree {
		mode = "free"
	} else if *filterPaid {
		mode = "paid"
	}

	key := os.Getenv("OPENROUTER_API_KEY")
	if key == "" {
		fmt.Fprintln(os.Stderr, "错误：未设置环境变量 OPENROUTER_API_KEY")
		fmt.Fprintln(os.Stderr, "提示：从环境读取（不读取 .env 文件），请先 export OPENROUTER_API_KEY=...")
		os.Exit(1)
	}

	if *modelID != "" {
		runEndpointsMode(key, *modelID, *asJSON)
		return
	}

	sortBy := ""
	if *popular {
		sortBy = "most-popular"
	}
	models, err := fetchModels(key, sortBy)
	if err != nil {
		fmt.Fprintf(os.Stderr, "抓取模型列表失败：%v\n", err)
		os.Exit(1)
	}

	filtered := filterAndSort(models, mode, *minCoding, *popular)
	if *sortScore {
		sortByCompositeScore(filtered)
	}

	if *limit > 0 && len(filtered) > *limit {
		filtered = filtered[:*limit]
	}

	var aaModels []AAModel
	var hasAA bool
	if *useAA {
		aaKey := os.Getenv("ARTIFICIAL_ANALYSIS_API_KEY")
		if aaKey == "" {
			fmt.Fprintln(os.Stderr, "警告：未设置 ARTIFICIAL_ANALYSIS_API_KEY，跳过 --aa 抓取")
		} else {
			aaModels, err = fetchAAModels(aaKey)
			if err != nil {
				fmt.Fprintf(os.Stderr, "警告：抓取 artificialanalysis 数据失败：%v（仅显示 openrouter 数据）\n", err)
			} else {
				hasAA = true
			}
		}
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		var payload any = filtered
		if hasAA {
			payload = struct {
				OpenRouter         []Model   `json:"openrouter"`
				ArtificialAnalysis []AAModel `json:"artificial_analysis"`
			}{
				OpenRouter:         filtered,
				ArtificialAnalysis: aaModels,
			}
		}
		if err := enc.Encode(payload); err != nil {
			fmt.Fprintf(os.Stderr, "输出 JSON 失败：%v\n", err)
			os.Exit(1)
		}
		return
	}

	printResults(filtered, len(models), *popular)
	if hasAA {
		fmt.Println()
		printAAResults(aaModels, *limit)
	}
	fmt.Println()
	printFormulaDescription()
}

// runEndpointsMode 查看单个模型在各 provider 的详情，与其他筛选逻辑独立。
func runEndpointsMode(key, modelID string, asJSON bool) {
	author, slug, err := splitModelID(modelID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "错误：%v\n", err)
		os.Exit(2)
	}
	info, err := fetchModelEndpoints(key, author, slug)
	if err != nil {
		fmt.Fprintf(os.Stderr, "抓取模型渠道失败：%v\n", err)
		os.Exit(1)
	}
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(info.Endpoints); err != nil {
			fmt.Fprintf(os.Stderr, "输出 JSON 失败：%v\n", err)
			os.Exit(1)
		}
		return
	}
	printEndpoints(info)
	fmt.Println()
	printChannelFormulaDescription()
}

// splitModelID 拆分 author/slug 形式的模型 ID。
func splitModelID(id string) (string, string, error) {
	parts := strings.SplitN(id, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("模型 ID 格式应为 author/slug，如 anthropic/claude-opus-5，收到 %q", id)
	}
	return parts[0], parts[1], nil
}

func isFree(m Model) bool {
	if strings.TrimSpace(m.Pricing.Prompt) == "" || strings.TrimSpace(m.Pricing.Completion) == "" {
		return false
	}
	inputPrice, inputErr := parsePrice(m.Pricing.Prompt)
	outputPrice, outputErr := parsePrice(m.Pricing.Completion)
	return inputErr == nil && outputErr == nil && inputPrice == 0 && outputPrice == 0
}

func filterAndSort(all []Model, mode string, minCoding float64, keepOrder bool) []Model {
	var result []Model
	for _, m := range all {
		free := isFree(m)
		switch mode {
		case "free":
			if !free {
				continue
			}
		case "paid":
			if free {
				continue
			}
		}
		coding := 0.0
		if m.Benchmarks.ArtificialAnalysis != nil {
			coding = m.Benchmarks.ArtificialAnalysis.CodingIndex
		}
		if coding < minCoding {
			continue
		}
		result = append(result, m)
	}

	// keepOrder 为 true 时保留服务端返回顺序（如 -popular 的流行度排序）。
	if keepOrder {
		return result
	}

	sort.Slice(result, func(i, j int) bool {
		ci := 0.0
		cj := 0.0
		if result[i].Benchmarks.ArtificialAnalysis != nil {
			ci = result[i].Benchmarks.ArtificialAnalysis.CodingIndex
		}
		if result[j].Benchmarks.ArtificialAnalysis != nil {
			cj = result[j].Benchmarks.ArtificialAnalysis.CodingIndex
		}
		if ci != cj {
			return ci > cj
		}
		return result[i].Name < result[j].Name
	})
	return result
}

func sortByCompositeScore(models []Model) {
	sort.SliceStable(models, func(i, j int) bool {
		scoreI := modelCompositeScore(models[i])
		scoreJ := modelCompositeScore(models[j])
		if scoreI != scoreJ {
			return scoreI > scoreJ
		}
		return models[i].Name < models[j].Name
	})
}

func modelCompositeScore(model Model) float64 {
	if model.Benchmarks.ArtificialAnalysis == nil {
		return 0
	}
	benchmarks := model.Benchmarks.ArtificialAnalysis
	return computeCompositeScoreForModel(
		model.ID,
		benchmarks.CodingIndex,
		benchmarks.IntelligenceIndex,
		model.Pricing.Prompt,
		model.Pricing.Completion,
	)
}
