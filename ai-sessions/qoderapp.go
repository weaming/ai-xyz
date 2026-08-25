package main

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
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

// parseQoderAppSession 解析新版 Qoder 会话。
// captureThinking 控制是否把中间助手消息作为思考内容输出；
// meta 提供应用内的时间元数据，为 nil 时退回文件修改时间。
func parseQoderAppSession(sessionPath string, loc *time.Location, allowEmpty, captureThinking bool, meta *qoderAppMetadata) (*SessionData, error) {
	session := &SessionData{Source: sourceQoderApp, SessionID: qoderAppSessionID(sessionPath), Path: sessionPath}

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
	applyQoderAppMetadata(session, sessionPath, loc, meta)
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
	meta, _ := loadQoderAppMetadata()
	return parseQoderAppSession(sessionPath, loc, false, captureThinking, meta)
}

// listQoderAppSessions 加载新版 Qoder 全部会话并按日期过滤。
func listQoderAppSessions(qoderAppDirectory string, loc *time.Location, targetDate *time.Time) []*SessionData {
	meta, _ := loadQoderAppMetadata()
	var sessions []*SessionData
	for _, sessionPath := range listQoderAppSessionPaths(qoderAppDirectory) {
		session, err := parseQoderAppSession(sessionPath, loc, true, false, meta)
		if err != nil {
			continue
		}
		if matchesDateFilter(session, targetDate) {
			sessions = append(sessions, session)
		}
	}
	return sessions
}

// qoderAppQuery 记录一条用户提问的提交时间与问题文本。
type qoderAppQuery struct {
	title string
	at    time.Time
}

// qoderAppMetadata 来自 Qoder 应用状态库的保守时间信息：
// 任务快照的创建/结束时间，与逐条用户提问的提交时间。
type qoderAppMetadata struct {
	tasks      map[string]qoderAppTaskTiming
	queryTimes map[string][]qoderAppQuery
}

// qoderAppTaskTiming 记录任务快照中的毫秒时间戳，0 表示缺失。
type qoderAppTaskTiming struct {
	createTimeMs int64
	endTimeMs    int64
}

// qoderAppStateDB 返回 Qoder 应用全局状态库路径。
func qoderAppStateDB() string {
	return filepath.Join(homeDir(), "Library", "Application Support", "QoderCN", "User", "globalStorage", "state.vscdb")
}

// loadQoderAppMetadata 读取状态库中的任务快照与提问历史，任何缺失返回空元数据。
func loadQoderAppMetadata() (*qoderAppMetadata, error) {
	databasePath := qoderAppStateDB()
	if _, err := os.Stat(databasePath); err != nil {
		return &qoderAppMetadata{}, err
	}
	db, err := sql.Open("sqlite", "file:"+databasePath+"?mode=ro")
	if err != nil {
		return &qoderAppMetadata{}, err
	}
	defer db.Close()

	meta := &qoderAppMetadata{
		tasks:      make(map[string]qoderAppTaskTiming),
		queryTimes: make(map[string][]qoderAppQuery),
	}

	var snapshot string
	if err := db.QueryRow("SELECT value FROM ItemTable WHERE key = 'aicoding.questTaskListSnapshot'").Scan(&snapshot); err == nil {
		parseQoderAppTaskSnapshot(snapshot, meta)
	}

	rows, err := db.Query("SELECT value FROM ItemTable WHERE key LIKE 'lingma.chat.localHistory%.quest'")
	if err != nil {
		return meta, nil
	}
	defer rows.Close()
	for rows.Next() {
		var historyText string
		if err := rows.Scan(&historyText); err != nil {
			continue
		}
		parseQoderAppLocalHistory(historyText, meta)
	}
	for _, queries := range meta.queryTimes {
		sort.Slice(queries, func(i, j int) bool { return queries[i].at.Before(queries[j].at) })
	}
	return meta, nil
}

// parseQoderAppTaskSnapshot 提取任务快照中每个任务的创建与结束时间。
func parseQoderAppTaskSnapshot(snapshotText string, meta *qoderAppMetadata) {
	var snapshot struct {
		Folders map[string]struct {
			Tasks []struct {
				ID         string `json:"id"`
				CreateTime int64  `json:"createTime"`
				EndTime    int64  `json:"endTime"`
				FinishedAt int64  `json:"finishedAt"`
			} `json:"tasks"`
		} `json:"folders"`
	}
	if err := json.Unmarshal([]byte(snapshotText), &snapshot); err != nil {
		return
	}
	for _, folder := range snapshot.Folders {
		for _, task := range folder.Tasks {
			if task.ID == "" {
				continue
			}
			endMs := task.EndTime
			if endMs == 0 {
				endMs = task.FinishedAt
			}
			meta.tasks[task.ID] = qoderAppTaskTiming{createTimeMs: task.CreateTime, endTimeMs: endMs}
		}
	}
}

// parseQoderAppLocalHistory 按完整任务 ID 归集每条用户提问的提交时间与文本。
func parseQoderAppLocalHistory(historyText string, meta *qoderAppMetadata) {
	var entries []struct {
		SessionID string `json:"sessionId"`
		Title     string `json:"title"`
		Timestamp int64  `json:"timestamp"`
	}
	if err := json.Unmarshal([]byte(historyText), &entries); err != nil {
		return
	}
	for _, entry := range entries {
		taskID := strings.TrimSuffix(entry.SessionID, ".session.execution")
		if taskID == "" || entry.Timestamp <= 0 {
			continue
		}
		meta.queryTimes[taskID] = append(meta.queryTimes[taskID], qoderAppQuery{
			title: entry.Title,
			at:    time.UnixMilli(entry.Timestamp),
		})
	}
}

// applyQoderAppMetadata 为会话注入应用内时间：
// 起止时间优先用任务快照与提问历史，缺失时退回文件修改时间；
// 各轮开始时间按问题文本与提问历史逐条匹配，再按相邻轮次近似推算用时。
func applyQoderAppMetadata(session *SessionData, sessionPath string, loc *time.Location, meta *qoderAppMetadata) {
	var fileTime *time.Time
	if info, err := os.Stat(sessionPath); err == nil {
		mtime := info.ModTime().In(loc)
		fileTime = &mtime
	}

	fullID := ""
	shortID := ""
	if parts := strings.SplitN(session.SessionID, "/", 2); len(parts) == 2 {
		shortID = parts[1]
	}
	if meta != nil && shortID != "" {
		for taskID := range meta.tasks {
			if strings.HasPrefix(taskID, shortID) {
				if fullID != "" && fullID != taskID {
					fullID = ""
					break
				}
				fullID = taskID
			}
		}
	}

	var startedAt, endedAt *time.Time
	if meta != nil && fullID != "" {
		if timing, ok := meta.tasks[fullID]; ok {
			if timing.createTimeMs > 0 {
				createTime := time.UnixMilli(timing.createTimeMs).In(loc)
				startedAt = &createTime
			}
			if timing.endTimeMs > 0 {
				endTime := time.UnixMilli(timing.endTimeMs).In(loc)
				endedAt = &endTime
			}
		}
		if queries := meta.queryTimes[fullID]; len(queries) > 0 {
			firstQuery := queries[0].at.In(loc)
			if startedAt == nil || firstQuery.Before(*startedAt) {
				startedAt = &firstQuery
			}
			if endedAt == nil {
				lastQuery := queries[len(queries)-1].at.In(loc)
				endedAt = &lastQuery
			}
			nextQuery := 0
			for turnIndex := range session.Turns {
				question := normalizeWhitespace(session.Turns[turnIndex].Question)
				if question == "" {
					continue
				}
				for queryIndex := nextQuery; queryIndex < len(queries); queryIndex++ {
					if normalizeWhitespace(queries[queryIndex].title) == question {
						queryTime := queries[queryIndex].at.In(loc)
						session.Turns[turnIndex].StartedAt = &queryTime
						nextQuery = queryIndex + 1
						break
					}
				}
			}
		}
	}
	if startedAt == nil {
		startedAt = fileTime
	}
	if endedAt == nil {
		endedAt = fileTime
	}
	session.StartedAt = startedAt
	session.EndedAt = endedAt
	approximateQoderAppTurnDurations(session)
}

// approximateQoderAppTurnDurations 按相邻已标注轮次近似推算用时：
// 本轮用时 ≈ 下一已标注轮开始时间 − 本轮开始时间，
// 最后一个已标注轮 ≈ 会话结束时间 − 本轮开始时间；未标注的轮次不给用时。
func approximateQoderAppTurnDurations(session *SessionData) {
	var matched []int
	for index := range session.Turns {
		if session.Turns[index].StartedAt != nil {
			matched = append(matched, index)
		}
	}
	for position, index := range matched {
		var end *time.Time
		if position+1 < len(matched) {
			end = session.Turns[matched[position+1]].StartedAt
		} else if session.EndedAt != nil {
			end = session.EndedAt
		}
		if end == nil || end.Before(*session.Turns[index].StartedAt) {
			continue
		}
		endTime := *end
		session.Turns[index].EndedAt = &endTime
	}
}

// normalizeWhitespace 压缩空白以便比较问题文本与提问历史标题。
func normalizeWhitespace(text string) string {
	return strings.Join(strings.Fields(text), " ")
}
