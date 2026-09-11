package main

import (
	"database/sql"
	"encoding/json"
	"strings"
	"time"
)

// zcode（Z.ai 的编码 CLI）把会话历史存在 ~/.zcode/cli/db/db.sqlite：
// session 存会话元信息，message 存消息头（角色、语义、模型、token 用量），
// part 存消息内容块（text/reasoning/tool/step-*），message 与 part 的 data 列都是 JSON。
// 真实用户提问的 semantics.kind 为 user_prompt，助手回答为 assistant_response，
// 其余（todo_reminder、timeline_event 等系统消息）不参与问答重建。

// zcode 消息的语义类型。
const (
	zcodeKindUserPrompt        = "user_prompt"
	zcodeKindAssistantResponse = "assistant_response"
)

// connectZcodeDatabase 以只读方式打开 zcode 历史数据库。
func connectZcodeDatabase(databasePath string) (*sql.DB, error) {
	return connectReadonly(databasePath, "zcode")
}

// resolveZcodeSessionID 解析 zcode 会话 ID，支持唯一前缀。
func resolveZcodeSessionID(databasePath, sessionID string) (string, error) {
	db, err := connectZcodeDatabase(databasePath)
	if err != nil {
		return "", err
	}
	defer db.Close()

	rows, err := db.Query("SELECT id FROM session WHERE id = ? OR id LIKE ? ORDER BY id", sessionID, sessionID+"%")
	if err != nil {
		return "", newHistoryError("查询 zcode 会话失败：%v", err)
	}
	defer rows.Close()

	var matches []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return "", newHistoryError("查询 zcode 会话失败：%v", err)
		}
		matches = append(matches, id)
	}
	if err := rows.Err(); err != nil {
		return "", newHistoryError("查询 zcode 会话失败：%v", err)
	}
	switch len(matches) {
	case 0:
		return "", newHistoryError("找不到 zcode 会话：%s", sessionID)
	case 1:
		return matches[0], nil
	default:
		return "", newHistoryError("zcode 会话前缀不唯一：%s", joinStrings(matches))
	}
}

// zcodeSessionMeta 会话表中的工作目录与起止时间。
type zcodeSessionMeta struct {
	directory string
	createdAt *time.Time
	updatedAt *time.Time
}

// loadZcodeSessionMeta 读取会话表元信息，记录缺失时返回零值。
func loadZcodeSessionMeta(db *sql.DB, sessionID string, loc *time.Location) zcodeSessionMeta {
	var meta zcodeSessionMeta
	var directory string
	var createdMS, updatedMS int64
	if err := db.QueryRow(
		"SELECT directory, time_created, time_updated FROM session WHERE id = ?", sessionID,
	).Scan(&directory, &createdMS, &updatedMS); err != nil {
		return meta
	}
	meta.directory = directory
	meta.createdAt = parseTimestamp(float64(createdMS), loc)
	meta.updatedAt = parseTimestamp(float64(updatedMS), loc)
	return meta
}

// parseZcode 解析 zcode SQLite 会话。
// captureToolDetails 和 captureThinking 控制是否采集工具详情和思考内容。
func parseZcode(sessionID, databasePath string, loc *time.Location, captureToolDetails, captureThinking bool) (*SessionData, error) {
	resolvedID, err := resolveZcodeSessionID(databasePath, sessionID)
	if err != nil {
		return nil, err
	}
	db, err := connectZcodeDatabase(databasePath)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	session := &SessionData{Source: sourceZcode, SessionID: resolvedID, Path: databasePath}
	meta := loadZcodeSessionMeta(db, resolvedID, loc)
	session.WorkingDir = meta.directory

	rows, err := db.Query(
		`SELECT m.id, m.data, COALESCE(p.data, '') FROM message m
		 LEFT JOIN part p ON p.message_id = m.id
		 WHERE m.session_id = ?
		 ORDER BY m.time_created, m.sequence, m.id, p.sequence`,
		resolvedID,
	)
	if err != nil {
		return nil, newHistoryError("查询 zcode 会话内容失败：%v", err)
	}
	defer rows.Close()

	var (
		turns     []*ConversationTurn
		turnsByID = make(map[string]*ConversationTurn)
		lastID    string
		message   map[string]any
		parts     []map[string]any
	)
	// 消息行按内容块逐行返回，切换到下一条消息前先归并当前消息。
	flush := func() {
		if message == nil {
			return
		}
		appendZcodeMessage(session, &turns, turnsByID, message, parts, loc, captureToolDetails, captureThinking)
		message, parts = nil, nil
	}
	for rows.Next() {
		var messageID, messageJSON, partJSON string
		if err := rows.Scan(&messageID, &messageJSON, &partJSON); err != nil {
			return nil, newHistoryError("读取 zcode 会话内容失败：%v", err)
		}
		if messageID != lastID {
			flush()
			lastID = messageID
			if err := json.Unmarshal([]byte(messageJSON), &message); err != nil {
				message = nil
			}
		}
		if partJSON != "" {
			var part map[string]any
			if err := json.Unmarshal([]byte(partJSON), &part); err == nil && part != nil {
				parts = append(parts, part)
			}
		}
	}
	flush()
	if err := rows.Err(); err != nil {
		return nil, newHistoryError("读取 zcode 会话内容失败：%v", err)
	}

	// 消息时间缺失时退回会话表记录的时间。
	if session.StartedAt == nil {
		session.StartedAt = meta.createdAt
	}
	if session.EndedAt == nil {
		session.EndedAt = meta.updatedAt
	}
	for _, turn := range turns {
		session.Turns = append(session.Turns, *turn)
	}
	session.refreshSummary()
	if len(session.Turns) == 0 {
		return nil, newHistoryError("zcode 会话没有可解析内容：%s", resolvedID)
	}
	return session, nil
}

// appendZcodeMessage 把一条 zcode 消息归并到对应轮次。
func appendZcodeMessage(session *SessionData, turns *[]*ConversationTurn, turnsByID map[string]*ConversationTurn,
	message map[string]any, parts []map[string]any, loc *time.Location, captureToolDetails, captureThinking bool) {
	kind := zcodeNestedString(message, "semantics", "kind")
	if kind != zcodeKindUserPrompt && kind != zcodeKindAssistantResponse {
		return
	}
	turnID := zcodeNestedString(message, "anchor", "turnId")
	if turnID == "" {
		return
	}
	turn, ok := turnsByID[turnID]
	if !ok {
		turn = &ConversationTurn{}
		turnsByID[turnID] = turn
		*turns = append(*turns, turn)
	}

	createdAt := zcodeMessageTime(message, "created", loc)
	completedAt := zcodeMessageTime(message, "completed", loc)
	turn.addTimestamp(createdAt)
	turn.addTimestamp(completedAt)
	session.addActivityTime(createdAt)
	session.addActivityTime(completedAt)

	if kind == zcodeKindUserPrompt {
		turn.Question = zcodePartsText(parts, "text")
		return
	}

	if model, ok := message["modelID"].(string); ok {
		session.addModel(model)
	}
	applyZcodeTokenUsage(session, message)
	if captureThinking {
		turn.appendThinking(zcodePartsText(parts, "reasoning"))
	}

	var answerParts []string
	for _, part := range parts {
		switch part["type"] {
		case "text":
			if text := strings.TrimSpace(getText(part["text"])); text != "" {
				answerParts = append(answerParts, text)
			}
		case "tool":
			appendZcodeToolCall(turn, part, captureToolDetails)
		}
	}
	if len(answerParts) > 0 {
		turn.Answer = strings.Join(answerParts, "\n")
	}
}

// appendZcodeToolCall 记录一次工具调用，仅在需要详情时保留输入输出。
func appendZcodeToolCall(turn *ConversationTurn, part map[string]any, captureToolDetails bool) {
	name, _ := part["tool"].(string)
	name = strings.TrimSpace(name)
	if name == "" {
		name = "tool"
	}
	turn.addTool(name)
	if !captureToolDetails {
		return
	}
	state, _ := part["state"].(map[string]any)
	output := getText(state["output"])
	if strings.TrimSpace(output) == "" {
		output = getText(state["error"])
	}
	turn.appendToolCall(name, jsonString(state["input"]), strings.TrimSpace(output))
}

// applyZcodeTokenUsage 累加一条助手消息的 token 用量。
// zcode 的 input 已包含缓存读写，需要拆出未缓存部分以对齐其它来源的统计口径。
func applyZcodeTokenUsage(session *SessionData, message map[string]any) {
	tokens, ok := message["tokens"].(map[string]any)
	if !ok {
		return
	}
	input := toInt(tokens["input"])
	output := toInt(tokens["output"])
	cache, _ := tokens["cache"].(map[string]any)
	cacheHit := toInt(cache["read"])
	cacheMiss := toInt(cache["write"])
	uncachedInput := maxInt(input-cacheHit-cacheMiss, 0)
	if input+output+cacheHit+cacheMiss == 0 {
		return
	}
	session.addTokenUsage(uncachedInput, output, cacheHit, cacheMiss)
}

// zcodePartsText 拼接指定类型内容块的文本。
func zcodePartsText(parts []map[string]any, partType string) string {
	var texts []string
	for _, part := range parts {
		if part["type"] != partType {
			continue
		}
		if text := strings.TrimSpace(getText(part["text"])); text != "" {
			texts = append(texts, text)
		}
	}
	return strings.Join(texts, "\n")
}

// zcodeMessageTime 读取消息 time 字段中的毫秒时间戳。
func zcodeMessageTime(message map[string]any, key string, loc *time.Location) *time.Time {
	timeBlock, ok := message["time"].(map[string]any)
	if !ok {
		return nil
	}
	value, ok := timeBlock[key]
	if !ok {
		return nil
	}
	return parseTimestamp(value, loc)
}

// zcodeNestedString 逐层读取嵌套 JSON 字段中的字符串，任一层缺失返回空串。
func zcodeNestedString(item map[string]any, keys ...string) string {
	var current any = item
	for _, key := range keys {
		typed, ok := current.(map[string]any)
		if !ok {
			return ""
		}
		nested, ok := typed[key]
		if !ok {
			return ""
		}
		current = nested
	}
	text, _ := current.(string)
	return text
}

// listZcodeSessionIDs 列出 zcode 全部会话 ID，按最近更新倒序。
func listZcodeSessionIDs(databasePath string) ([]string, error) {
	db, err := connectZcodeDatabase(databasePath)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	rows, err := db.Query("SELECT id FROM session ORDER BY time_updated DESC")
	if err != nil {
		return nil, newHistoryError("列出 zcode 会话失败：%v", err)
	}
	defer rows.Close()

	var sessionIDs []string
	for rows.Next() {
		var sessionID string
		if err := rows.Scan(&sessionID); err != nil {
			return nil, newHistoryError("列出 zcode 会话失败：%v", err)
		}
		sessionIDs = append(sessionIDs, sessionID)
	}
	return sessionIDs, rows.Err()
}

// listZcodeSessions 加载 zcode 全部会话并按日期过滤，数据库不可用时返回错误。
func listZcodeSessions(databasePath string, loc *time.Location, targetDate *time.Time) ([]*SessionData, error) {
	sessionIDs, err := listZcodeSessionIDs(databasePath)
	if err != nil {
		return nil, err
	}
	var sessions []*SessionData
	for _, sessionID := range sessionIDs {
		session, err := parseZcode(sessionID, databasePath, loc, false, false)
		if err != nil {
			continue
		}
		if matchesDateFilter(session, targetDate) {
			sessions = append(sessions, session)
		}
	}
	return sessions, nil
}

// extractZcodeTranscript 从 zcode 数据库读取纯净的 user/assistant 对话文本。
func extractZcodeTranscript(databasePath, sessionID string) ([]transcriptMessage, error) {
	db, err := connectZcodeDatabase(databasePath)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	rows, err := db.Query(
		`SELECT m.id, m.data, COALESCE(p.data, '') FROM message m
		 LEFT JOIN part p ON p.message_id = m.id
		 WHERE m.session_id = ?
		 ORDER BY m.time_created, m.sequence, m.id, p.sequence`,
		sessionID,
	)
	if err != nil {
		return nil, newHistoryError("查询 zcode 会话内容失败：%v", err)
	}
	defer rows.Close()

	var (
		messages []transcriptMessage
		lastID   string
		role     string
		texts    []string
	)
	flush := func() {
		if role == "" || len(texts) == 0 {
			return
		}
		message := transcriptMessage{Role: role, Content: strings.Join(texts, "\n")}
		if len(messages) > 0 && messages[len(messages)-1] == message {
			return
		}
		messages = append(messages, message)
	}
	for rows.Next() {
		var messageID, messageJSON, partJSON string
		if err := rows.Scan(&messageID, &messageJSON, &partJSON); err != nil {
			return nil, newHistoryError("读取 zcode 会话内容失败：%v", err)
		}
		if messageID != lastID {
			flush()
			lastID, role, texts = messageID, "", nil
			var message map[string]any
			if err := json.Unmarshal([]byte(messageJSON), &message); err == nil {
				role = zcodeTranscriptRole(message)
			}
		}
		if role == "" || partJSON == "" {
			continue
		}
		var part map[string]any
		if err := json.Unmarshal([]byte(partJSON), &part); err != nil || part["type"] != "text" {
			continue
		}
		if text := strings.TrimSpace(getText(part["text"])); text != "" {
			texts = append(texts, text)
		}
	}
	flush()
	if err := rows.Err(); err != nil {
		return nil, newHistoryError("读取 zcode 会话内容失败：%v", err)
	}
	return messages, nil
}

// zcodeTranscriptRole 把 zcode 消息映射为对话角色，系统消息返回空串。
func zcodeTranscriptRole(message map[string]any) string {
	switch zcodeNestedString(message, "semantics", "kind") {
	case zcodeKindUserPrompt:
		return "user"
	case zcodeKindAssistantResponse:
		return "assistant"
	}
	return ""
}
