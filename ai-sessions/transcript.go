package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// transcriptMessage 表示一条纯净对话消息，用于喂给 AI 了解会话。
type transcriptMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// extractTranscript 从会话源文件（Path）读取，提取完整 user/assistant 文本消息。
// 剔除工具调用、工具结果、思考等一切非对话内容，按来源区分不同 JSONL 封套。
func extractTranscript(session *SessionData) ([]transcriptMessage, error) {
	file, err := os.Open(session.Path)
	if err != nil {
		return nil, newHistoryError("读取会话源文件失败：%s：%v", session.Path, err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 1024*1024), 64*1024*1024)
	var messages []transcriptMessage
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var item map[string]any
		if err := json.Unmarshal([]byte(line), &item); err != nil || item == nil {
			continue
		}
		role, content := transcriptMessageOf(session.Source, item)
		if role == "" || content == "" || isTranscriptInjected(role, content) {
			continue
		}
		if session.Source == sourceQoderApp {
			content = extractQoderAppQuestion(content)
		}
		if len(messages) > 0 {
			last := messages[len(messages)-1]
			if last.Role == role && last.Content == content {
				continue
			}
		}
		messages = append(messages, transcriptMessage{Role: role, Content: content})
	}
	if err := scanner.Err(); err != nil {
		return nil, newHistoryError("读取会话源文件失败：%s：%v", session.Path, err)
	}
	return messages, nil
}

// transcriptMessageOf 从单行 JSON 提取队伍角色与文本内容，非对话行返回空。
func transcriptMessageOf(source string, item map[string]any) (string, string) {
	if source == sourceCodex {
		if item["type"] != "response_item" {
			return "", ""
		}
		payload, ok := item["payload"].(map[string]any)
		if !ok || payload["type"] != "message" {
			return "", ""
		}
		role, _ := payload["role"].(string)
		if role != "user" && role != "assistant" {
			return "", ""
		}
		return role, transcriptBlockText(payload["content"])
	}
	role := transcriptRole(item)
	if role == "" {
		return "", ""
	}
	message, _ := item["message"].(map[string]any)
	content := transcriptBlockText(message["content"])
	if content == "" {
		content = transcriptBlockText(item["content"])
	}
	return role, content
}

// transcriptRole 提取行中的用户/助手角色，兼容三种封套：
// role 在顶层（Qoder App）、role 在 message 内（Claude 新版）、type 即角色（Claude 旧版扁平格式）。
func transcriptRole(item map[string]any) string {
	if role, ok := item["role"].(string); ok && (role == "user" || role == "assistant") {
		return role
	}
	if message, ok := item["message"].(map[string]any); ok {
		if role, ok := message["role"].(string); ok && (role == "user" || role == "assistant") {
			return role
		}
	}
	if role, ok := item["type"].(string); ok && (role == "user" || role == "assistant") {
		return role
	}
	return ""
}

// transcriptBlockText 提取消息内容块中的纯文本，忽略工具与思考块。
func transcriptBlockText(value any) string {
	var parts []string
	add := func(text string) {
		if trimmed := strings.TrimSpace(text); trimmed != "" {
			parts = append(parts, trimmed)
		}
	}
	switch typed := value.(type) {
	case string:
		add(typed)
	case []any:
		for _, block := range typed {
			switch item := block.(type) {
			case string:
				add(item)
			case map[string]any:
				blockType, _ := item["type"].(string)
				if isTranscriptIgnoredBlock(blockType) {
					continue
				}
				if text, ok := item["text"].(string); ok {
					add(text)
				}
			}
		}
	}
	return strings.Join(parts, "\n")
}

// isTranscriptInjected 判断消息是否为系统注入的上下文（AGENTS.md 指令、环境信息），
// 这些属于会话上下文而非真实对话，喂给 AI 时剔除。
func isTranscriptInjected(role, content string) bool {
	if role != "user" {
		return false
	}
	return strings.Contains(content, "<AGENTS.md>") ||
		strings.Contains(content, "<INSTRUCTIONS>") ||
		strings.Contains(content, "<environment_context>")
}

// isTranscriptIgnoredBlock 判断内容块是否属于非对话内容（工具/思考/系统提醒）。
func isTranscriptIgnoredBlock(blockType string) bool {
	if blockType == "tool_use" || blockType == "tool_result" {
		return true
	}
	lowerType := strings.ToLower(blockType)
	return strings.Contains(lowerType, "think") || strings.Contains(lowerType, "reason") ||
		strings.Contains(lowerType, "reminder")
}

// renderTranscript 渲染纯净对话文本，jsonl 或 md 格式。
func renderTranscript(messages []transcriptMessage, format string) string {
	var builder strings.Builder
	if format == formatMD {
		for _, message := range messages {
			builder.WriteString("## " + message.Role + "\n\n" + message.Content + "\n\n")
		}
		return builder.String()
	}
	for _, message := range messages {
		encoded, err := json.Marshal(message)
		if err != nil {
			continue
		}
		builder.Write(encoded)
		builder.WriteString("\n")
	}
	return builder.String()
}

// printTranscript 输出纯净对话文本。格式无效时回退 JSONL。
func printTranscript(messages []transcriptMessage, format string) {
	fmt.Print(renderTranscript(messages, format))
}
