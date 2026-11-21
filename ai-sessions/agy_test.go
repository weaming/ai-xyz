package main

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// agyFixtureSession 描述一个待写入临时 agy 目录的测试会话。
type agyFixtureSession struct {
	id         string
	parentID   string
	workingDir string
	lines      []string
}

// writeAgyFixture 在临时目录构造 agy 的 brain 目录与 conversation_summaries.db。
func writeAgyFixture(t *testing.T, sessions []agyFixtureSession) string {
	t.Helper()
	agyDir := t.TempDir()
	dbPath := filepath.Join(agyDir, "conversation_summaries.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if _, err := db.Exec(`CREATE TABLE conversation_summaries (
		conversation_id text primary key,
		workspace_uris text not null default '',
		parent_conversation_id text not null default ''
	)`); err != nil {
		t.Fatal(err)
	}

	for _, item := range sessions {
		uris := "[]"
		if item.workingDir != "" {
			uris = `["file://` + item.workingDir + `"]`
		}
		if _, err := db.Exec(
			`INSERT INTO conversation_summaries (conversation_id, workspace_uris, parent_conversation_id) VALUES (?, ?, ?)`,
			item.id, uris, item.parentID,
		); err != nil {
			t.Fatal(err)
		}
		if len(item.lines) == 0 {
			continue
		}
		logsDir := filepath.Join(agyDir, "brain", item.id, ".system_generated", "logs")
		if err := os.MkdirAll(logsDir, 0o755); err != nil {
			t.Fatal(err)
		}
		transcriptPath := filepath.Join(logsDir, "transcript_full.jsonl")
		if err := os.WriteFile(transcriptPath, []byte(strings.Join(item.lines, "\n")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return agyDir
}

func agySampleLines() []string {
	return []string{
		`{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","created_at":"2026-11-21T08:00:00Z","content":"<USER_REQUEST>\n分析这个仓库\n</USER_REQUEST>\n<ADDITIONAL_METADATA>\nThe current local time is: 2026-11-21T16:00:00+08:00.\n</ADDITIONAL_METADATA>"}`,
		`{"step_index":1,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-11-21T08:00:05Z","model":"gemini-4-neon-medium","input_tokens":300,"cache_read_tokens":700,"output_tokens":50,"thinking":"先读 README","tool_calls":[{"name":"read_file","args":{"Path":"/tmp/README.md"}},{"name":"run_command","args":{"CommandLine":"ls"}}]}`,
		`{"step_index":2,"source":"MODEL","type":"GENERIC","status":"DONE","created_at":"2026-11-21T08:00:06Z","content":"# Demo Repo"}`,
		`{"step_index":3,"source":"MODEL","type":"GENERIC","status":"DONE","created_at":"2026-11-21T08:00:07Z","content":"Created At: ...","error":"permission denied"}`,
		`{"step_index":4,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-11-21T08:00:20Z","model":"gemini-4-neon-medium","input_tokens":200,"cache_read_tokens":800,"output_tokens":40,"content":"这是一个演示仓库。"}`,
		`{"step_index":5,"source":"SYSTEM","type":"CHECKPOINT","status":"DONE","created_at":"2026-11-21T08:00:30Z","input_tokens":100,"cache_read_tokens":0,"output_tokens":20,"content":"摘要内容"}`,
		`{"step_index":6,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","created_at":"2026-11-21T08:01:00Z","content":"<USER_REQUEST>\n继续优化\n</USER_REQUEST>"}`,
		`{"step_index":7,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-11-21T08:01:10Z","model":"gemini-4-argon-medium","input_tokens":100,"cache_read_tokens":900,"output_tokens":30,"content":"已优化完成。"}`,
	}
}

func TestParseAgy(t *testing.T) {
	agyDir := writeAgyFixture(t, []agyFixtureSession{
		{id: "40f69db6-364c-4692-8d48-5806402324df", workingDir: "/Users/garden/src/ai-xyz", lines: agySampleLines()},
	})

	session, err := parseAgy("40f69db6", agyDir, testLocation, true, true)
	if err != nil {
		t.Fatal(err)
	}
	if session.Source != sourceAgy || session.SessionID != "40f69db6-364c-4692-8d48-5806402324df" {
		t.Fatalf("Source/SessionID = %s/%s", session.Source, session.SessionID)
	}
	if session.WorkingDir != "/Users/garden/src/ai-xyz" {
		t.Fatalf("WorkingDir = %q, 期望 /Users/garden/src/ai-xyz", session.WorkingDir)
	}
	if session.CompactSummary != "摘要内容" {
		t.Fatalf("CompactSummary = %q, 期望 摘要内容", session.CompactSummary)
	}
	if len(session.Turns) != 2 {
		t.Fatalf("Turns = %d, 期望 2", len(session.Turns))
	}

	first := session.Turns[0]
	if first.Question != "分析这个仓库" || first.Answer != "这是一个演示仓库。" || first.Thinking != "先读 README" {
		t.Fatalf("第一轮 = %+v", first)
	}
	if len(first.ToolCalls) != 2 {
		t.Fatalf("第一轮 ToolCalls = %d, 期望 2", len(first.ToolCalls))
	}
	if first.ToolCalls[0].Name != "read_file" || first.ToolCalls[0].Output != "# Demo Repo" {
		t.Fatalf("工具 1 = %+v", first.ToolCalls[0])
	}
	if first.ToolCalls[1].Name != "run_command" || !strings.Contains(first.ToolCalls[1].Output, "Error: permission denied") {
		t.Fatalf("工具 2 = %+v", first.ToolCalls[1])
	}
	if session.Turns[1].Question != "继续优化" || session.Turns[1].Answer != "已优化完成。" {
		t.Fatalf("第二轮 = %+v", session.Turns[1])
	}

	if strings.Join(session.Models, ",") != "gemini-4-neon-medium,gemini-4-argon-medium" {
		t.Fatalf("Models = %v", session.Models)
	}
	if session.TokenStats.InputTokens != 700 || session.TokenStats.CacheHitTokens != 2400 || session.TokenStats.OutputTokens != 140 {
		t.Fatalf("TokenStats = %+v, 期望 in=700 cache=2400 out=140", session.TokenStats)
	}
	if session.TokenStats.TotalInputTokens() != 3100 {
		t.Fatalf("TotalInputTokens = %d, 期望 3100", session.TokenStats.TotalInputTokens())
	}
}

func TestListAgySessionsExcludesSubagentAndFiltersDate(t *testing.T) {
	agyDir := writeAgyFixture(t, []agyFixtureSession{
		{id: "aaaa-main", workingDir: "/tmp/a", lines: agySampleLines()},
		{id: "bbbb-child", parentID: "aaaa-main", workingDir: "/tmp/a", lines: agySampleLines()},
		{id: "cccc-empty", workingDir: "/tmp/c"},
	})

	day := time.Date(2026, 11, 21, 0, 0, 0, 0, testLocation)
	sessions, err := listAgySessions(agyDir, testLocation, &day)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].SessionID != "aaaa-main" {
		t.Fatalf("listAgySessions = %+v, 期望仅包含 aaaa-main", sessions)
	}

	otherDay := time.Date(2026, 11, 20, 0, 0, 0, 0, testLocation)
	empty, err := listAgySessions(agyDir, testLocation, &otherDay)
	if err != nil {
		t.Fatal(err)
	}
	if len(empty) != 0 {
		t.Fatalf("其他日期会话数 = %d, 期望 0", len(empty))
	}
}

func TestExtractTranscriptAgy(t *testing.T) {
	agyDir := writeAgyFixture(t, []agyFixtureSession{
		{id: "40f69db6-364c-4692-8d48-5806402324df", lines: agySampleLines()},
	})
	transcriptPath := filepath.Join(agyDir, "brain", "40f69db6-364c-4692-8d48-5806402324df", ".system_generated", "logs", "transcript_full.jsonl")

	messages, err := extractTranscript(&SessionData{Source: sourceAgy, Path: transcriptPath})
	if err != nil {
		t.Fatal(err)
	}
	want := []transcriptMessage{
		{Role: "user", Content: "分析这个仓库"},
		{Role: "assistant", Content: "这是一个演示仓库。"},
		{Role: "user", Content: "继续优化"},
		{Role: "assistant", Content: "已优化完成。"},
	}
	if len(messages) != len(want) {
		t.Fatalf("消息数 = %d, 期望 %d：%+v", len(messages), len(want), messages)
	}
	for index, item := range want {
		if messages[index] != item {
			t.Fatalf("第 %d 条 = %+v, 期望 %+v", index, messages[index], item)
		}
	}
}

func TestDetectSourceAgy(t *testing.T) {
	agyDir := writeAgyFixture(t, []agyFixtureSession{
		{id: "40f69db6-364c-4692-8d48-5806402324df", lines: agySampleLines()},
	})
	opts := &options{
		agyDir:        agyDir,
		claudeDir:     t.TempDir(),
		qoderDir:      t.TempDir(),
		qoderAppDir:   t.TempDir(),
		zcodeDatabase: filepath.Join(t.TempDir(), "missing.sqlite"),
	}

	if got := detectSource("40f69db6", opts); got != sourceAgy {
		t.Fatalf("detectSource(prefix) = %s, 期望 %s", got, sourceAgy)
	}

	transcriptPath := filepath.Join(agyDir, "brain", "40f69db6-364c-4692-8d48-5806402324df", ".system_generated", "logs", "transcript_full.jsonl")
	if got := detectSource(transcriptPath, opts); got != sourceAgy {
		t.Fatalf("detectSource(path) = %s, 期望 %s", got, sourceAgy)
	}
}
