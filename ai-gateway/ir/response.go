package ir

import "encoding/json"

// Response 是以有序 output item 为核心的响应 IR。
type Response struct {
	ID                 string
	Object             string
	CreatedAt          int64
	Model              string
	Status             string
	Output             []Item
	Usage              *Usage
	IncompleteDetails  json.RawMessage
	Error              json.RawMessage
	ServiceTier        string
	SystemFingerprint  string
	PreviousResponseID string
	Raw                json.RawMessage
	Unknown            map[string]json.RawMessage
}

// Usage 统一 token 统计，Details 保留供应商的细分结构。
type Usage struct {
	InputTokens       int
	OutputTokens      int
	TotalTokens       int
	ReasoningTokens   int
	CachedInputTokens int
	Details           map[string]json.RawMessage
}
