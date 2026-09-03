package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExtractTranscriptClaude(t *testing.T) {
	dir := t.TempDir()
	sessionPath := filepath.Join(dir, "ses_t.jsonl")
	lines := []string{
		`{"type":"user","timestamp":"2026-07-24T15:59:00+08:00","message":{"role":"user","content":"<AGENTS.md>\n指令内容\n</AGENTS.md>"}}`,
		`{"type":"user","timestamp":"2026-07-24T16:00:00+08:00","message":{"role":"user","content":[{"type":"text","text":"第一个问题"}]}}`,
		`{"type":"assistant","timestamp":"2026-07-24T16:01:00+08:00","message":{"role":"assistant","content":[{"type":"text","text":"回答一"},{"type":"tool_use","name":"Read","input":{}}]}}`,
		`{"type":"user","timestamp":"2026-07-24T16:02:00+08:00","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"x","content":"文件内容"}]}}`,
		`{"type":"assistant","timestamp":"2026-07-24T16:03:00+08:00","message":{"role":"assistant","content":[{"type":"thinking","thinking":"内部思考"},{"type":"text","text":"回答二"}]}}`,
		`{"type":"user","timestamp":"2026-07-24T16:04:00+08:00","message":{"role":"user","content":"第二个问题（纯字符串）"}}`,
	}
	if err := os.WriteFile(sessionPath, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}

	session := &SessionData{Source: sourceClaude, Path: sessionPath}
	messages, err := extractTranscript(session)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 4 {
		t.Fatalf("消息数 = %d, 期望 4：%+v", len(messages), messages)
	}
	want := []transcriptMessage{
		{Role: "user", Content: "第一个问题"},
		{Role: "assistant", Content: "回答一"},
		{Role: "assistant", Content: "回答二"},
		{Role: "user", Content: "第二个问题（纯字符串）"},
	}
	for index, message := range want {
		if messages[index] != message {
			t.Fatalf("第 %d 条 = %+v, 期望 %+v", index, messages[index], message)
		}
	}
}

func TestExtractTranscriptCodex(t *testing.T) {
	dir := t.TempDir()
	rolloutPath := filepath.Join(dir, "rollout-thread-1.jsonl")
	lines := []string{
		`{"type":"session_meta","payload":{"session_id":"thread-1"}}`,
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<INSTRUCTIONS>指令</INSTRUCTIONS>\n<environment_context>cwd</environment_context>"}]}}`,
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"问题一"}]}}`,
		`{"type":"event_msg","payload":{"type":"token_count","info":{}}}`,
		`{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"回答一"},{"type":"output_text","text":"补充"}]}}`,
		`{"type":"response_item","payload":{"type":"message","role":"developer","content":[{"type":"input_text","text":"系统提示"}]}}`,
		`{"type":"response_item","payload":{"type":"reasoning","summary":["内部思考"]}}`,
		`{"type":"event_msg","payload":{"type":"task_complete"}}`,
	}
	if err := os.WriteFile(rolloutPath, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}

	session := &SessionData{Source: sourceCodex, Path: rolloutPath}
	messages, err := extractTranscript(session)
	if err != nil {
		t.Fatal(err)
	}
	want := []transcriptMessage{
		{Role: "user", Content: "问题一"},
		{Role: "assistant", Content: "回答一\n补充"},
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

func TestExtractTranscriptQoderApp(t *testing.T) {
	dir := t.TempDir()
	sessionPath := filepath.Join(dir, "task.jsonl")
	lines := []string{
		`{"role":"user","message":{"content":[{"type":"text","text":"<user_query>\n提问\n</user_query>"}]}}`,
		`{"role":"user","message":{"content":[{"type":"text","text":"<user_query>\n提问\n</user_query>"}]}}`,
		`{"role":"user","message":{"content":[{"type":"text","text":"<system-reminder>\n时间提醒\n</system-reminder>\n\n<user_query>\n追问\n</user_query>"}]}}`,
		`{"role":"assistant","message":{"content":[{"type":"text","text":"答复"}]}}`,
	}
	if err := os.WriteFile(sessionPath, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}

	session := &SessionData{Source: sourceQoderApp, Path: sessionPath}
	messages, err := extractTranscript(session)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 3 {
		t.Fatalf("消息数 = %d, 期望 3：%+v", len(messages), messages)
	}
	if messages[0].Role != "user" || messages[0].Content != "提问" {
		t.Fatalf("第 1 条 = %+v", messages[0])
	}
	if messages[1].Content != "追问" {
		t.Fatalf("第 2 条 = %+v", messages[1])
	}
	if messages[2].Content != "答复" {
		t.Fatalf("第 3 条 = %+v", messages[2])
	}
}

func TestExtractTranscriptClaudeLegacy(t *testing.T) {
	dir := t.TempDir()
	sessionPath := filepath.Join(dir, "ses_legacy.jsonl")
	lines := []string{
		`{"type":"user","timestamp":"2026-01-22T15:06:54.805Z","content":"问题（旧格式）"}`,
		`{"type":"tool_use","timestamp":"2026-01-22T15:07:00.825Z","tool_name":"bash","tool_input":{"command":"ls"}}`,
		`{"type":"tool_result","timestamp":"2026-01-22T15:07:00.851Z","tool_name":"bash","tool_output":{"output":"total 8"}}`,
		`{"type":"assistant","timestamp":"2026-01-22T15:08:00.000Z","content":"回答（旧格式）"}`,
	}
	if err := os.WriteFile(sessionPath, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}

	session := &SessionData{Source: sourceClaude, Path: sessionPath}
	messages, err := extractTranscript(session)
	if err != nil {
		t.Fatal(err)
	}
	want := []transcriptMessage{
		{Role: "user", Content: "问题（旧格式）"},
		{Role: "assistant", Content: "回答（旧格式）"},
	}
	if len(messages) != len(want) || messages[0] != want[0] || messages[1] != want[1] {
		t.Fatalf("提取结果 = %+v, 期望 %+v", messages, want)
	}
}

func TestRenderTranscript(t *testing.T) {
	messages := []transcriptMessage{
		{Role: "user", Content: "问题一"},
		{Role: "assistant", Content: "回答一"},
	}
	jsonl := renderTranscript(messages, formatJSONL)
	wantJSONL := `{"role":"user","content":"问题一"}` + "\n" + `{"role":"assistant","content":"回答一"}` + "\n"
	if jsonl != wantJSONL {
		t.Fatalf("JSONL = %q, 期望 %q", jsonl, wantJSONL)
	}
	md := renderTranscript(messages, formatMD)
	wantMD := "## user\n\n问题一\n\n## assistant\n\n回答一\n\n"
	if md != wantMD {
		t.Fatalf("MD = %q, 期望 %q", md, wantMD)
	}
}
