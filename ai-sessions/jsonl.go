package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// sessionCandidates 查找匹配的 JSONL 会话文件。
func sessionCandidates(sessionID string, searchDirectories []string, excludeSubagents bool) []string {
	expanded := expandUser(sessionID)
	if info, err := os.Stat(expanded); err == nil && info.Mode().IsRegular() {
		return []string{expanded}
	}

	var candidates []string
	pattern := sessionID + ".jsonl"
	for _, searchDirectory := range searchDirectories {
		if _, err := os.Stat(searchDirectory); err != nil {
			continue
		}
		filepath.WalkDir(searchDirectory, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if entry.IsDir() {
				if excludeSubagents && entry.Name() == "subagents" {
					return filepath.SkipDir
				}
				return nil
			}
			if entry.Name() == pattern && !strings.HasSuffix(entry.Name(), ".wakatime") {
				candidates = append(candidates, path)
			}
			return nil
		})
	}
	return candidates
}

// claudeSessionCandidates 查找 Claude 会话文件。
func claudeSessionCandidates(sessionID, claudeDirectory string) []string {
	return sessionCandidates(sessionID, []string{
		filepath.Join(claudeDirectory, "projects"),
		filepath.Join(claudeDirectory, "transcripts"),
	}, true)
}

// resolveJSONLSession 解析 JSONL 会话文件路径（Claude/Qoder 通用）。
func resolveJSONLSession(source, sessionID, directory string) (string, error) {
	var candidates []string
	if source == sourceQoder {
		candidates = qoderSessionCandidates(sessionID, directory)
	} else {
		candidates = claudeSessionCandidates(sessionID, directory)
	}
	if len(candidates) == 0 {
		return "", newHistoryError("找不到 %s 会话：%s", source, sessionID)
	}
	if len(candidates) > 1 {
		return "", newHistoryError("%s 会话对应多个文件：%s", source, joinStrings(candidates))
	}
	return candidates[0], nil
}

// listSessionPaths 列出目录中的会话文件（排除子代理），按修改时间倒序。
func listSessionPaths(searchDirectories []string) []string {
	paths := make(map[string]struct{})
	for _, searchDirectory := range searchDirectories {
		if _, err := os.Stat(searchDirectory); err != nil {
			continue
		}
		filepath.WalkDir(searchDirectory, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if entry.IsDir() {
				if entry.Name() == "subagents" {
					return filepath.SkipDir
				}
				return nil
			}
			if strings.HasSuffix(entry.Name(), ".jsonl") && !strings.HasSuffix(entry.Name(), ".wakatime") {
				paths[path] = struct{}{}
			}
			return nil
		})
	}
	result := make([]string, 0, len(paths))
	for path := range paths {
		result = append(result, path)
	}
	sort.Slice(result, func(i, j int) bool {
		timeI, errI := os.Stat(result[i])
		timeJ, errJ := os.Stat(result[j])
		if errI != nil || errJ != nil {
			return false
		}
		return timeI.ModTime().After(timeJ.ModTime())
	})
	return result
}

// listClaudeSessionPaths 列出 Claude 项目和 transcript 中的会话文件（排除子代理）。
func listClaudeSessionPaths(claudeDirectory string) []string {
	return listSessionPaths([]string{
		filepath.Join(claudeDirectory, "projects"),
		filepath.Join(claudeDirectory, "transcripts"),
	})
}

// qoderSessionCandidates 查找 Qoder 会话文件。
func qoderSessionCandidates(sessionID, qoderDirectory string) []string {
	return sessionCandidates(sessionID, []string{filepath.Join(qoderDirectory, "projects")}, false)
}

// listQoderSessionPaths 列出 Qoder 项目中的会话文件（排除子代理）。
func listQoderSessionPaths(qoderDirectory string) []string {
	return listSessionPaths([]string{filepath.Join(qoderDirectory, "projects")})
}

// isRealUserInput 过滤 Claude/Qoder 的内部命令和工具返回消息。
func isRealUserInput(item map[string]any, content string) bool {
	if content == "" || item["isMeta"] == true {
		return false
	}
	switch item["toolUseResult"].(type) {
	case map[string]any, []any, string:
		return false
	}
	return !strings.Contains(content, "<local-command-caveat>") &&
		!strings.Contains(content, "<command-name>")
}

// pendingToolCall 记录待回填输出的工具调用位置。
type pendingToolCall struct {
	turn  *ConversationTurn
	index int
}

// parseJSONLSession 解析 JSONL 格式的会话（Claude/Qoder 通用）。
// captureToolDetails 和 captureThinking 控制是否采集工具详情和思考内容。
func parseJSONLSession(source, sessionPath string, loc *time.Location, allowEmpty, captureToolDetails, captureThinking bool) (*SessionData, error) {
	ext := filepath.Ext(sessionPath)
	session := &SessionData{Source: source, SessionID: strings.TrimSuffix(filepath.Base(sessionPath), ext), Path: sessionPath}
	var turns []*ConversationTurn
	var currentTurn *ConversationTurn
	seenMessageIDs := make(map[string]struct{})
	pendingResults := make(map[string]pendingToolCall)

	file, err := os.Open(sessionPath)
	if err != nil {
		return nil, newHistoryError("读取 %s 会话失败：%s：%v", source, sessionPath, err)
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 1024*1024), 64*1024*1024)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		var item map[string]any
		if err := json.Unmarshal([]byte(line), &item); err != nil {
			continue
		}
		if item == nil {
			continue
		}

		session.addActivityTimestamp(item["timestamp"], loc)
		if session.WorkingDir == "" {
			if cwd, ok := item["cwd"].(string); ok && cwd != "" {
				session.WorkingDir = cwd
			}
		}
		if session.PlanSlug == "" {
			if slug, ok := item["slug"].(string); ok && slug != "" {
				session.PlanSlug = slug
			}
		}
		if session.PlanSlug == "" {
			if attachment, ok := item["attachment"].(map[string]any); ok {
				if planPath, ok := attachment["planFilePath"].(string); ok && planPath != "" {
					session.PlanSlug = strings.TrimSuffix(filepath.Base(planPath), ".md")
				}
			}
		}
		if session.PlanSlug == "" {
			if planPath, ok := item["planFilePath"].(string); ok && planPath != "" {
				session.PlanSlug = strings.TrimSuffix(filepath.Base(planPath), ".md")
			}
		}

		itemType, _ := item["type"].(string)
		var message map[string]any
		if typed, ok := item["message"].(map[string]any); ok {
			message = typed
		}
		messageRole, _ := message["role"].(string)
		content := getText(message["content"])
		if content == "" {
			content = getText(item["content"])
		}
		if content != "" && item["isCompactSummary"] == true {
			session.CompactSummary = strings.TrimSpace(content)
		}

		if itemType == "user" && messageRole != "assistant" {
			if isRealUserInput(item, content) {
				if currentTurn != nil {
					turns = append(turns, currentTurn)
				}
				currentTurn = &ConversationTurn{Question: strings.TrimSpace(content)}
				continue
			}
		}

		if currentTurn == nil {
			currentTurn = &ConversationTurn{}
		}
		if captureThinking {
			currentTurn.appendThinking(getJSONLThinking(message, item))
		}
		if captureToolDetails {
			forEachToolBlock(item, func(block map[string]any) {
				name := toolBlockName(block)
				if block["type"] == "tool_use" {
					currentTurn.addTool(name)
					callIndex := currentTurn.appendToolCall(name, jsonString(block["input"]), "")
					if toolUseID, ok := block["id"].(string); ok && toolUseID != "" {
						pendingResults[toolUseID] = pendingToolCall{turn: currentTurn, index: callIndex}
					}
					return
				}
				toolUseID, _ := block["tool_use_id"].(string)
				if pending, ok := pendingResults[toolUseID]; ok {
					pending.turn.ToolCalls[pending.index].Output = getText(block["content"])
				}
			})
		} else {
			for _, name := range getToolNames(item) {
				currentTurn.addTool(name)
			}
		}
		if itemType == "assistant" || messageRole == "assistant" {
			if strings.TrimSpace(content) != "" {
				currentTurn.Answer = strings.TrimSpace(content)
			}
			if messageID, ok := message["id"].(string); ok && messageID != "" {
				if _, seen := seenMessageIDs[messageID]; !seen {
					seenMessageIDs[messageID] = struct{}{}
					if usage, ok := message["usage"].(map[string]any); ok {
						inputTokens := toInt(usage["input_tokens"])
						outputTokens := toInt(usage["output_tokens"])
						cacheHit := toInt(usage["cache_read_input_tokens"])
						cacheMiss := toInt(usage["cache_creation_input_tokens"])
						session.addTokenUsage(inputTokens, outputTokens, cacheHit, cacheMiss)
					}
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, newHistoryError("读取 %s 会话失败：%s：%v", source, sessionPath, err)
	}

	if currentTurn != nil {
		turns = append(turns, currentTurn)
	}
	for _, turn := range turns {
		session.Turns = append(session.Turns, *turn)
	}
	session.refreshSummary()
	if !allowEmpty && len(session.Inputs) == 0 && len(session.Tools) == 0 && session.FinalOutput == "" {
		return nil, newHistoryError("%s 会话没有可解析内容：%s（最后检查行：%d）", source, sessionPath, lineNumber)
	}
	return session, nil
}

// parseJSONLBySource 解析 JSONL 会话（Claude/Qoder 按来源分发）。
// captureToolDetails 和 captureThinking 控制是否采集工具详情和思考内容。
func parseJSONLBySource(source, sessionID, directory string, loc *time.Location, allowEmpty, captureToolDetails, captureThinking bool) (*SessionData, error) {
	sessionPath, err := resolveJSONLSession(source, sessionID, directory)
	if err != nil {
		return nil, err
	}
	return parseJSONLSession(source, sessionPath, loc, allowEmpty, captureToolDetails, captureThinking)
}

// getJSONLThinking 提取 JSONL 助手消息中的思考字段和内容块。
func getJSONLThinking(message, item map[string]any) string {
	var parts []string
	for _, source := range []map[string]any{message, item} {
		for _, key := range []string{"reasoning_content", "reasoning_details", "thinking", "reasoning"} {
			if value, ok := source[key]; ok {
				parts = append(parts, getThinkingText(value))
			}
		}
		if content, ok := source["content"]; ok {
			parts = append(parts, getThinkingBlocksText(content))
		}
	}
	return strings.Join(parts, "\n")
}

// detectSource 根据会话 ID 和本地历史文件判断来源。
func detectSource(sessionID, claudeDirectory, qoderDirectory, qoderAppDirectory string) string {
	if strings.HasPrefix(sessionID, "ses_") || strings.HasSuffix(sessionID, ".jsonl") {
		return sourceClaude
	}
	if info, err := os.Stat(expandUser(sessionID)); err == nil && info.Mode().IsRegular() {
		return sourceClaude
	}
	if len(claudeSessionCandidates(sessionID, claudeDirectory)) > 0 {
		return sourceClaude
	}
	if len(qoderSessionCandidates(sessionID, qoderDirectory)) > 0 {
		return sourceQoder
	}
	if _, err := resolveQoderAppSession(sessionID, qoderAppDirectory); err == nil {
		return sourceQoderApp
	}
	return sourceCodex
}

// loadAllSessions 加载指定来源的全部可解析会话。
func loadAllSessions(source, codexDatabase, claudeDirectory, qoderDirectory, qoderAppDirectory string, loc *time.Location, targetDate *time.Time, includeArchived bool) ([]*SessionData, error) {
	var sessions []*SessionData

	if source == sourceAll || source == sourceCodex {
		if err := ensureCodexIndex(codexDatabase); err != nil {
			return nil, err
		}
		sessionIDs, err := listCodexSessionIDs(codexDatabase, loc, targetDate, includeArchived)
		if err != nil {
			return nil, err
		}
		for _, sessionID := range sessionIDs {
			session, err := parseCodex(sessionID, codexDatabase, loc, false, false)
			if err != nil {
				return nil, err
			}
			sessions = append(sessions, session)
		}
	}

	if source == sourceAll || source == sourceClaude {
		for _, sessionPath := range listClaudeSessionPaths(claudeDirectory) {
			session, err := parseJSONLBySource(sourceClaude, sessionPath, claudeDirectory, loc, true, false, false)
			if err != nil {
				fmt.Fprintf(os.Stderr, "警告：跳过 Claude 会话：%v\n", err)
				continue
			}
			if matchesDateFilter(session, targetDate) {
				sessions = append(sessions, session)
			}
		}
	}

	if source == sourceAll || source == sourceQoder {
		for _, sessionPath := range listQoderSessionPaths(qoderDirectory) {
			session, err := parseJSONLBySource(sourceQoder, sessionPath, qoderDirectory, loc, true, false, false)
			if err != nil {
				fmt.Fprintf(os.Stderr, "警告：跳过 Qoder 会话：%v\n", err)
				continue
			}
			if matchesDateFilter(session, targetDate) {
				sessions = append(sessions, session)
			}
		}
	}

	if source == sourceAll || source == sourceQoderApp {
		sessions = append(sessions, listQoderAppSessions(qoderAppDirectory, loc, targetDate)...)
	}

	if len(sessions) == 0 {
		return nil, errNoSessions
	}
	return sessions, nil
}

// matchesDateFilter 判断会话是否满足日期过滤和内容过滤。
func matchesDateFilter(session *SessionData, targetDate *time.Time) bool {
	if len(session.Inputs) == 0 && len(session.Tools) == 0 && session.FinalOutput == "" {
		return false
	}
	if targetDate == nil {
		return true
	}
	endDate := session.endedDate()
	if endDate == nil {
		return false
	}
	return endDate.Equal(*targetDate)
}

// formatSessionTime 格式化会话时间字符串。
func formatSessionTime(startedAt, endedAt *time.Time, loc *time.Location) string {
	if startedAt != nil && endedAt != nil {
		return fmt.Sprintf("%s ~ %s (TZ=%s)",
			startedAt.Format("2006-01-02 15:04"), endedAt.Format("2006-01-02 15:04"), loc)
	}
	return fmt.Sprintf("（无） (TZ=%s)", loc)
}
