package main

import (
	"database/sql"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// zcodeFixtureMessage 一条待写入 fixture 的消息及其内容块。
type zcodeFixtureMessage struct {
	id      string
	timeMS  int64
	message string
	parts   []string
}

// writeZcodeFixture 创建最小可用的 zcode 历史数据库，返回数据库路径。
func writeZcodeFixture(t *testing.T, sessionID, directory string, timeCreatedMS, timeUpdatedMS int64, messages []zcodeFixtureMessage) string {
	t.Helper()
	databasePath := filepath.Join(t.TempDir(), "db.sqlite")
	db, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	statements := []string{
		`CREATE TABLE session (id text primary key, directory text, time_created integer, time_updated integer)`,
		`CREATE TABLE message (id text primary key, session_id text, time_created integer, sequence integer, data text)`,
		`CREATE TABLE part (id text primary key, message_id text, session_id text, sequence integer, data text)`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(
		`INSERT INTO session (id, directory, time_created, time_updated) VALUES (?, ?, ?, ?)`,
		sessionID, directory, timeCreatedMS, timeUpdatedMS,
	); err != nil {
		t.Fatal(err)
	}
	for index, message := range messages {
		if _, err := db.Exec(
			`INSERT INTO message (id, session_id, time_created, sequence, data) VALUES (?, ?, ?, ?, ?)`,
			message.id, sessionID, message.timeMS, index, message.message,
		); err != nil {
			t.Fatal(err)
		}
		for partIndex, part := range message.parts {
			if _, err := db.Exec(
				`INSERT INTO part (id, message_id, session_id, sequence, data) VALUES (?, ?, ?, ?, ?)`,
				message.id+"-part-"+strconv.Itoa(partIndex), message.id, sessionID, partIndex, part,
			); err != nil {
				t.Fatal(err)
			}
		}
	}
	return databasePath
}

// zcodeFixtureMessages 两轮问答的 fixture：第一轮含思考与一次工具调用。
func zcodeFixtureMessages() []zcodeFixtureMessage {
	const baseMS = 1784880000000 // 2026-07-24 16:00:00 +08:00
	return []zcodeFixtureMessage{
		{
			id: "msg1", timeMS: baseMS,
			message: `{"role":"user","time":{"created":1784880000000},"semantics":{"kind":"user_prompt"},"anchor":{"turnId":"turn_a"}}`,
			parts: []string{
				`{"type":"text","text":"问题一"}`,
				`{"type":"step-start"}`,
			},
		},
		{
			id: "msg2", timeMS: baseMS + 10,
			message: `{"role":"assistant","time":{"created":1784880000010,"completed":1784880060000},"modelID":"GLM-5.3","tokens":{"total":1050,"input":1000,"output":50,"cache":{"read":400,"write":100}},"semantics":{"kind":"assistant_response"},"anchor":{"turnId":"turn_a"}}`,
			parts: []string{
				`{"type":"reasoning","text":"先看看仓库"}`,
				`{"type":"tool","tool":"Bash","state":{"status":"completed","input":{"command":"ls"},"output":"file1\nfile2"}}`,
				`{"type":"text","text":"回答一"}`,
			},
		},
		{
			id: "msg3", timeMS: baseMS + 20,
			message: `{"role":"user","time":{"created":1784880000020},"semantics":{"kind":"todo_reminder"},"anchor":{"turnId":"turn_a"}}`,
			parts:   []string{`{"type":"text","text":"待办提醒"}`},
		},
		{
			id: "msg4", timeMS: baseMS + 30,
			message: `{"role":"user","time":{"created":1784880000030},"semantics":{"kind":"user_prompt"},"anchor":{"turnId":"turn_b"}}`,
			parts:   []string{`{"type":"text","text":"问题二"}`},
		},
		{
			id: "msg5", timeMS: baseMS + 40,
			message: `{"role":"assistant","time":{"created":1784880000040,"completed":1784880061000},"modelID":"GLM-5.3-Flash","tokens":{"total":120,"input":100,"output":20,"cache":{"read":0,"write":0}},"semantics":{"kind":"assistant_response"},"anchor":{"turnId":"turn_b"}}`,
			parts:   []string{`{"type":"text","text":"回答二"}`},
		},
	}
}

func TestParseZcode(t *testing.T) {
	databasePath := writeZcodeFixture(t, "sess_test-0001", "/tmp/proj", 1784880000000, 1784880061000, zcodeFixtureMessages())

	session, err := parseZcode("sess_test", databasePath, testLocation, true, true)
	if err != nil {
		t.Fatal(err)
	}
	if session.Source != sourceZcode || session.SessionID != "sess_test-0001" {
		t.Fatalf("Source/SessionID = %s/%s", session.Source, session.SessionID)
	}
	if session.WorkingDir != "/tmp/proj" || session.Path != databasePath {
		t.Fatalf("WorkingDir/Path = %s/%s", session.WorkingDir, session.Path)
	}
	if len(session.Turns) != 2 {
		t.Fatalf("Turns = %d, 期望 2（todo_reminder 不应产生轮次）", len(session.Turns))
	}

	first := session.Turns[0]
	if first.Question != "问题一" || first.Answer != "回答一" {
		t.Fatalf("Q/A = %q/%q", first.Question, first.Answer)
	}
	if first.Thinking != "先看看仓库" {
		t.Fatalf("Thinking = %q", first.Thinking)
	}
	if len(first.Tools) != 1 || first.Tools[0] != "Bash" {
		t.Fatalf("Tools = %v", first.Tools)
	}
	if len(first.ToolCalls) != 1 || first.ToolCalls[0].Input != `{"command":"ls"}` || first.ToolCalls[0].Output != "file1\nfile2" {
		t.Fatalf("ToolCalls = %+v", first.ToolCalls)
	}
	if duration := first.Duration(); duration == nil || duration.Seconds() != 60 {
		t.Fatalf("第一轮用时 = %v", duration)
	}
	if session.Turns[1].Question != "问题二" || session.Turns[1].Answer != "回答二" {
		t.Fatalf("第二轮 Q/A = %q/%q", session.Turns[1].Question, session.Turns[1].Answer)
	}

	stats := session.TokenStats
	if stats.InputTokens != 600 || stats.CacheHitTokens != 400 || stats.CacheMissTokens != 100 || stats.OutputTokens != 70 {
		t.Fatalf("TokenStats = %+v", stats)
	}
	if stats.TotalInputTokens() != 1100 {
		t.Fatalf("TotalInputTokens = %d, 期望 1100", stats.TotalInputTokens())
	}
	if strings.Join(session.Models, ",") != "GLM-5.3,GLM-5.3-Flash" {
		t.Fatalf("Models = %v", session.Models)
	}
	if session.StartedAt == nil || session.EndedAt == nil {
		t.Fatal("时间应来自消息时间戳")
	}
	if got := session.StartedAt.Format("2006-01-02 15:04:05"); got != "2026-07-24 16:00:00" {
		t.Fatalf("StartedAt = %s", got)
	}
}

func TestParseZcodeWithoutToolDetails(t *testing.T) {
	databasePath := writeZcodeFixture(t, "sess_test-0001", "/tmp/proj", 1784880000000, 1784880061000, zcodeFixtureMessages())

	session, err := parseZcode("sess_test-0001", databasePath, testLocation, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(session.Turns[0].Tools) != 1 {
		t.Fatalf("Tools = %v", session.Turns[0].Tools)
	}
	if len(session.Turns[0].ToolCalls) != 0 || session.Turns[0].Thinking != "" {
		t.Fatalf("不应采集工具详情与思考：%+v", session.Turns[0])
	}
}

func TestResolveZcodeSessionID(t *testing.T) {
	messages := zcodeFixtureMessages()
	databasePath := writeZcodeFixture(t, "sess_test-0001", "/tmp/proj", 1784880000000, 1784880061000, messages)

	if resolved, err := resolveZcodeSessionID(databasePath, "sess_test"); err != nil || resolved != "sess_test-0001" {
		t.Fatalf("前缀解析 = %s, %v", resolved, err)
	}
	if _, err := resolveZcodeSessionID(databasePath, "sess_other"); err == nil {
		t.Fatal("未知会话应报错")
	}
}

func TestListZcodeSessions(t *testing.T) {
	databasePath := writeZcodeFixture(t, "sess_test-0001", "/tmp/proj", 1784880000000, 1784880061000, zcodeFixtureMessages())

	sessions, err := listZcodeSessions(databasePath, testLocation, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].SessionID != "sess_test-0001" {
		t.Fatalf("会话列表 = %v", sessions)
	}

	otherDay := time.Date(2026, 7, 25, 0, 0, 0, 0, testLocation)
	if sessions, err := listZcodeSessions(databasePath, testLocation, &otherDay); err != nil || len(sessions) != 0 {
		t.Fatalf("日期过滤应排除该会话：%v", sessions)
	}
}

func TestExtractZcodeTranscript(t *testing.T) {
	databasePath := writeZcodeFixture(t, "sess_test-0001", "/tmp/proj", 1784880000000, 1784880061000, zcodeFixtureMessages())

	messages, err := extractZcodeTranscript(databasePath, "sess_test-0001")
	if err != nil {
		t.Fatal(err)
	}
	want := []transcriptMessage{
		{Role: "user", Content: "问题一"},
		{Role: "assistant", Content: "回答一"},
		{Role: "user", Content: "问题二"},
		{Role: "assistant", Content: "回答二"},
	}
	if len(messages) != len(want) {
		t.Fatalf("消息数 = %d, 期望 %d：%+v", len(messages), len(want), messages)
	}
	for index, message := range want {
		if messages[index] != message {
			t.Fatalf("第 %d 条 = %+v, 期望 %+v", index, messages[index], message)
		}
	}
}

func TestDetectZcodeSource(t *testing.T) {
	databasePath := writeZcodeFixture(t, "sess_test-0001", "/tmp/proj", 1784880000000, 1784880061000, zcodeFixtureMessages())
	tempDir := t.TempDir()
	opts := &options{
		claudeDir:     filepath.Join(tempDir, "claude"),
		qoderDir:      filepath.Join(tempDir, "qoder"),
		qoderAppDir:   filepath.Join(tempDir, "qoder-app"),
		zcodeDatabase: databasePath,
	}
	if source := detectSource("sess_test-0001", opts); source != sourceZcode {
		t.Fatalf("detectSource = %s, 期望 %s", source, sourceZcode)
	}
}
