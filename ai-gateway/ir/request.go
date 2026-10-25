package ir

import "encoding/json"

// Request 是协议无关的请求语义，Input 保持源协议中的顺序。
type Request struct {
	Model             string
	Instructions      []ContentPart
	Input             []Item
	Tools             []Tool
	ToolChoice        json.RawMessage
	Temperature       *float64
	TopP              *float64
	MaxOutputTokens   *int
	Stop              []string
	Reasoning         *ReasoningConfig
	Text              *TextConfig
	Stream            bool
	StreamOptions     map[string]any
	PreviousResponse  string
	Store             *bool
	ParallelToolCalls *bool
	PresencePenalty   *float64
	FrequencyPenalty  *float64
	ServiceTier       string
	Metadata          json.RawMessage
	Extensions        map[string]json.RawMessage
	Unknown           map[string]json.RawMessage
	// Dropped 是源协议携带、目标协议不表达、但不改变语义的调参/元数据字段名。
	Dropped []string
}

// TextConfig 描述结构化文本输出和文本呈现选项，Format 使用规范化后的格式对象。
type TextConfig struct {
	Format    json.RawMessage
	Verbosity string
}

// ReasoningConfig 保留 reasoning 的可迁移字段以及供应商扩展。
type ReasoningConfig struct {
	Effort          string
	Summary         string
	IncludeThoughts *bool
	Encrypted       json.RawMessage
	Extensions      map[string]json.RawMessage
}

// Tool 描述 function、custom、MCP 和供应商专用工具。
type Tool struct {
	Type        string
	Name        string
	Description string
	Parameters  json.RawMessage
	Strict      *bool
	ServerLabel string
	Format      json.RawMessage
	Raw         json.RawMessage
	Extensions  map[string]json.RawMessage
}

// Item 是 Open Responses 风格的有序上下文单元。
type Item struct {
	Type       string
	ID         string
	Role       string
	Status     string
	Phase      string
	Content    []ContentPart
	CallID     string
	Name       string
	Arguments  string
	Input      string
	Output     json.RawMessage
	Encrypted  string
	Position   int
	Raw        json.RawMessage
	Extensions map[string]json.RawMessage
}

// ContentPart 是消息或工具结果中的内容单元。
type ContentPart struct {
	Type        string
	Text        string
	Refusal     string
	ImageURL    string
	FileURL     string
	FileID      string
	FileData    string
	Filename    string
	Detail      string
	AudioFormat string
	Annotations []json.RawMessage
	Raw         json.RawMessage
	Extensions  map[string]json.RawMessage
}
