package main

// Model / AAModel 数据结构

type AAModel struct {
	ID          string        `json:"id"`
	Name        string        `json:"name"`
	Slug        string        `json:"slug"`
	Evaluations AAEvaluations `json:"evaluations"`
	Pricing     AAPricing     `json:"pricing"`
}

type AAEvaluations struct {
	IntelligenceIndex float64 `json:"artificial_analysis_intelligence_index"`
	CodingIndex       float64 `json:"artificial_analysis_coding_index"`
	AgenticIndex      float64 `json:"artificial_analysis_agentic_index"`
}

type AAPricing struct {
	InputPer1M  float64 `json:"price_1m_input_tokens"`
	OutputPer1M float64 `json:"price_1m_output_tokens"`
}

type aaPagination struct {
	HasMore bool `json:"has_more"`
}

type AAResponse struct {
	Pagination aaPagination `json:"pagination"`
	Data       []AAModel    `json:"data"`
}

type Model struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	ContextLen  int    `json:"context_length"`
	Pricing     Price  `json:"pricing"`
	Benchmarks  Bench  `json:"benchmarks"`
	Description string `json:"description"`
	Expiration  string `json:"expiration_date"`
}

type Price struct {
	Prompt     string `json:"prompt"`
	Completion string `json:"completion"`
}

type Bench struct {
	ArtificialAnalysis *ArtificialAnalysis `json:"artificial_analysis,omitempty"`
}

type ArtificialAnalysis struct {
	IntelligenceIndex float64 `json:"intelligence_index"`
	CodingIndex       float64 `json:"coding_index"`
	AgenticIndex      float64 `json:"agentic_index"`
}

type ModelResponse struct {
	Data []Model `json:"data"`
}

// EndpointPricing / EndpointPercentiles / ModelEndpoint / ModelEndpointsInfo 对应
// GET /models/{author}/{slug}/endpoints 的响应结构。
type EndpointPricing struct {
	Prompt     string `json:"prompt"`
	Completion string `json:"completion"`
	Request    string `json:"request"`
}

type EndpointPercentiles struct {
	P50 float64 `json:"p50"`
	P75 float64 `json:"p75"`
	P90 float64 `json:"p90"`
	P99 float64 `json:"p99"`
}

type ModelEndpoint struct {
	Name                  string               `json:"name"`
	ProviderName          string               `json:"provider_name"`
	Tag                   string               `json:"tag"`
	ContextLength         int                  `json:"context_length"`
	MaxCompletionTokens   int                  `json:"max_completion_tokens"`
	Quantization          string               `json:"quantization"`
	Pricing               EndpointPricing      `json:"pricing"`
	UptimeLast1d          *float64             `json:"uptime_last_1d"`
	LatencyLast30m        *EndpointPercentiles `json:"latency_last_30m"`
	ThroughputLast30m     *EndpointPercentiles `json:"throughput_last_30m"`
	SupportsImplicitCache bool                 `json:"supports_implicit_caching"`
}

type ModelEndpointsInfo struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Endpoints []ModelEndpoint `json:"endpoints"`
}

type EndpointsResponse struct {
	Data ModelEndpointsInfo `json:"data"`
}
