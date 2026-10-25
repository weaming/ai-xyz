package gemini

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"google.golang.org/genai"
)

type responsesRequest struct {
	Model           string              `json:"model"`
	Instructions    string              `json:"instructions"`
	Input           responseInput       `json:"input"`
	Tools           []openaiTool        `json:"tools"`
	ToolChoice      any                 `json:"tool_choice"`
	Reasoning       *responsesReasoning `json:"reasoning,omitempty"`
	ReasoningEffort string              `json:"reasoning_effort,omitempty"`
	Temperature     *float32            `json:"temperature,omitempty"`
	TopP            *float32            `json:"top_p,omitempty"`
	MaxOutputTokens int                 `json:"max_output_tokens,omitempty"`
	Stream          bool                `json:"stream"`
}

type responsesReasoning struct {
	Effort  string `json:"effort,omitempty"`
	Summary string `json:"summary,omitempty"`
}

type responseItem struct {
	Type             string `json:"type"`
	Role             string `json:"role,omitempty"`
	Content          any    `json:"content,omitempty"`
	ID               string `json:"id,omitempty"`
	CallID           string `json:"call_id,omitempty"`
	Name             string `json:"name,omitempty"`
	Arguments        string `json:"arguments,omitempty"`
	ThoughtSignature string `json:"thought_signature,omitempty"`
	Output           any    `json:"output,omitempty"`
}

type responseInput []responseItem

func (input *responseInput) UnmarshalJSON(raw []byte) error {
	if string(bytes.TrimSpace(raw)) == "null" {
		return fmt.Errorf("Responses input 不能是 null")
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		*input = responseInput{{Type: "message", Role: "user", Content: text}}
		return nil
	}
	var items []responseItem
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&items); err != nil {
		return fmt.Errorf("Responses input 必须是字符串或 item 数组: %w", err)
	}
	*input = items
	return nil
}

// ServeResponses 将 Responses 请求交给 Gemini GenerateContent。
func (s *Server) ServeResponses(w http.ResponseWriter, r *http.Request, body []byte) {
	startTime := time.Now()
	ctx, cancel := context.WithTimeout(r.Context(), s.timeout)
	defer cancel()
	r = r.WithContext(ctx)
	var req responsesRequest
	if err := decodeKnownRequest(body, &req, responsesRequestFields); err != nil {
		log.Printf("[ai-gateway/gemini] POST /v1/responses 400 Bad Request (dur=%v): 非法 JSON: %v", time.Since(startTime), err)
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", fmt.Sprintf("请求体不是合法 JSON: %v", err))
		return
	}
	if len(req.Input) == 0 {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "Responses input 不能为空")
		return
	}
	if req.Reasoning != nil && req.Reasoning.Summary != "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "Responses reasoning.summary 暂不支持")
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
	instructions := []string{}
	if req.Instructions != "" {
		instructions = append(instructions, req.Instructions)
	}
	for _, item := range req.Input {
		if item.Role == "system" || item.Role == "developer" {
			text, _, err := extractMessageText(item.Content)
			if err != nil {
				writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
				return
			}
			if text != "" {
				instructions = append(instructions, text)
			}
		}
	}
	if len(instructions) > 0 {
		config.SystemInstruction = &genai.Content{Parts: []*genai.Part{{Text: strings.Join(instructions, "\n\n")}}}
	}
	tools, err := toGeminiTools(req.Tools)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if tools != nil {
		config.Tools = tools
	}
	if req.MaxOutputTokens > 0 {
		config.MaxOutputTokens = int32(req.MaxOutputTokens)
	}

	effort := req.ReasoningEffort
	if effort == "" && req.Reasoning != nil {
		effort = req.Reasoning.Effort
	}
	config.ThinkingConfig = buildThinkingConfig(model, effort, s.thinkingLevel)

	toolConfig, err := toGeminiToolConfig(req.ToolChoice)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if toolConfig != nil {
		config.ToolConfig = toolConfig
	}

	contents, err := toGeminiContentsFromResponsesInput(req.Input)
	if err != nil {
		log.Printf("[ai-gateway/gemini] POST /v1/responses 400 Bad Request (dur=%v): input 转换失败: %v", time.Since(startTime), err)
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	respID := newID("resp_")

	if req.Stream {
		s.handleResponsesStream(w, r, model, respID, contents, config, len(req.Input), len(req.Tools), startTime)
		return
	}

	resp, err := s.client.Models.GenerateContent(r.Context(), model, contents, config)
	duration := time.Since(startTime)
	if err != nil {
		log.Printf("[ai-gateway/gemini] POST /v1/responses 502 Bad Gateway model=%s (dur=%v): %v", model, duration, err)
		writeOpenAIError(w, http.StatusBadGateway, "api_error", fmt.Sprintf("LLM provider 请求失败: %v", err))
		return
	}

	usage := toUsageInfo(resp.UsageMetadata)
	tokensTag := formatUsageTag(usage)
	log.Printf("[ai-gateway/gemini] POST /v1/responses 200 OK model=%s stream=false input=%d tools=%d dur=%v %s",
		model, len(req.Input), len(req.Tools), duration.Round(time.Millisecond), tokensTag)

	outItems := make([]map[string]any, 0)
	candidate := firstCandidate(resp)
	if candidate != nil && candidate.Content != nil {
		msgText, toolCalls := partsToMessage(candidate.Content.Parts)
		if msgText.Content != nil && *msgText.Content != "" {
			outItems = append(outItems, map[string]any{
				"type": "message",
				"role": "assistant",
				"content": []map[string]any{
					{"type": "output_text", "text": *msgText.Content},
				},
			})
		}
		for _, tc := range toolCalls {
			toolItem := map[string]any{
				"type":      "function_call",
				"call_id":   tc.ID,
				"name":      tc.Function.Name,
				"arguments": tc.Function.Arguments,
			}
			if tc.ThoughtSignature != "" {
				toolItem["thought_signature"] = tc.ThoughtSignature
			}
			outItems = append(outItems, toolItem)
		}
	}

	res := map[string]any{
		"id":         respID,
		"object":     "response",
		"created_at": unixNow(),
		"model":      modelVersion(resp, model),
		"output":     outItems,
		"usage":      toResponsesUsage(usage),
	}
	finishReason := genai.FinishReasonUnspecified
	if candidate != nil {
		finishReason = candidate.FinishReason
	}
	status, incompleteDetails := responsesStatusFromGeminiFinish(finishReason)
	if isPromptBlocked(resp) {
		status, incompleteDetails = "incomplete", map[string]any{"reason": "content_filter"}
	}
	res["status"] = status
	if incompleteDetails != nil {
		res["incomplete_details"] = incompleteDetails
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(res); err != nil {
		log.Printf("[ai-gateway/gemini] 写入响应失败: %v", err)
	}
}

func (s *Server) handleResponsesStream(
	w http.ResponseWriter,
	r *http.Request,
	model string,
	respID string,
	contents []*genai.Content,
	config *genai.GenerateContentConfig,
	inputCount int,
	toolCount int,
	startTime time.Time,
) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		log.Printf("[ai-gateway/gemini] POST /v1/responses 500 Internal Error (dur=%v): 连接不支持流式输出", time.Since(startTime))
		http.Error(w, "当前连接不支持流式输出", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	_ = writeSSE(w, map[string]any{
		"type": "response.created",
		"response": map[string]any{
			"id": respID,
		},
	})
	flusher.Flush()

	stream := s.client.Models.GenerateContentStream(r.Context(), model, contents, config)
	var lastUsage *usageInfo
	var chunkCount int
	var hasContent bool
	var hasToolCalls bool
	var ttft time.Duration
	lastFinishReason := genai.FinishReasonUnspecified
	isBlocked := false

	reasoningActive := false
	reasoningID := newID("reasoning_")
	var reasoningBuilder strings.Builder

	messageActive := false
	messageID := newID("msg_")
	var messageBuilder strings.Builder

	for resp, err := range stream {
		if err != nil {
			isCanceled := errors.Is(err, context.Canceled) ||
				errors.Is(r.Context().Err(), context.Canceled) ||
				strings.Contains(err.Error(), "context canceled")

			duration := time.Since(startTime).Round(time.Millisecond)
			if isCanceled {
				log.Printf("[ai-gateway/gemini] POST /v1/responses 499 CANCELED model=%s stream=true input=%d tools=%d dur=%v chunks=%d (client canceled)",
					model, inputCount, toolCount, duration, chunkCount)
				return
			}

			log.Printf("[ai-gateway/gemini] POST /v1/responses 502 Bad Gateway model=%s stream=true input=%d tools=%d dur=%v chunks=%d: %v",
				model, inputCount, toolCount, duration, chunkCount, err)
			_ = writeSSE(w, map[string]any{
				"type": "response.failed",
				"response": map[string]any{
					"id": respID,
					"error": map[string]any{
						"code":    "api_error",
						"message": "Gemini 流式请求失败: " + err.Error(),
					},
				},
			})
			flusher.Flush()
			return
		}

		chunkCount++
		if chunkCount == 1 {
			ttft = time.Since(startTime).Round(time.Millisecond)
		}

		if u := toUsageInfo(resp.UsageMetadata); u != nil {
			lastUsage = u
		}

		candidate := firstCandidate(resp)
		if isPromptBlocked(resp) {
			isBlocked = true
		}
		if candidate != nil && candidate.FinishReason != genai.FinishReasonUnspecified {
			lastFinishReason = candidate.FinishReason
		}
		if candidate != nil && candidate.Content != nil {
			for _, part := range candidate.Content.Parts {
				if part == nil {
					continue
				}

				if part.Thought && part.Text != "" {
					if !reasoningActive {
						reasoningActive = true
						_ = writeSSE(w, map[string]any{
							"type": "response.output_item.added",
							"item": map[string]any{
								"type":    "reasoning",
								"id":      reasoningID,
								"summary": []any{},
							},
						})
						flusher.Flush()
					}
					reasoningBuilder.WriteString(part.Text)
					_ = writeSSE(w, map[string]any{
						"type":          "response.reasoning_text.delta",
						"delta":         part.Text,
						"content_index": 0,
					})
					flusher.Flush()
				} else if part.Text != "" {
					if reasoningActive {
						reasoningActive = false
						_ = writeSSE(w, map[string]any{
							"type": "response.output_item.done",
							"item": map[string]any{
								"type":    "reasoning",
								"id":      reasoningID,
								"summary": []any{},
							},
						})
						flusher.Flush()
					}

					if !messageActive {
						messageActive = true
						_ = writeSSE(w, map[string]any{
							"type": "response.output_item.added",
							"item": map[string]any{
								"type":    "message",
								"role":    "assistant",
								"id":      messageID,
								"content": []any{},
							},
						})
						flusher.Flush()
					}

					hasContent = true
					messageBuilder.WriteString(part.Text)
					_ = writeSSE(w, map[string]any{
						"type":  "response.output_text.delta",
						"delta": part.Text,
					})
					flusher.Flush()
				} else if part.FunctionCall != nil {
					if reasoningActive {
						reasoningActive = false
						_ = writeSSE(w, map[string]any{
							"type": "response.output_item.done",
							"item": map[string]any{
								"type":    "reasoning",
								"id":      reasoningID,
								"summary": []any{},
							},
						})
						flusher.Flush()
					}
					if messageActive {
						messageActive = false
						_ = writeSSE(w, map[string]any{
							"type": "response.output_item.done",
							"item": map[string]any{
								"type": "message",
								"role": "assistant",
								"id":   messageID,
								"content": []map[string]any{
									{"type": "output_text", "text": messageBuilder.String()},
								},
							},
						})
						flusher.Flush()
					}

					hasToolCalls = true
					call := part.FunctionCall
					argsJSON, err := json.Marshal(call.Args)
					if err != nil {
						argsJSON = []byte("{}")
					}
					callID := call.ID
					if callID == "" {
						callID = newID("call_")
					}
					toolItem := map[string]any{
						"type":      "function_call",
						"call_id":   normalizeToolCallID(callID),
						"name":      call.Name,
						"arguments": string(argsJSON),
					}
					if signature := encodeThoughtSignature(part.ThoughtSignature); signature != "" {
						toolItem["thought_signature"] = signature
					}
					_ = writeSSE(w, map[string]any{
						"type": "response.output_item.done",
						"item": toolItem,
					})
					flusher.Flush()
				}
			}
		}
	}

	if reasoningActive {
		_ = writeSSE(w, map[string]any{
			"type": "response.output_item.done",
			"item": map[string]any{
				"type":    "reasoning",
				"id":      reasoningID,
				"summary": []any{},
			},
		})
		flusher.Flush()
	}

	if messageActive {
		_ = writeSSE(w, map[string]any{
			"type": "response.output_item.done",
			"item": map[string]any{
				"type": "message",
				"role": "assistant",
				"id":   messageID,
				"content": []map[string]any{
					{"type": "output_text", "text": messageBuilder.String()},
				},
			},
		})
		flusher.Flush()
	}

	totalDuration := time.Since(startTime).Round(time.Millisecond)
	tokensTag := formatUsageTag(lastUsage)
	warnTag := ""
	if !hasContent && !hasToolCalls {
		warnTag = " WARN=empty_response"
	}

	log.Printf("[ai-gateway/gemini] POST /v1/responses 200 OK model=%s stream=true input=%d tools=%d ttft=%v dur=%v %s chunks=%d%s",
		model, inputCount, toolCount, ttft, totalDuration, tokensTag, chunkCount, warnTag)

	status, incompleteDetails := responsesStatusFromGeminiFinish(lastFinishReason)
	if isBlocked {
		status, incompleteDetails = "incomplete", map[string]any{"reason": "content_filter"}
	}
	finalResponse := map[string]any{"id": respID, "status": status, "usage": toResponsesUsage(lastUsage)}
	if incompleteDetails != nil {
		finalResponse["incomplete_details"] = incompleteDetails
	}
	_ = writeSSE(w, map[string]any{"type": "response." + status, "response": finalResponse})
	flusher.Flush()

	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}

func responsesStatusFromGeminiFinish(reason genai.FinishReason) (string, map[string]any) {
	switch mapFinishReason(reason, false) {
	case "length":
		return "incomplete", map[string]any{"reason": "max_output_tokens"}
	case "content_filter":
		return "incomplete", map[string]any{"reason": "content_filter"}
	default:
		return "completed", nil
	}
}

func toResponsesUsage(u *usageInfo) map[string]any {
	if u == nil {
		return map[string]any{
			"input_tokens":  0,
			"output_tokens": 0,
			"total_tokens":  0,
		}
	}
	res := map[string]any{
		"input_tokens":  u.PromptTokens,
		"output_tokens": u.CompletionTokens,
		"total_tokens":  u.TotalTokens,
	}
	if u.PromptTokensDetails != nil && u.PromptTokensDetails.CachedTokens > 0 {
		res["input_tokens_details"] = map[string]any{
			"cached_tokens": u.PromptTokensDetails.CachedTokens,
		}
	}
	if u.CompletionTokensDetails != nil && u.CompletionTokensDetails.ReasoningTokens > 0 {
		res["output_tokens_details"] = map[string]any{
			"reasoning_tokens": u.CompletionTokensDetails.ReasoningTokens,
		}
	}
	return res
}

func toGeminiContentsFromResponsesInput(input []responseItem) ([]*genai.Content, error) {
	contents := make([]*genai.Content, 0, len(input))
	callIDToName := make(map[string]string)

	for _, item := range input {
		itemType := item.Type
		if itemType == "" && item.Role != "" {
			itemType = "message"
		}
		switch itemType {
		case "message":
			switch item.Role {
			case "system", "developer":
				continue
			case "assistant":
				text, _, err := extractMessageText(item.Content)
				if err != nil {
					return nil, fmt.Errorf("Responses assistant 消息 content: %w", err)
				}
				if text != "" {
					contents = append(contents, &genai.Content{
						Role:  genai.RoleModel,
						Parts: []*genai.Part{{Text: text}},
					})
				}
			case "user":
				parts, err := extractUserParts(item.Content)
				if err != nil {
					return nil, fmt.Errorf("Responses user 消息 content: %w", err)
				}
				if len(parts) > 0 {
					contents = append(contents, &genai.Content{
						Role:  genai.RoleUser,
						Parts: parts,
					})
				}
			default:
				return nil, fmt.Errorf("不支持 Responses 消息 role %q", item.Role)
			}
		case "function_call":
			if item.CallID != "" && item.Name != "" {
				callIDToName[item.CallID] = item.Name
			}
			args := map[string]any{}
			if item.Arguments != "" {
				if err := json.Unmarshal([]byte(item.Arguments), &args); err != nil {
					return nil, fmt.Errorf("function_call %q 的 arguments 不是合法 JSON: %w", item.Name, err)
				}
			}
			call := &genai.FunctionCall{
				Name: item.Name,
				Args: args,
			}
			if item.CallID != "" {
				call.ID = normalizeToolCallID(item.CallID)
			}
			signature, err := decodeThoughtSignature(item.ThoughtSignature)
			if err != nil {
				return nil, fmt.Errorf("function_call %q: %w", item.CallID, err)
			}
			part := &genai.Part{FunctionCall: call, ThoughtSignature: signature}

			last := lastContent(contents)
			if last != nil && last.Role == genai.RoleModel {
				last.Parts = append(last.Parts, part)
			} else {
				contents = append(contents, &genai.Content{
					Role:  genai.RoleModel,
					Parts: []*genai.Part{part},
				})
			}
		case "function_call_output":
			name := item.Name
			if name == "" && item.CallID != "" {
				name = callIDToName[item.CallID]
			}
			if name == "" {
				return nil, fmt.Errorf("function_call_output 的 call_id %q 找不到对应 function name", item.CallID)
			}
			outputStr := extractOutputString(item.Output)
			part := &genai.Part{
				FunctionResponse: &genai.FunctionResponse{
					Name:     name,
					Response: toolOutputToResponse(outputStr),
				},
			}
			if item.CallID != "" {
				part.FunctionResponse.ID = normalizeToolCallID(item.CallID)
			}

			last := lastContent(contents)
			if last != nil && last.Role == genai.RoleUser && hasFunctionResponse(last) {
				last.Parts = append(last.Parts, part)
			} else {
				contents = append(contents, &genai.Content{
					Role:  genai.RoleUser,
					Parts: []*genai.Part{part},
				})
			}
		case "reasoning":
			// skip reasoning historical events
		default:
			return nil, fmt.Errorf("不支持 Responses input item.type %q", itemType)
		}
	}
	return contents, nil
}

func extractOutputString(output any) string {
	if output == nil {
		return ""
	}
	switch v := output.(type) {
	case string:
		return v
	case []byte:
		return string(v)
	case map[string]any:
		if c, ok := v["content"].(string); ok {
			return c
		}
		b, err := json.Marshal(v)
		if err == nil {
			return string(b)
		}
		return fmt.Sprint(v)
	default:
		b, err := json.Marshal(v)
		if err == nil {
			return string(b)
		}
		return fmt.Sprint(v)
	}
}
