package gemini

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"google.golang.org/genai"
)

// Server OpenAI 兼容的 Gemini 代理服务。
type Server struct {
	client        *genai.Client
	model         string
	thinkingLevel genai.ThinkingLevel
	aliases       map[string]string
	timeout       time.Duration
}

var idCounter atomic.Uint64

// ServerOptions 配置模型选择和请求处理。
type ServerOptions struct {
	DefaultModel  string
	ThinkingLevel string
	ModelAliases  map[string]string
	Timeout       time.Duration
}

// NewServer 创建单个 route 的 Gemini HTTP handler。
func NewServer(client *genai.Client, options ServerOptions) (*Server, error) {
	level, err := thinkingLevel(options.ThinkingLevel)
	if err != nil {
		return nil, err
	}
	timeout := options.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}
	return &Server{
		client:        client,
		model:         options.DefaultModel,
		thinkingLevel: level,
		aliases:       options.ModelAliases,
		timeout:       timeout,
	}, nil
}

func thinkingLevel(value string) (genai.ThinkingLevel, error) {
	switch strings.ToLower(value) {
	case "":
		return genai.ThinkingLevelUnspecified, nil
	case "low":
		return genai.ThinkingLevelLow, nil
	case "medium":
		return genai.ThinkingLevelMedium, nil
	case "high":
		return genai.ThinkingLevelHigh, nil
	default:
		return "", fmt.Errorf("Gemini thinking_level 无效: %q", value)
	}
}

func (s *Server) resolveModel(requested string) string {
	if requested == "" {
		requested = s.model
	}
	if target, exists := s.aliases[requested]; exists && target != "" {
		return target
	}
	return requested
}

type openAIErrorResponse struct {
	Error openAIError `json:"error"`
}

type openAIError struct {
	Message string  `json:"message"`
	Type    string  `json:"type"`
	Param   *string `json:"param,omitempty"`
	Code    int     `json:"code"`
}

func writeOpenAIError(w http.ResponseWriter, status int, errType, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(openAIErrorResponse{
		Error: openAIError{
			Message: message,
			Type:    errType,
			Code:    status,
		},
	})
}

// ServeChat 将 Chat Completions 请求交给 Gemini GenerateContent。
func (s *Server) ServeChat(w http.ResponseWriter, r *http.Request, body []byte) {
	startTime := time.Now()
	ctx, cancel := context.WithTimeout(r.Context(), s.timeout)
	defer cancel()
	r = r.WithContext(ctx)
	var req chatRequest
	if err := decodeKnownRequest(body, &req, chatRequestFields); err != nil {
		log.Printf("[ai-gateway/gemini] POST /v1/chat/completions 400 Bad Request (dur=%v): 非法 JSON: %v", time.Since(startTime), err)
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", fmt.Sprintf("请求体不是合法 JSON: %v", err))
		return
	}
	if len(req.Messages) == 0 {
		log.Printf("[ai-gateway/gemini] POST /v1/chat/completions 400 Bad Request (dur=%v): messages 不能为空", time.Since(startTime))
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "messages 不能为空")
		return
	}

	model := s.resolveModel(req.Model)
	if model == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "缺少 model，且 route 未配置 defaults.model")
		return
	}

	config := &genai.GenerateContentConfig{}
	config.Temperature = req.Temperature
	config.TopP = req.TopP
	config.StopSequences = req.Stop
	system, err := extractSystemPrompt(req.Messages)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if system != "" {
		config.SystemInstruction = &genai.Content{Parts: []*genai.Part{{Text: system}}}
	}
	tools, err := toGeminiTools(req.Tools)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if tools != nil {
		config.Tools = tools
	}
	if err := applyResponseFormat(config, req.ResponseFormat); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	maxTokens := req.MaxTokens
	if maxTokens == 0 {
		maxTokens = req.MaxCompletionTokens
	}
	if maxTokens > 0 {
		config.MaxOutputTokens = int32(maxTokens)
	}

	// 思考配置：支持 includeThoughts 与 ThinkingLevel / ThinkingBudget 映射
	config.ThinkingConfig = buildThinkingConfig(model, req.ReasoningEffort, s.thinkingLevel)

	// ToolChoice 转换
	toolConfig, err := toGeminiToolConfig(req.ToolChoice)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if toolConfig != nil {
		config.ToolConfig = toolConfig
	}

	contents, err := toGeminiContents(req.Messages)
	if err != nil {
		log.Printf("[ai-gateway/gemini] POST /v1/chat/completions 400 Bad Request (dur=%v): toGeminiContents 失败: %v", time.Since(startTime), err)
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	if req.Stream {
		includeUsage := req.StreamOptions != nil && req.StreamOptions.IncludeUsage
		s.handleStream(w, r, model, contents, config, includeUsage, len(req.Messages), len(req.Tools), startTime)
		return
	}

	resp, err := s.client.Models.GenerateContent(r.Context(), model, contents, config)
	duration := time.Since(startTime)
	if err != nil {
		log.Printf("[ai-gateway/gemini] POST /v1/chat/completions 502 Bad Gateway model=%s (dur=%v): %v", model, duration, err)
		writeOpenAIError(w, http.StatusBadGateway, "api_error", fmt.Sprintf("LLM provider 请求失败: %v", err))
		return
	}

	res := fromGeminiResponse(resp, model)
	choice := res.Choices[0]
	hasContent := choice.Message.Content != nil && *choice.Message.Content != ""
	toolCallCount := len(choice.Message.ToolCalls)

	warnTag := ""
	if !hasContent && toolCallCount == 0 {
		warnTag = " WARN=empty_response"
	}

	tokensTag := formatUsageTag(res.Usage)
	log.Printf("[ai-gateway/gemini] POST /v1/chat/completions 200 OK model=%s stream=false msgs=%d tools=%d dur=%v %s finish=%s calls=%d%s",
		model, len(req.Messages), len(req.Tools), duration.Round(time.Millisecond), tokensTag, choice.FinishReason, toolCallCount, warnTag)

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(res); err != nil {
		log.Printf("[ai-gateway/gemini] 写入响应失败: %v", err)
	}
}

func formatUsageTag(u *usageInfo) string {
	if u == nil {
		return "tokens=0/0/0"
	}
	think := 0
	if u.CompletionTokensDetails != nil {
		think = u.CompletionTokensDetails.ReasoningTokens
	}
	return fmt.Sprintf("tokens=in:%d/out:%d/think:%d", u.PromptTokens, u.CompletionTokens, think)
}

func buildThinkingConfig(model string, reasoningEffort string, defaultLevel genai.ThinkingLevel) *genai.ThinkingConfig {
	effort := strings.ToLower(reasoningEffort)
	if effort == "none" || effort == "off" || effort == "disabled" || effort == "0" {
		budget := int32(0)
		return &genai.ThinkingConfig{
			IncludeThoughts: false,
			ThinkingBudget:  &budget,
		}
	}

	// 只要不是显式关闭，默认始终开启 IncludeThoughts: true，确保 Google 返回思考内容
	cfg := &genai.ThinkingConfig{
		IncludeThoughts: true,
	}

	if effort != "" && effort != "auto" {
		level := mapReasoningEffort(effort)
		if level != genai.ThinkingLevelUnspecified {
			cfg.ThinkingLevel = level
		}
	} else if defaultLevel != "" && defaultLevel != genai.ThinkingLevelUnspecified {
		cfg.ThinkingLevel = defaultLevel
	}

	return cfg
}

func mapReasoningEffort(effort string) genai.ThinkingLevel {
	switch strings.ToLower(effort) {
	case "low", "minimal":
		return genai.ThinkingLevelLow
	case "medium":
		return genai.ThinkingLevelMedium
	case "high":
		return genai.ThinkingLevelHigh
	default:
		return genai.ThinkingLevelUnspecified
	}
}

// handleStream 以 SSE 输出 OpenAI 格式的流式 chunks。
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request, model string, contents []*genai.Content, config *genai.GenerateContentConfig, includeUsage bool, msgCount, toolCount int, startTime time.Time) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		log.Printf("[ai-gateway/gemini] POST /v1/chat/completions 500 Internal Error (dur=%v): 连接不支持流式输出", time.Since(startTime))
		http.Error(w, "当前连接不支持流式输出", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	stream := s.client.Models.GenerateContentStream(r.Context(), model, contents, config)
	requestID := requestID()
	createdAt := unixNow()
	toolCallState := &toolCallStreamState{}
	var lastUsage *usageInfo
	var chunkCount int
	var hasContent bool
	var hasToolCalls bool
	var finishReason string
	var ttft time.Duration

	for resp, err := range stream {
		if err != nil {
			isCanceled := errors.Is(err, context.Canceled) ||
				errors.Is(r.Context().Err(), context.Canceled) ||
				strings.Contains(err.Error(), "context canceled")

			duration := time.Since(startTime).Round(time.Millisecond)
			if isCanceled {
				log.Printf("[ai-gateway/gemini] POST /v1/chat/completions 499 CANCELED model=%s stream=true msgs=%d tools=%d dur=%v chunks=%d (client canceled)",
					model, msgCount, toolCount, duration, chunkCount)
				return
			}

			log.Printf("[ai-gateway/gemini] POST /v1/chat/completions 502 Bad Gateway model=%s stream=true msgs=%d tools=%d dur=%v chunks=%d: %v",
				model, msgCount, toolCount, duration, chunkCount, err)
			_ = writeSSE(w, map[string]any{"error": map[string]string{
				"message": "Gemini 流式请求失败: " + err.Error(),
				"type":    "provider_error",
			}})
			flusher.Flush()
			return
		}

		chunkCount++
		if chunkCount == 1 {
			ttft = time.Since(startTime).Round(time.Millisecond)
		}

		chunk := toOpenAIChunk(resp, model, requestID, createdAt, toolCallState)
		if chunk.Usage != nil {
			lastUsage = chunk.Usage
		}
		if len(chunk.Choices) > 0 {
			delta := chunk.Choices[0].Delta
			if delta.Content != nil && *delta.Content != "" {
				hasContent = true
			}
			if len(delta.ToolCalls) > 0 {
				hasToolCalls = true
			}
			if chunk.Choices[0].FinishReason != nil {
				finishReason = *chunk.Choices[0].FinishReason
			}
		}

		if err := writeSSE(w, chunk); err != nil {
			return
		}
		flusher.Flush()
	}

	totalDuration := time.Since(startTime).Round(time.Millisecond)
	warnTag := ""
	if !hasContent && !hasToolCalls {
		warnTag = " WARN=empty_response"
	}

	tokensTag := formatUsageTag(lastUsage)
	if finishReason == "" {
		finishReason = "stop"
	}

	log.Printf("[ai-gateway/gemini] POST /v1/chat/completions 200 OK model=%s stream=true msgs=%d tools=%d ttft=%v dur=%v %s finish=%s chunks=%d%s",
		model, msgCount, toolCount, ttft, totalDuration, tokensTag, finishReason, chunkCount, warnTag)

	if includeUsage && lastUsage != nil {
		usageChunk := &chatChunk{
			ID:      requestID,
			Object:  "chat.completion.chunk",
			Created: createdAt,
			Model:   model,
			Choices: []chunkChoice{},
			Usage:   lastUsage,
		}
		_ = writeSSE(w, usageChunk)
		flusher.Flush()
	}

	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}

// ServeModels 列出该 route 可用的模型名和别名。
func (s *Server) ServeModels(w http.ResponseWriter) {
	allModels := []string{}
	seen := map[string]bool{}
	if s.model != "" {
		allModels = append(allModels, s.model)
		seen[s.model] = true
	}
	for alias := range s.aliases {
		if !seen[alias] {
			seen[alias] = true
			allModels = append(allModels, alias)
		}
	}
	sort.Strings(allModels)

	dataList := make([]map[string]any, 0, len(allModels))
	modelsList := make([]map[string]any, 0, len(allModels))

	for _, m := range allModels {
		dataList = append(dataList, map[string]any{
			"id":       m,
			"object":   "model",
			"owned_by": "google",
		})
		modelsList = append(modelsList, map[string]any{
			"slug":         m,
			"display_name": m,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"object": "list",
		"data":   dataList,
		"models": modelsList,
	})
}

func writeSSE(w http.ResponseWriter, data any) error {
	b, err := json.Marshal(data)
	if err != nil {
		return err
	}
	if event, ok := data.(map[string]any); ok {
		if eventType, ok := event["type"].(string); ok && strings.HasPrefix(eventType, "response.") {
			if _, err := fmt.Fprintf(w, "event: %s\n", eventType); err != nil {
				return err
			}
		}
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", b)
	return err
}

func requestID() string {
	return newID("chatcmpl-")
}

func newID(prefix string) string {
	return fmt.Sprintf("%s%d-%d", prefix, time.Now().UnixNano(), idCounter.Add(1))
}

func unixNow() int64 {
	return time.Now().Unix()
}
