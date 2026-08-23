package main

import (
	"errors"
	"math"
	"sort"
	"strconv"
	"strings"
)

const (
	// 根据 ai-sessions -d all 的约 6,714M 输入 Token 和 20.1M 输出 Token 估计。
	averageCacheHitRate = 0.978
	// 输入输出占比约为 99.7% / 0.3%。
	inputTokenUsageWeight  = 0.997
	outputTokenUsageWeight = 0.003
	// Claude 和 Codex 的缓存命中价约为普通输入价的 10%。
	cachedInputPriceRatio = 0.1
	// DeepSeek V4 的缓存命中价约为未命中输入价的 3%。
	deepSeekCachedInputPriceRatio = 0.03
	priceBaselinePerMillion       = 1
	openRouterPriceMultiplier     = 1_000_000
	compositeFormulaDescription   = "综合分 = (编程分×60% + 智能分×40%) × 价格系数\n价格系数 = 1 ÷ (1 + 有效价格 ÷ 1美元/百万token)，结果限制在 0~100\n有效输入价 = 输入价×[(1−97.8%) + 97.8%×命中价系数]; DeepSeek 命中价系数约 3%，其他模型按 10%\n有效价格 = 有效输入价×99.7% + 输出价×0.3%"

	// 渠道评分参考点：吞吐 50 t/s 时吞吐因子为 0.5，延迟 1000ms 时延迟因子为 0.5。
	channelThroughputRefTPS = 50.0
	channelLatencyRefMS     = 1000.0
	// 可用率指数：99.9%→≈0.99，99%→≈0.92，97%→≈0.78，85%→≈0.32，0%→0。
	channelUptimeExponent = 8.0
	// 吞吐/延迟/可用率全部缺失时的回退值（回退到参考点即因子 0.5）。
	channelFallbackUptimePct       = 99.0
	channelScoreFormulaDescription = "渠道评分 = 100 × 价格因子 × 吞吐因子 × 延迟因子 × 可用率因子，限制在 0~100\n价格因子 = 1 ÷ (1 + 有效价格 ÷ 1美元/百万token)，有效价格沿用综合分的缓存命中折算；不支持隐式缓存的渠道不折扣\n吞吐因子 = T ÷ (T + 50 t/s)；延迟因子 = 1000ms ÷ (1000ms + L)；可用率因子 = (可用率/100)^8\n吞吐/延迟/可用率缺失时取同模型渠道中位数，无任何数据时按参考点折半（因子 0.5）/可用率按 99% 计"
)

// computeCompositeScore 兼容 OpenRouter 的价格格式（每 token），并返回 0~100 分。
func computeCompositeScore(coding, intel float64, pricePrompt, priceCompletion string) float64 {
	return computeCompositeScoreWithCachePriceRatio(
		coding,
		intel,
		pricePrompt,
		priceCompletion,
		cachedInputPriceRatio,
	)
}

// computeCompositeScoreForModel 按模型标识选择缓存命中价格后计算综合分。
func computeCompositeScoreForModel(modelID string, coding, intel float64, pricePrompt, priceCompletion string) float64 {
	return computeCompositeScoreWithCachePriceRatio(
		coding,
		intel,
		pricePrompt,
		priceCompletion,
		cachedInputPriceRatioForModel(modelID),
	)
}

func computeCompositeScoreWithCachePriceRatio(coding, intel float64, pricePrompt, priceCompletion string, cachePriceRatio float64) float64 {
	inputPrice, inputErr := parsePrice(pricePrompt)
	outputPrice, outputErr := parsePrice(priceCompletion)
	if inputErr != nil || outputErr != nil {
		return 0
	}
	return computeCompositeScorePerMillionWithCachePriceRatio(
		coding,
		intel,
		inputPrice*openRouterPriceMultiplier,
		outputPrice*openRouterPriceMultiplier,
		cachePriceRatio,
	)
}

// computeCompositeScorePerMillion 使用每百万 token 的价格计算综合分。
func computeCompositeScorePerMillion(coding, intel, inputPrice, outputPrice float64) float64 {
	return computeCompositeScorePerMillionWithCachePriceRatio(
		coding,
		intel,
		inputPrice,
		outputPrice,
		cachedInputPriceRatio,
	)
}

// computeCompositeScorePerMillionWithCachePriceRatio 使用指定缓存命中价格计算综合分。
func computeCompositeScorePerMillionWithCachePriceRatio(coding, intel, inputPrice, outputPrice, cachePriceRatio float64) float64 {
	if coding < 0 {
		coding = 0
	}
	if intel < 0 {
		intel = 0
	}
	if inputPrice < 0 {
		inputPrice = 0
	}
	if outputPrice < 0 {
		outputPrice = 0
	}
	if cachePriceRatio < 0 {
		cachePriceRatio = 0
	}

	quality := coding*0.6 + intel*0.4
	cacheAdjustedInputPrice := inputPrice * ((1 - averageCacheHitRate) + averageCacheHitRate*cachePriceRatio)
	effectivePrice := cacheAdjustedInputPrice*inputTokenUsageWeight + outputPrice*outputTokenUsageWeight
	priceFactor := 1.0 / (1.0 + effectivePrice/priceBaselinePerMillion)
	score := quality * priceFactor
	if score > 100 {
		return 100
	}
	return score
}

func cachedInputPriceRatioForModel(modelID string) float64 {
	if strings.Contains(strings.ToLower(modelID), "deepseek") {
		return deepSeekCachedInputPriceRatio
	}
	return cachedInputPriceRatio
}

// channelEffectivePricePerMillion 计算渠道有效价格（美元/百万 token），
// 与模型综合分同口径；不支持隐式缓存的渠道缓存命中价系数按 1（无折扣）。
func channelEffectivePricePerMillion(e ModelEndpoint, modelID string) float64 {
	inputPrice, inputErr := parsePrice(e.Pricing.Prompt)
	outputPrice, outputErr := parsePrice(e.Pricing.Completion)
	if inputErr != nil || outputErr != nil {
		return 0
	}
	cacheRatio := 1.0
	if e.SupportsImplicitCache {
		cacheRatio = cachedInputPriceRatioForModel(modelID)
	}
	cacheAdjustedInputPrice := inputPrice * openRouterPriceMultiplier * ((1 - averageCacheHitRate) + averageCacheHitRate*cacheRatio)
	return cacheAdjustedInputPrice*inputTokenUsageWeight + outputPrice*openRouterPriceMultiplier*outputTokenUsageWeight
}

// computeChannelScore 渠道评分（0~100）：价格、吞吐、延迟、可用性四个因子相乘。
// throughputTPS/latencyMS/uptimePct 传入前需先回退缺失值（中位数或参考点）。
func computeChannelScore(e ModelEndpoint, modelID string, throughputTPS, latencyMS, uptimePct float64) float64 {
	if throughputTPS < 0 {
		throughputTPS = 0
	}
	if latencyMS < 0 {
		latencyMS = 0
	}
	if uptimePct < 0 {
		uptimePct = 0
	}
	priceFactor := 1.0 / (1.0 + channelEffectivePricePerMillion(e, modelID)/priceBaselinePerMillion)
	throughputFactor := throughputTPS / (throughputTPS + channelThroughputRefTPS)
	latencyFactor := channelLatencyRefMS / (channelLatencyRefMS + latencyMS)
	uptimeFactor := math.Pow(uptimePct/100, channelUptimeExponent)
	score := 100 * priceFactor * throughputFactor * latencyFactor * uptimeFactor
	if score > 100 {
		return 100
	}
	return score
}

// medianFloat 返回中位数；空切片返回 false。
func medianFloat(values []float64) (float64, bool) {
	if len(values) == 0 {
		return 0, false
	}
	sorted := make([]float64, len(values))
	copy(sorted, values)
	sort.Float64s(sorted)
	mid := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[mid], true
	}
	return (sorted[mid-1] + sorted[mid]) / 2, true
}

func parsePrice(raw string) (float64, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return 0, errors.New("price is empty")
	}
	return strconv.ParseFloat(value, 64)
}
