package main

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// 新版 Qoder 应用把聊天历史存到 <qoder-cn>/cache/projects/<项目>/conversation-history/<task>/<task>.jsonl，
// 每行形如 {"role":"user|assistant","message":{"content":[{"type":"text","text":...}]}}，
// 与 Claude 风格 JSONL 不兼容，因此单独解析。

var (
	qoderAppQueryPattern  = regexp.MustCompile(`(?s)<user_query>\s*(.*?)\s*</user_query>`)
	qoderAppReminderBlock = regexp.MustCompile(`(?s)</?(?:system-reminder|system_reminder)>.*?(?:</(?:system-reminder|system_reminder)>|$)`)
)

// qoderAppProjectSlug 取会话文件所属的项目目录名。
// 目录结构为 <项目>/conversation-history/<task>/<task>.jsonl。
func qoderAppProjectSlug(sessionPath string) string {
	return filepath.Base(filepath.Dir(filepath.Dir(filepath.Dir(sessionPath))))
}

// qoderAppSessionID 生成 "<项目>/<task>" 形式的会话 ID，避免不同项目的 task 重名。
func qoderAppSessionID(sessionPath string) string {
	taskID := strings.TrimSuffix(filepath.Base(sessionPath), filepath.Ext(sessionPath))
	return filepath.Join(qoderAppProjectSlug(sessionPath), taskID)
}

// listQoderAppSessionPaths 列出新版 Qoder 会话文件，按修改时间倒序。
func listQoderAppSessionPaths(qoderAppDirectory string) []string {
	var paths []string
	if _, err := os.Stat(qoderAppDirectory); err != nil {
		return paths
	}
	filepath.WalkDir(qoderAppDirectory, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		if filepath.Base(filepath.Dir(filepath.Dir(path))) != "conversation-history" {
			return nil
		}
		if strings.HasSuffix(entry.Name(), ".jsonl") {
			paths = append(paths, path)
		}
		return nil
	})
	sort.Slice(paths, func(i, j int) bool {
		infoI, errI := os.Stat(paths[i])
		infoJ, errJ := os.Stat(paths[j])
		if errI != nil || errJ != nil {
			return false
		}
		return infoI.ModTime().After(infoJ.ModTime())
	})
	return paths
}

// resolveQoderAppSession 支持按文件路径、完整会话 ID、task ID 或唯一前缀解析会话。
func resolveQoderAppSession(sessionID, qoderAppDirectory string) (string, error) {
	expanded := expandUser(sessionID)
	if info, err := os.Stat(expanded); err == nil && info.Mode().IsRegular() {
		return expanded, nil
	}
	matches := make(map[string]struct{})
	for _, path := range listQoderAppSessionPaths(qoderAppDirectory) {
		fullID := qoderAppSessionID(path)
		taskID := filepath.Base(fullID)
		if fullID == sessionID || taskID == sessionID ||
			strings.HasPrefix(fullID, sessionID) || strings.HasPrefix(taskID, sessionID) {
			matches[path] = struct{}{}
		}
	}
	if len(matches) == 0 {
		return "", newHistoryError("找不到 qoder-app 会话：%s", sessionID)
	}
	if len(matches) > 1 {
		ids := make([]string, 0, len(matches))
		for path := range matches {
			ids = append(ids, qoderAppSessionID(path))
		}
		sort.Strings(ids)
		return "", newHistoryError("qoder-app 会话前缀不唯一：%s", joinStrings(ids))
	}
	for path := range matches {
		return path, nil
	}
	return "", newHistoryError("找不到 qoder-app 会话：%s", sessionID)
}

// extractQoderAppQuestion 提取用户消息中的 <user_query> 内容，无标签时去掉系统提醒块。
func extractQoderAppQuestion(content string) string {
	queryMatches := qoderAppQueryPattern.FindAllStringSubmatch(content, -1)
	if len(queryMatches) > 0 {
		var queries []string
		for _, match := range queryMatches {
			query := strings.TrimSpace(match[1])
			if query != "" {
				queries = append(queries, query)
			}
		}
		if len(queries) > 0 {
			return strings.Join(queries, "\n")
		}
	}
	return strings.TrimSpace(qoderAppReminderBlock.ReplaceAllString(content, ""))
}

// parseQoderAppSession 解析新版 Qoder 会话，时间取文件修改时间。
// captureThinking 控制是否把中间助手消息作为思考内容输出。
func parseQoderAppSession(sessionPath string, loc *time.Location, allowEmpty, captureThinking bool) (*SessionData, error) {
	session := &SessionData{Source: "qoder-app", SessionID: qoderAppSessionID(sessionPath), Path: sessionPath}
	if info, err := os.Stat(sessionPath); err == nil {
		fileTime := info.ModTime().In(loc)
		session.StartedAt = &fileTime
		session.EndedAt = &fileTime
	}

	file, err := os.Open(sessionPath)
	if err != nil {
		return nil, newHistoryError("读取 qoder-app 会话失败：%s：%v", sessionPath, err)
	}
	defer file.Close()

	var turns []*ConversationTurn
	var currentTurn *ConversationTurn
	seenLines := make(map[string]struct{})
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 1024*1024), 64*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if _, seen := seenLines[line]; seen {
			continue
		}
		seenLines[line] = struct{}{}
		var item map[string]any
		if err := json.Unmarshal([]byte(line), &item); err != nil {
			continue
		}
		message, _ := item["message"].(map[string]any)
		content := strings.TrimSpace(getText(message["content"]))
		if content == "" {
			continue
		}

		switch item["role"] {
		case "user":
			if currentTurn != nil {
				turns = append(turns, currentTurn)
			}
			currentTurn = &ConversationTurn{Question: extractQoderAppQuestion(content)}
		case "assistant":
			if currentTurn == nil {
				currentTurn = &ConversationTurn{}
			}
			if captureThinking && currentTurn.Answer != "" {
				currentTurn.appendThinking(currentTurn.Answer)
			}
			currentTurn.Answer = content
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, newHistoryError("读取 qoder-app 会话失败：%s：%v", sessionPath, err)
	}
	if currentTurn != nil {
		turns = append(turns, currentTurn)
	}
	for _, turn := range turns {
		session.Turns = append(session.Turns, *turn)
	}
	session.refreshSummary()
	if !allowEmpty && len(session.Inputs) == 0 && session.FinalOutput == "" {
		return nil, newHistoryError("qoder-app 会话没有可解析内容：%s", sessionPath)
	}
	return session, nil
}

// parseQoderApp 按会话 ID 解析新版 Qoder 会话。
func parseQoderApp(sessionID, qoderAppDirectory string, loc *time.Location, captureThinking bool) (*SessionData, error) {
	sessionPath, err := resolveQoderAppSession(sessionID, qoderAppDirectory)
	if err != nil {
		return nil, err
	}
	return parseQoderAppSession(sessionPath, loc, false, captureThinking)
}

// listQoderAppSessions 加载新版 Qoder 全部会话并按日期过滤。
func listQoderAppSessions(qoderAppDirectory string, loc *time.Location, targetDate *time.Time) []*SessionData {
	var sessions []*SessionData
	for _, sessionPath := range listQoderAppSessionPaths(qoderAppDirectory) {
		session, err := parseQoderAppSession(sessionPath, loc, true, false)
		if err != nil {
			continue
		}
		if matchesDateFilter(session, targetDate) {
			sessions = append(sessions, session)
		}
	}
	return sessions
}
