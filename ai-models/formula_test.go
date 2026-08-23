package main

import (
	"math"
	"testing"
)

func TestCompositeScoreUsesExplicitPriceUnits(t *testing.T) {
	openRouterScore := computeCompositeScore(56.2, 42.1, "0.00000013", "0.00000028")
	aaScore := computeCompositeScorePerMillion(56.2, 42.1, 0.13, 0.28)

	if math.Abs(openRouterScore-aaScore) > 1e-9 {
		t.Fatalf("OpenRouter score = %f, AA score = %f", openRouterScore, aaScore)
	}
	if openRouterScore <= 0 || openRouterScore >= 100 {
		t.Fatalf("score = %f, want a non-saturated score in (0, 100)", openRouterScore)
	}
}

func TestCompositeScorePriceChangesScore(t *testing.T) {
	cheap := computeCompositeScorePerMillion(80, 70, 0.1, 0.2)
	expensive := computeCompositeScorePerMillion(80, 70, 10, 20)

	if cheap <= expensive {
		t.Fatalf("cheap score = %f, expensive score = %f", cheap, expensive)
	}
	if cheap > 100 || expensive < 0 {
		t.Fatalf("scores out of range: cheap=%f expensive=%f", cheap, expensive)
	}
}

func TestCompositeScoreIncludesCacheHitCost(t *testing.T) {
	inputPrice := 1.0
	outputPrice := 0.0
	cacheAdjustedInputPrice := inputPrice * ((1 - averageCacheHitRate) + averageCacheHitRate*cachedInputPriceRatio)
	effectivePrice := cacheAdjustedInputPrice*inputTokenUsageWeight + outputPrice*outputTokenUsageWeight
	expected := 80 / (1 + effectivePrice/priceBaselinePerMillion)
	actual := computeCompositeScorePerMillion(80, 80, inputPrice, outputPrice)

	if math.Abs(actual-expected) > 1e-9 {
		t.Fatalf("score = %f, want %f", actual, expected)
	}
	if inputTokenUsageWeight+outputTokenUsageWeight != 1 {
		t.Fatalf("usage weights = %f, want 1", inputTokenUsageWeight+outputTokenUsageWeight)
	}
}

func TestParsePrice(t *testing.T) {
	if value, err := parsePrice(" 0.000001 "); err != nil || value != 0.000001 {
		t.Fatalf("parsePrice returned (%f, %v)", value, err)
	}
	if _, err := parsePrice(""); err == nil {
		t.Fatal("empty price should return an error")
	}
	if _, err := parsePrice("not-a-price"); err == nil {
		t.Fatal("invalid price should return an error")
	}
}

func TestDeepSeekV4FlashCompositeScoreIsPresent(t *testing.T) {
	score := computeCompositeScorePerMillion(69.1, 51.8, 0.44, 1.32)
	if score <= 0 || score >= 100 {
		t.Fatalf("DeepSeek V4 Flash score = %f, want a non-saturated score in (0, 100)", score)
	}
}

func TestDeepSeekUsesSpecificCacheHitPrice(t *testing.T) {
	generalScore := computeCompositeScoreForModel("anthropic/claude-sonnet", 80, 80, "1", "0")
	deepSeekScore := computeCompositeScoreForModel("deepseek/deepseek-v4-flash", 80, 80, "1", "0")
	if deepSeekScore <= generalScore {
		t.Fatalf("DeepSeek score = %f, general score = %f", deepSeekScore, generalScore)
	}
	if got := cachedInputPriceRatioForModel("deepseek/deepseek-v4-pro"); got != deepSeekCachedInputPriceRatio {
		t.Fatalf("DeepSeek cache ratio = %f, want %f", got, deepSeekCachedInputPriceRatio)
	}
	if got := cachedInputPriceRatioForModel("openai/gpt-5.3-codex"); got != cachedInputPriceRatio {
		t.Fatalf("general cache ratio = %f, want %f", got, cachedInputPriceRatio)
	}
}

func TestFormatPricePerMillion(t *testing.T) {
	cases := map[string]string{
		"0":                   "0",
		"0.00000013":          "0.13",
		"0.00000019999999998": "0.2",
		"0.000001":            "1",
		"not-a-price":         "not-a-price",
	}
	for raw, want := range cases {
		if got := formatPricePerMillion(raw); got != want {
			t.Fatalf("formatPricePerMillion(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestComputeChannelScoreFormula(t *testing.T) {
	uptime := 99.9
	e := ModelEndpoint{
		ProviderName:          "Test",
		Pricing:               EndpointPricing{Prompt: "0.000001", Completion: "0.000002"},
		SupportsImplicitCache: true,
		UptimeLast1d:          &uptime,
	}
	score := computeChannelScore(e, "openai/gpt-4", 100, 500, 99.9)

	// 手工复算期望值：价格因子、吞吐因子、延迟因子、可用率因子相乘。
	effectivePrice := channelEffectivePricePerMillion(e, "openai/gpt-4")
	expected := 100 *
		(1.0 / (1.0 + effectivePrice/priceBaselinePerMillion)) *
		(100.0 / (100.0 + channelThroughputRefTPS)) *
		(channelLatencyRefMS / (channelLatencyRefMS + 500)) *
		math.Pow(99.9/100, channelUptimeExponent)
	if math.Abs(score-expected) > 1e-9 {
		t.Fatalf("score = %f, want %f", score, expected)
	}
	if score <= 0 || score >= 100 {
		t.Fatalf("score = %f, want a non-saturated score in (0, 100)", score)
	}
}

func TestComputeChannelScoreFactors(t *testing.T) {
	base := ModelEndpoint{Pricing: EndpointPricing{Prompt: "0.000001", Completion: "0.000002"}}
	// 吞吐更高、延迟更低、可用率更高都应提高评分。
	higher := computeChannelScore(base, "m", 100, 500, 99.9)
	lower := computeChannelScore(base, "m", 20, 3000, 95)
	if higher <= lower {
		t.Fatalf("higher score = %f, lower = %f", higher, lower)
	}
	// 可用率为 0 时评分应为 0。
	if got := computeChannelScore(base, "m", 100, 500, 0); got != 0 {
		t.Fatalf("zero uptime score = %f, want 0", got)
	}
	// 更贵的渠道评分更低。
	slightlyCheaper := ModelEndpoint{Pricing: EndpointPricing{Prompt: "0.0000001", Completion: "0.0000002"}}
	if computeChannelScore(slightlyCheaper, "m", 100, 500, 99.9) <= higher {
		t.Fatal("cheaper channel should score higher")
	}
}

func TestChannelCacheDiscountRequiresImplicitCaching(t *testing.T) {
	withCache := ModelEndpoint{Pricing: EndpointPricing{Prompt: "0.000001", Completion: "0"}, SupportsImplicitCache: true}
	noCache := ModelEndpoint{Pricing: EndpointPricing{Prompt: "0.000001", Completion: "0"}, SupportsImplicitCache: false}
	if channelEffectivePricePerMillion(withCache, "m") >= channelEffectivePricePerMillion(noCache, "m") {
		t.Fatal("implicit-caching channel should have a lower effective price")
	}
}

func TestMedianFloat(t *testing.T) {
	if got, ok := medianFloat([]float64{3, 1, 2}); !ok || got != 2 {
		t.Fatalf("odd median = %f, ok=%v", got, ok)
	}
	if got, ok := medianFloat([]float64{1, 2, 3, 4}); !ok || got != 2.5 {
		t.Fatalf("even median = %f, ok=%v", got, ok)
	}
	if _, ok := medianFloat(nil); ok {
		t.Fatal("empty slice should return false")
	}
}

func TestIsFreeRequiresBothPrices(t *testing.T) {
	free := Model{Pricing: Price{Prompt: "0.0", Completion: "0"}}
	partiallyPriced := Model{Pricing: Price{Prompt: "0", Completion: "0.000001"}}
	missingPrice := Model{Pricing: Price{Prompt: "", Completion: "0"}}

	if !isFree(free) {
		t.Fatal("zero input and output prices should be free")
	}
	if isFree(partiallyPriced) || isFree(missingPrice) {
		t.Fatal("partially priced or missing-priced model should not be free")
	}
}
