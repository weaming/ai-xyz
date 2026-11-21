package main

import (
	"bufio"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// agy（Antigravity CLI）会话数据在 ~/.gemini/antigravity-cli：
// 会话记录位于 brain/<conversation_id>/.system_generated/logs/transcript_full.jsonl
// （缺失时回退 transcript.jsonl），每行一条事件（USER_INPUT/PLANNER_RESPONSE/GENERIC/CHECKPOINT 等）；
// conversation_summaries.db 存工作目录（workspace_uris）和父子会话关系（parent_conversation_id）。

const (
	agyUserRequestOpen  = "<USER_REQUEST>"
	agyUserRequestClose = "</USER_REQUEST>"
)

// agySessionMeta 会话摘要库中的工作目录与子代理标记。
type agySessionMeta struct {
	workingDir string
	isSubagent bool
}

// loadAgySessionMetaMap 从 conversation_summaries.db 批量读取会话元信息，库缺失时返回空表。
func loadAgySessionMetaMap(agyDir string) map[string]agySessionMeta {
	result := make(map[string]agySessionMeta)
	dbPath := filepath.Join(agyDir, "conversation_summaries.db")
	db, err := connectReadonly(dbPath, "agy")
	if err != nil {
		return result
	}
	defer db.Close()

	rows, err := db.Query("SELECT conversation_id, workspace_uris, parent_conversation_id FROM conversation_summaries")
	if err != nil {
		return result
	}
	defer rows.Close()

	for rows.Next() {
		var conversationID, workspaceURIs, parentID string
		if err := rows.Scan(&conversationID, &workspaceURIs, &parentID); err != nil {
			continue
		}
		result[conversationID] = agySessionMeta{
			workingDir: parseAgyWorkspaceURI(workspaceURIs),
			isSubagent: strings.TrimSpace(parentID) != "",
		}
	}
	return result
}

// loadAgySessionMeta 读取单个会话的元信息，库缺失或无记录时返回零值。
func loadAgySessionMeta(agyDir, sessionID string) agySessionMeta {
	dbPath := filepath.Join(agyDir, "conversation_summaries.db")
	db, err := connectReadonly(dbPath, "agy")
	if err != nil {
		return agySessionMeta{}
	}
	defer db.Close()

	var workspaceURIs, parentID string
	if err := db.QueryRow(
		"SELECT workspace_uris, parent_conversation_id FROM conversation_summaries WHERE conversation_id = ?",
		sessionID,
	).Scan(&workspaceURIs, &parentID); err != nil {
		return agySessionMeta{}
	}
	return agySessionMeta{
		workingDir: parseAgyWorkspaceURI(workspaceURIs),
		isSubagent: strings.TrimSpace(parentID) != "",
	}
}

// parseAgyWorkspaceURI 从 JSON 数组 ["file:///path/to/dir"] 提取首个本地目录路径。
func parseAgyWorkspaceURI(raw string) string {
	var uris []string
	if err := json.Unmarshal([]byte(raw), &uris); err != nil || len(uris) == 0 {
		return ""
	}
	first := strings.TrimSpace(uris[0])
	if !strings.HasPrefix(first, "file://") {
		return first
	}
	parsed, err := url.Parse(first)
	if err != nil || parsed.Path == "" {
		return strings.TrimPrefix(first, "file://")
	}
	return parsed.Path
}

// agyTranscriptPath 返回脑目录内可用的 transcript 文件路径，优先 transcript_full.jsonl。
func agyTranscriptPath(brainSessionDir string) string {
	logsDir := filepath.Join(brainSessionDir, ".system_generated", "logs")
	for _, name := range []string{"transcript_full.jsonl", "transcript.jsonl"} {
		candidate := filepath.Join(logsDir, name)
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() {
			return candidate
		}
	}
	return ""
}

// isAgyTranscriptPath 判断路径是否为 agy 会话 transcript 文件。
func isAgyTranscriptPath(path, agyDir string) bool {
	expanded := expandUser(path)
	info, err := os.Stat(expanded)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	cleaned := filepath.Clean(expanded)
	base := filepath.Base(cleaned)
	if base != "transcript_full.jsonl" && base != "transcript.jsonl" {
		return false
	}
	if strings.Contains(cleaned, filepath.Join(".system_generated", "logs")) {
		return true
	}
	if agyDir != "" {
		cleanedDir := filepath.Clean(expandUser(agyDir))
		if strings.HasPrefix(cleaned, cleanedDir+string(os.PathSeparator)) {
			return true
		}
	}
	return false
}

// agySessionIDFromPath 从 transcript 文件路径反推会话 ID。
func agySessionIDFromPath(path string) string {
	cleaned := filepath.Clean(path)
	logsDir := filepath.Dir(cleaned)
	sysDir := filepath.Dir(logsDir)
	if filepath.Base(logsDir) == "logs" && filepath.Base(sysDir) == ".system_generated" {
		if sessionID := filepath.Base(filepath.Dir(sysDir)); sessionID != "" && sessionID != "." {
			return sessionID
		}
	}
	return strings.TrimSuffix(filepath.Base(cleaned), filepath.Ext(cleaned))
}

// agySessionCandidate 表示一个匹配到的 agy 会话。
type agySessionCandidate struct {
	sessionID string
	path      string
}

// agySessionCandidates 按直接路径或 ID 前缀查找 agy 会话。
func agySessionCandidates(sessionID, agyDir string) []agySessionCandidate {
	expanded := expandUser(sessionID)
	if info, err := os.Stat(expanded); err == nil && info.Mode().IsRegular() {
		return []agySessionCandidate{{sessionID: agySessionIDFromPath(expanded), path: expanded}}
	}

	brainDir := filepath.Join(expandUser(agyDir), "brain")
	entries, err := os.ReadDir(brainDir)
	if err != nil {
		return nil
	}
	var candidates []agySessionCandidate
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), sessionID) {
			continue
		}
		transcriptPath := agyTranscriptPath(filepath.Join(brainDir, entry.Name()))
		if transcriptPath == "" {
			continue
		}
		candidates = append(candidates, agySessionCandidate{
			sessionID: entry.Name(),
			path:      transcriptPath,
		})
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].sessionID < candidates[j].sessionID
	})
	return candidates
}

// resolveAgySession 解析 agy 会话 ID 或文件路径，支持唯一前缀。
func resolveAgySession(sessionID, agyDir string) (string, string, error) {
	candidates := agySessionCandidates(sessionID, agyDir)
	switch len(candidates) {
	case 0:
		return "", "", newHistoryError("找不到 agy 会话：%s", sessionID)
	case 1:
		return candidates[0].sessionID, candidates[0].path, nil
	default:
		ids := make([]string, 0, len(candidates))
		for _, candidate := range candidates {
			ids = append(ids, candidate.sessionID)
		}
		return "", "", newHistoryError("agy 会话前缀不唯一：%s", joinStrings(ids))
	}
}

// extractAgyUserRequest 从 USER_INPUT 的 content 中提取 <USER_REQUEST> 标签内的真实提问。
func extractAgyUserRequest(content string) string {
	start := strings.Index(content, agyUserRequestOpen)
	if start >= 0 {
		body := content[start+len(agyUserRequestOpen):]
		if end := strings.Index(body, agyUserRequestClose); end >= 0 {
			return strings.TrimSpace(body[:end])
		}
		return strings.TrimSpace(body)
	}
	return strings.TrimSpace(content)
}

// parseAgy 解析指定 agy 会话。
func parseAgy(sessionID, agyDir string, loc *time.Location, captureToolDetails, captureThinking bool) (*SessionData, error) {
	resolvedID, transcriptPath, err := resolveAgySession(sessionID, agyDir)
	if err != nil {
		return nil, err
	}
	meta := loadAgySessionMeta(expandUser(agyDir), resolvedID)
	return parseAgyFile(resolvedID, transcriptPath, meta.workingDir, loc, captureToolDetails, captureThinking)
}

// parseAgyFile 从 agy transcript JSONL 文件解析会话数据。
func parseAgyFile(sessionID, transcriptPath, workingDir string, loc *time.Location, captureToolDetails, captureThinking bool) (*SessionData, error) {
	file, err := os.Open(transcriptPath)
	if err != nil {
		return nil, newHistoryError("读取 agy 会话失败：%s：%v", transcriptPath, err)
	}
	defer file.Close()

	session := &SessionData{
		Source:     sourceAgy,
		SessionID:  sessionID,
		Path:       transcriptPath,
		WorkingDir: workingDir,
	}

	var (
		turns        []*ConversationTurn
		currentTurn  *ConversationTurn
		pendingTools []pendingToolCall
	)

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 1024*1024), 64*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var item map[string]any
		if err := json.Unmarshal([]byte(line), &item); err != nil || item == nil {
			continue
		}

		activityTime := parseTimestamp(item["created_at"], loc)
		session.addActivityTime(activityTime)

		eventType, _ := item["type"].(string)
		switch eventType {
		case "USER_INPUT":
			question := extractAgyUserRequest(getText(item["content"]))
			if question == "" {
				continue
			}
			if currentTurn != nil {
				turns = append(turns, currentTurn)
			}
			currentTurn = &ConversationTurn{Question: question}
			currentTurn.addTimestamp(activityTime)
			pendingTools = nil

		case "PLANNER_RESPONSE":
			if currentTurn == nil {
				currentTurn = &ConversationTurn{}
			}
			currentTurn.addTimestamp(activityTime)
			if model, ok := item["model"].(string); ok {
				session.addModel(model)
			}
			applyAgyTokenUsage(session, item)
			if captureThinking {
				if thinking, ok := item["thinking"].(string); ok {
					currentTurn.appendThinking(thinking)
				}
			}
			if toolCalls, ok := item["tool_calls"].([]any); ok {
				for _, rawCall := range toolCalls {
					call, ok := rawCall.(map[string]any)
					if !ok {
						continue
					}
					name, _ := call["name"].(string)
					name = strings.TrimSpace(name)
					if name == "" {
						name = "tool"
					}
					currentTurn.addTool(name)
					if captureToolDetails {
						callIndex := currentTurn.appendToolCall(name, jsonString(call["args"]), "")
						pendingTools = append(pendingTools, pendingToolCall{turn: currentTurn, index: callIndex})
					}
				}
			}
			if answer := strings.TrimSpace(getText(item["content"])); answer != "" {
				currentTurn.Answer = answer
			}

		case "GENERIC":
			if currentTurn != nil {
				currentTurn.addTimestamp(activityTime)
			}
			if captureToolDetails && len(pendingTools) > 0 {
				pending := pendingTools[0]
				pendingTools = pendingTools[1:]
				pending.turn.ToolCalls[pending.index].Output = formatAgyToolOutput(item)
			}

		case "CHECKPOINT":
			applyAgyTokenUsage(session, item)
			if summary := strings.TrimSpace(getText(item["content"])); summary != "" {
				session.CompactSummary = summary
			}

		default:
			if currentTurn != nil {
				currentTurn.addTimestamp(activityTime)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, newHistoryError("读取 agy 会话失败：%s：%v", transcriptPath, err)
	}

	if currentTurn != nil {
		turns = append(turns, currentTurn)
	}
	for _, turn := range turns {
		session.Turns = append(session.Turns, *turn)
	}
	session.refreshSummary()
	if len(session.Turns) == 0 {
		return nil, newHistoryError("agy 会话没有可解析内容：%s", sessionID)
	}
	return session, nil
}

// applyAgyTokenUsage 累加 agy 事件的 token 用量；input_tokens 为未缓存输入。
func applyAgyTokenUsage(session *SessionData, item map[string]any) {
	inputTokens := toInt(item["input_tokens"])
	outputTokens := toInt(item["output_tokens"])
	cacheRead := toInt(item["cache_read_tokens"])
	if inputTokens+outputTokens+cacheRead == 0 {
		return
	}
	session.addTokenUsage(inputTokens, outputTokens, cacheRead, 0)
}

// formatAgyToolOutput 合并 GENERIC 工具结果中的 content 与 error 字段。
func formatAgyToolOutput(item map[string]any) string {
	content := strings.TrimSpace(getText(item["content"]))
	errText := strings.TrimSpace(getText(item["error"]))
	if errText == "" {
		return content
	}
	if content == "" {
		return "Error: " + errText
	}
	return content + "\nError: " + errText
}

// listAgySessions 加载 agy 全部顶层会话（排除子代理）并按日期过滤。
func listAgySessions(agyDir string, loc *time.Location, targetDate *time.Time) ([]*SessionData, error) {
	expandedDir := expandUser(agyDir)
	brainDir := filepath.Join(expandedDir, "brain")
	entries, err := os.ReadDir(brainDir)
	if err != nil {
		return nil, newHistoryError("读取 agy 会话目录失败：%s：%v", brainDir, err)
	}

	metaMap := loadAgySessionMetaMap(expandedDir)
	var sessions []*SessionData
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		sessionID := entry.Name()
		meta := metaMap[sessionID]
		if meta.isSubagent {
			continue
		}
		transcriptPath := agyTranscriptPath(filepath.Join(brainDir, sessionID))
		if transcriptPath == "" {
			continue
		}
		session, err := parseAgyFile(sessionID, transcriptPath, meta.workingDir, loc, false, false)
		if err != nil {
			continue
		}
		if matchesDateFilter(session, targetDate) {
			sessions = append(sessions, session)
		}
	}
	return sessions, nil
}

// extractAgyTranscript 从 agy transcript 文件提取纯净的 user/assistant 对话文本。
func extractAgyTranscript(transcriptPath string) ([]transcriptMessage, error) {
	file, err := os.Open(transcriptPath)
	if err != nil {
		return nil, newHistoryError("读取会话源文件失败：%s：%v", transcriptPath, err)
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

		var role, content string
		switch item["type"] {
		case "USER_INPUT":
			role = "user"
			content = extractAgyUserRequest(getText(item["content"]))
		case "PLANNER_RESPONSE":
			role = "assistant"
			content = strings.TrimSpace(getText(item["content"]))
		default:
			continue
		}
		if content == "" {
			continue
		}
		message := transcriptMessage{Role: role, Content: content}
		if len(messages) > 0 && messages[len(messages)-1] == message {
			continue
		}
		messages = append(messages, message)
	}
	if err := scanner.Err(); err != nil {
		return nil, newHistoryError("读取会话源文件失败：%s：%v", transcriptPath, err)
	}
	return messages, nil
}
