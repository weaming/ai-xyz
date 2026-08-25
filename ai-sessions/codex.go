package main

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// connectReadonly 以只读方式打开 SQLite 历史数据库。
func connectReadonly(databasePath string) (*sql.DB, error) {
	if _, err := os.Stat(databasePath); err != nil {
		return nil, newHistoryError("找不到 Codex 历史数据库：%s", databasePath)
	}
	db, err := sql.Open("sqlite", "file:"+databasePath+"?mode=ro")
	if err != nil {
		return nil, newHistoryError("打开 Codex 历史数据库失败：%v", err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, newHistoryError("打开 Codex 历史数据库失败：%v", err)
	}
	return db, nil
}

// ensureCodexIndex 确保 thread_items 表有列表查询所需的索引。
func ensureCodexIndex(databasePath string) error {
	db, err := sql.Open("sqlite", databasePath)
	if err != nil {
		return newHistoryError("创建 Codex 索引失败：%v", err)
	}
	defer db.Close()
	var exists int
	err = db.QueryRow(
		"SELECT 1 FROM sqlite_master WHERE type='index' AND name='idx_thread_items_list_sessions'",
	).Scan(&exists)
	if err == nil {
		return nil
	}
	if _, err := db.Exec(
		"CREATE INDEX idx_thread_items_list_sessions ON thread_items(thread_id, created_at_ms)",
	); err != nil {
		return newHistoryError("创建 Codex 索引失败：%v", err)
	}
	return nil
}

// resolveCodexSession 解析 Codex 会话 ID，并允许使用唯一前缀。
func resolveCodexSession(sessionID, databasePath string) (string, string, error) {
	db, err := connectReadonly(databasePath)
	if err != nil {
		return "", "", err
	}
	defer db.Close()
	rows, err := db.Query(
		`SELECT DISTINCT thread_id FROM thread_items WHERE thread_id = ? OR thread_id LIKE ? ORDER BY thread_id`,
		sessionID, sessionID+"%",
	)
	if err != nil {
		return "", "", newHistoryError("查询 Codex 会话失败：%v", err)
	}
	defer rows.Close()
	var matches []string
	for rows.Next() {
		var threadID string
		if err := rows.Scan(&threadID); err != nil {
			return "", "", newHistoryError("查询 Codex 会话失败：%v", err)
		}
		matches = append(matches, threadID)
	}
	if len(matches) == 0 {
		return "", "", newHistoryError("找不到 Codex 会话：%s", sessionID)
	}
	if len(matches) > 1 {
		return "", "", newHistoryError("Codex 会话前缀不唯一：%s", joinStrings(matches))
	}
	if err := rows.Err(); err != nil {
		return "", "", newHistoryError("查询 Codex 会话失败：%v", err)
	}
	return matches[0], databasePath, nil
}

// parseCodex 解析 Codex SQLite 会话。
// captureToolDetails 和 captureThinking 控制是否采集工具详情和思考内容。
func parseCodex(sessionID, databasePath string, loc *time.Location, captureToolDetails, captureThinking bool) (*SessionData, error) {
	resolvedID, resolvedPath, err := resolveCodexSession(sessionID, databasePath)
	if err != nil {
		return nil, err
	}
	session := &SessionData{Source: sourceCodex, SessionID: resolvedID, Path: resolvedPath}

	db, err := connectReadonly(databasePath)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	// 归档状态与 cwd 以 Codex 应用的 state 元数据库为准，旧布局回退历史库自身。
	if meta, ok := lookupCodexThreadMeta(resolvedID, resolvedPath); ok {
		session.IsArchived = meta.archived
		session.WorkingDir = meta.cwd
	}
	rows, err := db.Query(
		`SELECT item_type, item_json, created_at_ms, turn_id FROM thread_items WHERE thread_id = ? ORDER BY rollout_ordinal`,
		resolvedID,
	)
	if err != nil {
		return nil, newHistoryError("查询 Codex 会话内容失败：%v", err)
	}
	defer rows.Close()
	tokenStats, requestHitRates, models, err := parseCodexTokenUsage(resolvedID, databasePath)
	if err != nil {
		return nil, err
	}
	session.TokenStats = tokenStats
	session.RequestHitRates = requestHitRates
	for _, model := range models {
		session.addModel(model)
	}

	var turnOrder []string
	turnsByID := make(map[string]*ConversationTurn)
	for rows.Next() {
		var itemType, itemJSON string
		var createdAtMS int64
		var turnID sql.NullString
		if err := rows.Scan(&itemType, &itemJSON, &createdAtMS, &turnID); err != nil {
			return nil, newHistoryError("读取 Codex 会话内容失败：%v", err)
		}
		activityTime := parseTimestamp(float64(createdAtMS), loc)
		session.addActivityTime(activityTime)

		turnKey := turnID.String
		turn, ok := turnsByID[turnKey]
		if !ok {
			turn = &ConversationTurn{}
			turnsByID[turnKey] = turn
			turnOrder = append(turnOrder, turnKey)
		}
		turn.addTimestamp(activityTime)

		var item map[string]any
		if err := json.Unmarshal([]byte(itemJSON), &item); err != nil {
			continue
		}
		if item == nil {
			continue
		}
		if captureThinking {
			if itemType == "reasoning" || itemType == "agentReasoning" || itemType == "agentReasoningRawContent" || item["type"] == "reasoning" {
				turn.appendThinking(getThinkingText(item))
			}
			if payload, ok := item["payload"].(map[string]any); ok && payload["type"] == "reasoning" {
				turn.appendThinking(getThinkingText(payload))
			}
		}

		switch itemType {
		case "userMessage":
			content := strings.TrimSpace(getText(item["content"]))
			if content != "" {
				if turn.Question != "" {
					turn.Question += "\n" + content
				} else {
					turn.Question = content
				}
			}
		case "commandExecution":
			turn.addTool("shell")
			if captureToolDetails {
				input, _ := item["command"].(string)
				output, _ := item["aggregatedOutput"].(string)
				turn.appendToolCall("shell", strings.TrimSpace(input), strings.TrimSpace(output))
			}
			if session.WorkingDir == "" {
				if cwd, ok := item["cwd"].(string); ok && cwd != "" {
					session.WorkingDir = cwd
				}
			}
		case "fileChange":
			turn.addTool("fileChange")
			if captureToolDetails {
				turn.appendToolCall("fileChange", jsonString(item["changes"]), "")
			}
		case "imageView":
			turn.addTool("imageView")
			if captureToolDetails {
				path, _ := item["path"].(string)
				turn.appendToolCall("imageView", path, "")
			}
		case "webSearch":
			turn.addTool("webSearch")
			if captureToolDetails {
				query, _ := item["query"].(string)
				turn.appendToolCall("webSearch", query, jsonString(item["results"]))
			}
		case "mcpToolCall":
			toolName := "mcpToolCall"
			if tool, ok := item["tool"].(string); ok {
				toolName = tool
			}
			turn.addTool(toolName)
			if captureToolDetails {
				turn.appendToolCall(toolName, jsonString(item["arguments"]), codexResultText(item["result"]))
			}
		case "agentMessage":
			output, ok := item["text"].(string)
			if ok && strings.TrimSpace(output) != "" {
				output = strings.TrimSpace(output)
				if item["phase"] == "final_answer" || turn.Answer == "" {
					turn.Answer = output
				}
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, newHistoryError("读取 Codex 会话内容失败：%v", err)
	}

	for _, key := range turnOrder {
		session.Turns = append(session.Turns, *turnsByID[key])
	}
	session.refreshSummary()
	return session, nil
}

// codexThreadMeta 线程级元数据。
type codexThreadMeta struct {
	cwd      string
	archived bool
}

// findCodexStateDatabase 返回版本号最新的 Codex state 元数据库路径，不存在时返回空串。
func findCodexStateDatabase() string {
	matches, _ := filepath.Glob(filepath.Join(homeDir(), ".codex", "state_*.sqlite"))
	sort.Slice(matches, func(left, right int) bool {
		versionOf := func(path string) int {
			version, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(filepath.Base(path), "state_"), ".sqlite"))
			if err != nil {
				return 0
			}
			return version
		}
		return versionOf(matches[left]) < versionOf(matches[right])
	})
	if len(matches) == 0 {
		return ""
	}
	return matches[len(matches)-1]
}

// codexMetaPaths 返回可能存放 threads 表的库路径，新布局的 state 库在前。
func codexMetaPaths(historyPath string) []string {
	if statePath := findCodexStateDatabase(); statePath != "" && statePath != historyPath {
		return []string{statePath, historyPath}
	}
	return []string{historyPath}
}

// getCodexThreadMeta 从指定库读取线程 cwd 与归档状态，无该表或记录、库不可用时返回 ok=false。
func getCodexThreadMeta(databasePath, sessionID string) (codexThreadMeta, bool) {
	db, err := connectReadonly(databasePath)
	if err != nil {
		return codexThreadMeta{}, false
	}
	defer db.Close()
	var archived int
	meta := codexThreadMeta{}
	err = db.QueryRow("SELECT COALESCE(cwd, ''), COALESCE(archived, 0) FROM threads WHERE id = ?", sessionID).Scan(&meta.cwd, &archived)
	if err != nil {
		return codexThreadMeta{}, false
	}
	meta.archived = archived != 0
	return meta, true
}

// lookupCodexThreadMeta 按优先级在候选库中查找线程元数据。
func lookupCodexThreadMeta(sessionID, databasePath string) (codexThreadMeta, bool) {
	for _, path := range codexMetaPaths(databasePath) {
		if meta, ok := getCodexThreadMeta(path, sessionID); ok {
			return meta, true
		}
	}
	return codexThreadMeta{}, false
}

// parseCodexTokenUsage 解析 Codex rollout 日志中的累计 Token、单次命中率与使用过的模型。
func parseCodexTokenUsage(sessionID, databasePath string) (TokenUsage, []float64, []string, error) {
	rolloutPath, err := findCodexRolloutPath(sessionID, databasePath)
	if err != nil || rolloutPath == "" {
		return TokenUsage{}, nil, nil, err
	}

	file, err := os.Open(rolloutPath)
	if err != nil {
		return TokenUsage{}, nil, nil, newHistoryError("读取 Codex rollout 失败：%s：%v", rolloutPath, err)
	}
	defer file.Close()

	var tokenStats TokenUsage
	var requestHitRates []float64
	var models []string
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 1024*1024), 64*1024*1024)
	for scanner.Scan() {
		var item map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &item); err != nil {
			continue
		}
		payload, ok := item["payload"].(map[string]any)
		if !ok {
			continue
		}
		if item["type"] == "turn_context" {
			if model, ok := payload["model"].(string); ok && strings.TrimSpace(model) != "" {
				models = appendUniqueString(models, model)
			}
			continue
		}
		if payload["type"] != "token_count" {
			continue
		}

		info, ok := payload["info"].(map[string]any)
		if !ok {
			continue
		}
		if totalUsage, ok := info["total_token_usage"].(map[string]any); ok {
			tokenStats = codexTokenUsage(totalUsage)
		}
		if lastUsage, ok := info["last_token_usage"].(map[string]any); ok {
			requestHitRates = appendRequestHitRate(requestHitRates, codexTokenUsage(lastUsage))
		}
	}
	if err := scanner.Err(); err != nil {
		return TokenUsage{}, nil, nil, newHistoryError("读取 Codex rollout 失败：%s：%v", rolloutPath, err)
	}
	return tokenStats, requestHitRates, models, nil
}

// appendUniqueString 按首次出现顺序追加不重复的字符串。
func appendUniqueString(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

// findCodexRolloutPath 查找与 Codex 会话对应的 rollout 日志，依次搜索 sessions 与 archived_sessions 目录。
func findCodexRolloutPath(sessionID, databasePath string) (string, error) {
	baseDirectory := filepath.Dir(databasePath)
	for _, name := range []string{"sessions", "archived_sessions"} {
		rolloutPath, err := findRolloutInDirectory(sessionID, filepath.Join(baseDirectory, name))
		if err != nil || rolloutPath != "" {
			return rolloutPath, err
		}
	}
	return "", nil
}

// findRolloutInDirectory 在单个目录及其按日期分层的子目录中查找 rollout 文件，目录不存在时返回空串。
func findRolloutInDirectory(sessionID, directory string) (string, error) {
	if _, err := os.Stat(directory); err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", newHistoryError("读取 Codex rollout 目录失败：%s：%v", directory, err)
	}

	patterns := []string{
		filepath.Join(directory, "*", "*", "*", "*-"+sessionID+".jsonl"),
		filepath.Join(directory, "*-"+sessionID+".jsonl"),
	}
	for _, pattern := range patterns {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			return "", newHistoryError("查找 Codex rollout 失败：%s：%v", pattern, err)
		}
		if len(matches) > 0 {
			return matches[0], nil
		}
	}
	return "", nil
}

// codexTokenUsage 将 Codex 的总输入 Token 拆分为未缓存、缓存命中和缓存创建。
func codexTokenUsage(usage map[string]any) TokenUsage {
	totalInput := maxInt(toInt(usage["input_tokens"]), 0)
	cacheHit := maxInt(toInt(usage["cached_input_tokens"]), 0)
	cacheMiss := maxInt(toInt(usage["cache_write_input_tokens"]), 0)
	uncachedInput := totalInput - cacheHit - cacheMiss
	if uncachedInput < 0 {
		uncachedInput = 0
	}
	return TokenUsage{
		InputTokens:     uncachedInput,
		OutputTokens:    maxInt(toInt(usage["output_tokens"]), 0),
		CacheHitTokens:  cacheHit,
		CacheMissTokens: cacheMiss,
	}
}

// appendRequestHitRate 追加一次请求的缓存命中率。
func appendRequestHitRate(rates []float64, usage TokenUsage) []float64 {
	totalInput := usage.TotalInputTokens()
	if totalInput == 0 {
		return rates
	}
	return append(rates, float64(usage.CacheHitTokens)/float64(totalInput)*100)
}

func maxInt(value, minimum int) int {
	if value < minimum {
		return minimum
	}
	return value
}

// listCodexSessionIDs 列出 Codex 历史中的会话 ID。
func listCodexSessionIDs(databasePath string, loc *time.Location, targetDate *time.Time, includeArchived bool) ([]string, error) {
	db, err := connectReadonly(databasePath)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	query := `
		SELECT thread_items.thread_id, MAX(thread_items.created_at_ms) AS end_ms
		FROM thread_items`
	var excludedArchived map[string]struct{}
	if !includeArchived {
		excludedArchived = codexArchivedIDs(databasePath)
	}
	query += " GROUP BY thread_items.thread_id"
	queryParameters := []any{}
	if targetDate != nil {
		startTimestamp, endTimestamp := getDateTimestampBounds(*targetDate, loc)
		query += " HAVING end_ms >= ? AND end_ms < ?"
		queryParameters = append(queryParameters, startTimestamp, endTimestamp)
	}
	query += " ORDER BY end_ms DESC"

	rows, err := db.Query(query, queryParameters...)
	if err != nil {
		return nil, newHistoryError("列出 Codex 会话失败：%v", err)
	}
	defer rows.Close()
	var sessionIDs []string
	for rows.Next() {
		var threadID string
		var endMS int64
		if err := rows.Scan(&threadID, &endMS); err != nil {
			return nil, newHistoryError("列出 Codex 会话失败：%v", err)
		}
		if _, ok := excludedArchived[threadID]; ok {
			continue
		}
		sessionIDs = append(sessionIDs, threadID)
	}
	return sessionIDs, rows.Err()
}

// codexArchivedIDs 返回已归档会话 ID 集合，以 Codex 应用的 state 元数据库为准；
// 候选库均无 threads 表时返回 nil，表示无归档数据、不过滤。
func codexArchivedIDs(databasePath string) map[string]struct{} {
	for _, path := range codexMetaPaths(databasePath) {
		db, err := connectReadonly(path)
		if err != nil {
			continue
		}
		rows, err := db.Query("SELECT id FROM threads WHERE archived = 1")
		if err != nil {
			db.Close()
			continue
		}
		ids := make(map[string]struct{})
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err == nil {
				ids[id] = struct{}{}
			}
		}
		rows.Close()
		db.Close()
		return ids
	}
	return nil
}

// codexResultText 提取 MCP 工具结果的文本内容。
func codexResultText(result any) string {
	if typed, ok := result.(map[string]any); ok {
		return getText(typed["content"])
	}
	return getText(result)
}

// joinStrings 用逗号连接字符串列表。
func joinStrings(items []string) string {
	var builder strings.Builder
	for index, item := range items {
		if index > 0 {
			builder.WriteString(", ")
		}
		builder.WriteString(item)
	}
	return builder.String()
}
