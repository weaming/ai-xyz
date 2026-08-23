package main

import "testing"

func TestFilterAndSortKeepOrder(t *testing.T) {
	all := []Model{
		{Name: "popular-no-bench", ID: "a"},
		{
			Name: "less-popular-high-coding",
			ID:   "b",
			Benchmarks: Bench{ArtificialAnalysis: &ArtificialAnalysis{
				CodingIndex: 90,
			}},
		},
	}

	kept := filterAndSort(all, "all", 0, true)
	if len(kept) != 2 || kept[0].ID != "a" || kept[1].ID != "b" {
		t.Fatalf("keepOrder changed popularity order: %+v", kept)
	}

	sorted := filterAndSort(all, "all", 0, false)
	if len(sorted) != 2 || sorted[0].ID != "b" {
		t.Fatalf("coding sort not applied: %+v", sorted)
	}
}

func TestScoreEndpointsFallbackAndOrder(t *testing.T) {
	uptime := 99.9
	info := ModelEndpointsInfo{
		ID: "test/model",
		Endpoints: []ModelEndpoint{
			{ // 性能差但便宜：不应排第一（延迟/吞吐因子太低）
				ProviderName:      "CheapButSlow",
				Pricing:           EndpointPricing{Prompt: "0.0000001", Completion: "0.0000002"},
				ThroughputLast30m: &EndpointPercentiles{P50: 5},
				LatencyLast30m:    &EndpointPercentiles{P50: 8000},
				UptimeLast1d:      &uptime,
			},
			{ // 性能好的渠道应排第一；吞吐缺失时用中位数回退后评分仍应大于 0。
				ProviderName:      "Fast",
				Pricing:           EndpointPricing{Prompt: "0.0000001", Completion: "0.0000002"},
				ThroughputLast30m: &EndpointPercentiles{P50: 120},
				LatencyLast30m:    &EndpointPercentiles{P50: 500},
				UptimeLast1d:      &uptime,
			},
			{ // 无任何性能数据：回退到中位数，评分介于两者之间。
				ProviderName: "Unknown",
				Pricing:      EndpointPricing{Prompt: "0.0000001", Completion: "0.0000002"},
			},
		},
	}

	rows := scoreEndpoints(info)
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(rows))
	}
	if rows[0].e.ProviderName != "Fast" {
		t.Fatalf("first row = %s, want Fast", rows[0].e.ProviderName)
	}
	if rows[2].e.ProviderName != "CheapButSlow" {
		t.Fatalf("last row = %s, want CheapButSlow", rows[2].e.ProviderName)
	}
	for i := 1; i < len(rows); i++ {
		if rows[i-1].score < rows[i].score {
			t.Fatalf("rows not sorted descending: %v", rows)
		}
	}
	if rows[1].score <= 0 {
		t.Fatalf("fallback row score = %f, want > 0", rows[1].score)
	}
}

func TestEndpointShortName(t *testing.T) {
	cases := []struct {
		name         string
		providerName string
		want         string
	}{
		{"Amazon Bedrock | anthropic/claude-opus-5-20260723", "Amazon Bedrock", "anthropic/claude-opus-5-20260723"},
		{"OpenAI: GPT-4", "OpenAI", "GPT-4"},
		{"Ox Alpha", "Stealth", "Ox Alpha"},
	}
	for _, c := range cases {
		e := ModelEndpoint{Name: c.name, ProviderName: c.providerName}
		if got := endpointShortName(e); got != c.want {
			t.Errorf("endpointShortName(%q) = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestSortByCompositeScore(t *testing.T) {
	models := []Model{
		{
			Name: "higher coding, expensive",
			Pricing: Price{
				Prompt:     "0.00001",
				Completion: "0.0001",
			},
			Benchmarks: Bench{ArtificialAnalysis: &ArtificialAnalysis{
				CodingIndex:       80,
				IntelligenceIndex: 10,
			}},
		},
		{
			Name: "lower coding, free",
			Pricing: Price{
				Prompt:     "0",
				Completion: "0",
			},
			Benchmarks: Bench{ArtificialAnalysis: &ArtificialAnalysis{
				CodingIndex:       70,
				IntelligenceIndex: 70,
			}},
		},
	}

	sortByCompositeScore(models)
	if models[0].Name != "lower coding, free" {
		t.Fatalf("first model = %q, want lower coding, free", models[0].Name)
	}
}
