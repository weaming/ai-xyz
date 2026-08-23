package main

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/weaming/printable/go-column"
)

func printResults(models []Model, total int, ranked bool) {
	if ranked {
		fmt.Println("=== OpenRouter 流行模型（最近一周 token 用量倒序）===")
	} else {
		fmt.Println("=== OpenRouter 模型 ===")
	}
	fmt.Printf("筛选结果：%d 个模型（共 %d 个）\n", len(models), total)

	var lines []string
	header := "模型\t名称\t上下文\t价格/百万\t编程分\t智能分\t综合得分\t免费"
	if ranked {
		header = "排名\t" + header
	}
	lines = append(lines, header)
	for i, m := range models {
		coding := 0.0
		intel := 0.0
		if m.Benchmarks.ArtificialAnalysis != nil {
			coding = m.Benchmarks.ArtificialAnalysis.CodingIndex
			intel = m.Benchmarks.ArtificialAnalysis.IntelligenceIndex
		}
		freeStr := "-"
		if isFree(m) {
			freeStr = "是"
		}
		pricePrompt := m.Pricing.Prompt
		if pricePrompt == "" {
			pricePrompt = "0"
		}
		score := computeCompositeScoreForModel(m.ID, coding, intel, pricePrompt, m.Pricing.Completion)
		line := fmt.Sprintf("%s\t%s\t%d\t%s\t%.1f\t%.1f\t%.1f\t%s",
			m.ID, m.Name, m.ContextLen, formatPricePerMillion(pricePrompt), coding, intel, score, freeStr)
		if ranked {
			line = fmt.Sprintf("%d\t%s", i+1, line)
		}
		lines = append(lines, line)
	}

	input := []byte(strings.Join(lines, "\n") + "\n")
	output, err := column.Render(input, []column.ColumnOption{
		{Name: "table"},
		{Name: "separator", Value: strPtr("\t")},
		{Name: "output-separator", Value: strPtr("\t")},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "格式化失败，回退到原始输出：%v\n", err)
		for _, line := range lines {
			fmt.Println(line)
		}
		return
	}
	fmt.Println(output)
}

func printAAResults(models []AAModel, limit int) {
	type scored struct {
		model AAModel
		score float64
	}
	var filteredAA []AAModel
	for _, m := range models {
		if m.Evaluations.CodingIndex >= 50 {
			filteredAA = append(filteredAA, m)
		}
	}
	scoredList := make([]scored, len(filteredAA))
	for i, m := range filteredAA {
		scoredList[i] = scored{
			model: m,
			score: computeCompositeScorePerMillionWithCachePriceRatio(
				m.Evaluations.CodingIndex,
				m.Evaluations.IntelligenceIndex,
				m.Pricing.InputPer1M,
				m.Pricing.OutputPer1M,
				cachedInputPriceRatioForModel(m.Slug),
			),
		}
	}
	sort.Slice(scoredList, func(i, j int) bool {
		if scoredList[i].score != scoredList[j].score {
			return scoredList[i].score > scoredList[j].score
		}
		if scoredList[i].model.Name != scoredList[j].model.Name {
			return scoredList[i].model.Name < scoredList[j].model.Name
		}
		return scoredList[i].model.Slug < scoredList[j].model.Slug
	})

	if limit > 0 && len(scoredList) > limit {
		scoredList = scoredList[:limit]
	}

	fmt.Println("=== Artificial Analysis 评分数据 ===")
	fmt.Printf("AA 模型：%d 个（按综合得分倒序，保留 1 位小数）\n", len(scoredList))
	var lines []string
	lines = append(lines, "模型名\tSlug\t综合得分\tAA编程分\tAA智能分\tAA代理分\t输入/百万\t输出/百万")
	for _, s := range scoredList {
		m := s.model
		coding := m.Evaluations.CodingIndex
		intel := m.Evaluations.IntelligenceIndex
		agent := m.Evaluations.AgenticIndex
		priceIn := fmt.Sprintf("%.2f", m.Pricing.InputPer1M)
		priceOut := fmt.Sprintf("%.2f", m.Pricing.OutputPer1M)
		if m.Pricing.InputPer1M == 0 && m.Pricing.OutputPer1M == 0 {
			priceIn = "0"
			priceOut = "0"
		}
		lines = append(lines, fmt.Sprintf("%s\t%s\t%.1f\t%.1f\t%.1f\t%.1f\t%s\t%s",
			m.Name, m.Slug, s.score, coding, intel, agent, priceIn, priceOut))
	}
	input := []byte(strings.Join(lines, "\n") + "\n")
	output, err := column.Render(input, []column.ColumnOption{
		{Name: "table"},
		{Name: "separator", Value: strPtr("\t")},
		{Name: "output-separator", Value: strPtr("\t")},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "AA 格式化失败：%v\n", err)
		for _, line := range lines {
			fmt.Println(line)
		}
		return
	}
	fmt.Println(output)
}

// channelRow 保存单条渠道展示所需的数据：缺失指标已回退，评分已计算。
type channelRow struct {
	e          ModelEndpoint
	throughput float64
	latency    float64
	uptime     float64
	score      float64
}

func printEndpoints(info ModelEndpointsInfo) {
	fmt.Printf("=== %s（%s）的 Providers ===\n", info.Name, info.ID)
	fmt.Printf("共 %d 个渠道（按渠道评分降序，价格单位：美元/百万 token）\n", len(info.Endpoints))

	rows := scoreEndpoints(info)

	var lines []string
	lines = append(lines, "Provider\t渠道\ttag\t评分\t上下文\t最大输出\t输入/百万\t输出/百万\t免费\t吞吐p50(t/s)\t延迟p50(ms)\t可用率1d\t量化\t隐式缓存")
	for _, row := range rows {
		e := row.e
		freeStr := "-"
		if isFreeEndpoint(e) {
			freeStr = "是"
		}
		throughput := "-"
		if e.ThroughputLast30m != nil {
			throughput = fmt.Sprintf("%.1f", e.ThroughputLast30m.P50)
		}
		latency := "-"
		if e.LatencyLast30m != nil {
			latency = fmt.Sprintf("%.1f", e.LatencyLast30m.P50)
		}
		uptime := "-"
		if e.UptimeLast1d != nil {
			uptime = fmt.Sprintf("%.1f%%", *e.UptimeLast1d)
		}
		quantization := e.Quantization
		if quantization == "" {
			quantization = "-"
		}
		cacheStr := "否"
		if e.SupportsImplicitCache {
			cacheStr = "是"
		}
		tag := e.Tag
		if tag == "" {
			tag = "-"
		}
		lines = append(lines, fmt.Sprintf("%s\t%s\t%s\t%.1f\t%d\t%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s",
			e.ProviderName, endpointShortName(e), tag, row.score, e.ContextLength, e.MaxCompletionTokens,
			formatPricePerMillion(e.Pricing.Prompt), formatPricePerMillion(e.Pricing.Completion),
			freeStr, throughput, latency, uptime, quantization, cacheStr))
	}

	input := []byte(strings.Join(lines, "\n") + "\n")
	output, err := column.Render(input, []column.ColumnOption{
		{Name: "table"},
		{Name: "separator", Value: strPtr("\t")},
		{Name: "output-separator", Value: strPtr("\t")},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "格式化失败，回退到原始输出：%v\n", err)
		for _, line := range lines {
			fmt.Println(line)
		}
		return
	}
	fmt.Println(output)
}

// scoreEndpoints 为每条渠道计算评分（缺失指标用同模型中位数回退），并按评分降序排序。
func scoreEndpoints(info ModelEndpointsInfo) []channelRow {
	var throughputs, latencies, uptimes []float64
	for _, e := range info.Endpoints {
		if e.ThroughputLast30m != nil {
			throughputs = append(throughputs, e.ThroughputLast30m.P50)
		}
		if e.LatencyLast30m != nil {
			latencies = append(latencies, e.LatencyLast30m.P50)
		}
		if e.UptimeLast1d != nil {
			uptimes = append(uptimes, *e.UptimeLast1d)
		}
	}
	throughputFallback, ok := medianFloat(throughputs)
	if !ok {
		throughputFallback = channelThroughputRefTPS
	}
	latencyFallback, ok := medianFloat(latencies)
	if !ok {
		latencyFallback = channelLatencyRefMS
	}
	uptimeFallback, ok := medianFloat(uptimes)
	if !ok {
		uptimeFallback = channelFallbackUptimePct
	}

	rows := make([]channelRow, len(info.Endpoints))
	for i, e := range info.Endpoints {
		row := channelRow{e: e, throughput: throughputFallback, latency: latencyFallback, uptime: uptimeFallback}
		if e.ThroughputLast30m != nil {
			row.throughput = e.ThroughputLast30m.P50
		}
		if e.LatencyLast30m != nil {
			row.latency = e.LatencyLast30m.P50
		}
		if e.UptimeLast1d != nil {
			row.uptime = *e.UptimeLast1d
		}
		row.score = computeChannelScore(e, info.ID, row.throughput, row.latency, row.uptime)
		rows[i] = row
	}

	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].score != rows[j].score {
			return rows[i].score > rows[j].score
		}
		return rows[i].e.ProviderName < rows[j].e.ProviderName
	})
	return rows
}

func printChannelFormulaDescription() {
	fmt.Println("=== 渠道评分计算公式 ===")
	fmt.Println(channelScoreFormulaDescription)
}

// endpointShortName 去掉渠道名中重复的 Provider 前缀，
// 支持 "Provider | xxx" 与 "Provider: xxx" 两种命名格式。
func endpointShortName(e ModelEndpoint) string {
	for _, sep := range []string{" | ", ": "} {
		prefix := e.ProviderName + sep
		if strings.HasPrefix(e.Name, prefix) {
			return strings.TrimPrefix(e.Name, prefix)
		}
	}
	return e.Name
}

// isFreeEndpoint 判断渠道是否免费（输入与输出价均为 0）。
func isFreeEndpoint(e ModelEndpoint) bool {
	inputPrice, inputErr := parsePrice(e.Pricing.Prompt)
	outputPrice, outputErr := parsePrice(e.Pricing.Completion)
	return inputErr == nil && outputErr == nil && inputPrice == 0 && outputPrice == 0
}

func printFormulaDescription() {
	fmt.Println("=== 综合分计算公式 ===")
	fmt.Println(compositeFormulaDescription)
}

func strPtr(s string) *string {
	return &s
}

// formatPricePerMillion 将 OpenRouter 的每 Token 价格转换为每百万 Token 价格。
func formatPricePerMillion(raw string) string {
	value, err := parsePrice(raw)
	if err != nil {
		return raw
	}
	if value == 0 {
		return "0"
	}
	formatted := strconv.FormatFloat(value*openRouterPriceMultiplier, 'f', 6, 64)
	formatted = strings.TrimRight(formatted, "0")
	return strings.TrimRight(formatted, ".")
}
