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

	"github.com/weaming/ai-xyz/ai-gateway/convert"
	"github.com/weaming/ai-xyz/ai-gateway/ir"
	"google.golang.org/genai"
)

// Server 把入口协议转换成 Gemini GenerateContent 并调用 SDK。
type Server struct {
	client        *genai.Client
	model         string
	thinkingLevel genai.ThinkingLevel
	aliases       map[string]string
	timeout       time.Duration
	converter     *Converter
}

var idCounter atomic.Uint64

// ServerOptions 配置模型选择和请求处理。
type ServerOptions struct {
	DefaultModel  string
	ThinkingLevel string
	ModelAliases  map[string]string
	Timeout       time.Duration
	Provider      ir.Provider
}

// NewServer 创建单个 route 的 Gemini HTTP handler。
func NewServer(client *genai.Client, options ServerOptions) (*Server, error) {
	level, err := thinkingLevel(options.ThinkingLevel)
	if err != nil {
		return nil, err
	}
	provider := options.Provider
	if provider == "" {
		provider = ir.ProviderGoogle
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
		converter:     NewConverter(provider, level),
	}, nil
}

// Converter 暴露该 route 的转换器，便于网关与测试复用。
func (s *Server) Converter() *Converter {
	return s.converter
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

// Serve 把入口协议请求转换成 Gemini GenerateContent 请求并输出入口协议响应。
func (s *Server) Serve(w http.ResponseWriter, r *http.Request, body []byte, incoming convert.Protocol) {
	startTime := time.Now()
	ctx, cancel := context.WithTimeout(r.Context(), s.timeout)
	defer cancel()
	r = r.WithContext(ctx)

	options := convert.Options{DefaultModel: s.model}
	request, err := s.converter.ParseRequest(incoming, body, options)
	if err != nil {
		log.Printf("[ai-gateway/gemini] %s 400 Bad Request (dur=%v): %v", incoming, time.Since(startTime), err)
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	model := s.resolveModel(request.Model)
	if model == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "缺少 model，且 route 未配置 defaults.model")
		return
	}
	request.Model = model
	options.IncludeUsage = includeUsage(request)

	native, err := encodeRequest(request, s.thinkingLevel)
	if err != nil {
		log.Printf("[ai-gateway/gemini] %s 400 Bad Request (dur=%v): %v", incoming, time.Since(startTime), err)
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	config := native.GenerationConfig
	config.SystemInstruction = native.SystemInstruction
	config.Tools = native.Tools
	config.ToolConfig = native.ToolConfig

	if request.Stream {
		s.serveStream(w, r, incoming, model, native.Contents, config, options, droppedTag(request), startTime)
		return
	}
	response, err := s.client.Models.GenerateContent(r.Context(), model, native.Contents, config)
	duration := time.Since(startTime)
	if err != nil {
		log.Printf("[ai-gateway/gemini] %s 502 Bad Gateway model=%s (dur=%v): %v", incoming, model, duration, err)
		writeOpenAIError(w, http.StatusBadGateway, "api_error", fmt.Sprintf("LLM provider 请求失败: %v", err))
		return
	}
	decoded := decodeResponse(response, model)
	decoded.Model = modelVersion(response, model)
	output, err := s.converter.WriteResponse(incoming, decoded, options)
	if err != nil {
		log.Printf("[ai-gateway/gemini] %s 502 Bad Gateway model=%s (dur=%v): 转换响应失败: %v", incoming, model, duration, err)
		writeOpenAIError(w, http.StatusBadGateway, "conversion_error", err.Error())
		return
	}
	log.Printf("[ai-gateway/gemini] %s 200 OK model=%s stream=false items=%d tools=%d dur=%v %s%s",
		incoming, model, len(decoded.Output), len(request.Tools), duration.Round(time.Millisecond), formatUsageTag(decoded.Usage), droppedTag(request))
	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write(output); err != nil {
		log.Printf("[ai-gateway/gemini] 写入响应失败: %v", err)
	}
}

func (s *Server) serveStream(w http.ResponseWriter, r *http.Request, incoming convert.Protocol, model string, contents []*genai.Content, config *genai.GenerateContentConfig, options convert.Options, dropped string, startTime time.Time) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		log.Printf("[ai-gateway/gemini] %s 500 Internal Error (dur=%v): 连接不支持流式输出", incoming, time.Since(startTime))
		http.Error(w, "当前连接不支持流式输出", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher.Flush()

	streamWriter, err := s.converter.NewStreamWriter(incoming, w, options)
	if err != nil {
		log.Printf("[ai-gateway/gemini] %s 500 Internal Error: %v", incoming, err)
		return
	}
	state := newChunkState()
	emit := func(events []ir.StreamEvent) error {
		for _, event := range events {
			if err := streamWriter.Write(event); err != nil {
				return err
			}
		}
		return nil
	}

	chunks := 0
	var ttft time.Duration
	for response, err := range s.client.Models.GenerateContentStream(r.Context(), model, contents, config) {
		if err != nil {
			duration := time.Since(startTime).Round(time.Millisecond)
			if isCanceled(r, err) {
				log.Printf("[ai-gateway/gemini] %s 499 CANCELED model=%s stream=true dur=%v chunks=%d (client canceled)", incoming, model, duration, chunks)
				return
			}
			log.Printf("[ai-gateway/gemini] %s 502 Bad Gateway model=%s stream=true dur=%v chunks=%d: %v", incoming, model, duration, chunks, err)
			writeOpenAIError(w, http.StatusBadGateway, "api_error", fmt.Sprintf("Gemini 流式请求失败: %v", err))
			return
		}
		chunks++
		if chunks == 1 {
			ttft = time.Since(startTime).Round(time.Millisecond)
		}
		if err := emit(state.events(response, model)); err != nil {
			log.Printf("[ai-gateway/gemini] %s 写流事件失败: %v", incoming, err)
			return
		}
		flusher.Flush()
	}
	if chunks == 0 {
		log.Printf("[ai-gateway/gemini] %s 502 Bad Gateway model=%s stream=true dur=%v: 上游没有返回任何 chunk", incoming, model, time.Since(startTime).Round(time.Millisecond))
		writeOpenAIError(w, http.StatusBadGateway, "api_error", "Gemini 流式请求没有返回任何数据")
		return
	}
	if err := emit(state.finishEvents()); err != nil {
		log.Printf("[ai-gateway/gemini] %s 写终止事件失败: %v", incoming, err)
		return
	}
	response, err := streamWriter.Close()
	if err != nil {
		log.Printf("[ai-gateway/gemini] %s 关闭流失败: %v", incoming, err)
		return
	}
	flusher.Flush()
	log.Printf("[ai-gateway/gemini] %s 200 OK model=%s stream=true ttft=%v dur=%v %s finish=%s chunks=%d items=%d%s",
		incoming, model, ttft, time.Since(startTime).Round(time.Millisecond), formatUsageTag(response.Usage), response.FinishReason, chunks, len(response.Output), dropped)
}

func isCanceled(r *http.Request, err error) bool {
	return errors.Is(err, context.Canceled) ||
		errors.Is(r.Context().Err(), context.Canceled) ||
		strings.Contains(err.Error(), "context canceled")
}

func includeUsage(request *ir.Request) bool {
	if request.StreamOptions == nil {
		return false
	}
	value, _ := request.StreamOptions["include_usage"].(bool)
	return value
}

// droppedTag 把目标协议忽略的调参/元数据字段写进日志，便于发现客户端差异。
func droppedTag(request *ir.Request) string {
	if len(request.Dropped) == 0 {
		return ""
	}
	return " dropped=" + strings.Join(request.Dropped, ",")
}

func formatUsageTag(usage *ir.Usage) string {
	if usage == nil {
		return "tokens=0/0/0"
	}
	return fmt.Sprintf("tokens=in:%d/out:%d/think:%d", usage.InputTokens, usage.OutputTokens, usage.ReasoningTokens)
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
	for _, model := range allModels {
		dataList = append(dataList, map[string]any{"id": model, "object": "model", "owned_by": "google"})
		modelsList = append(modelsList, map[string]any{"slug": model, "display_name": model})
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"object": "list",
		"data":   dataList,
		"models": modelsList,
	})
}

func newID(prefix string) string {
	return fmt.Sprintf("%s%d-%d", prefix, time.Now().UnixNano(), idCounter.Add(1))
}
