package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeQoderAppFixture 在临时目录中创建 <项目>/conversation-history/<task>/<task>.jsonl。
func writeQoderAppFixture(t *testing.T, project, taskID string, lines []string) string {
	t.Helper()
	taskDirectory := filepath.Join(t.TempDir(), project, "conversation-history", taskID)
	if err := os.MkdirAll(taskDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	sessionPath := filepath.Join(taskDirectory, taskID+".jsonl")
	if err := os.WriteFile(sessionPath, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	return sessionPath
}

func TestExtractQoderAppQuestion(t *testing.T) {
	cases := []struct {
		name  string
		value string
		want  string
	}{
		{"user_query标签", "<user_query>\n问题一\n</user_query><system_reminder>\n提示\n</system_reminder>", "问题一"},
		{"带系统提醒", "<system-reminder>\n时间提醒\n</system-reminder>\n\n\n\n<user_query>\n问题二</user_query>", "问题二"},
		{"多个标签", "<user_query>甲</user_query><user_query>乙</user_query>", "甲\n乙"},
		{"无标签去提醒", "<system-reminder>\n提醒\n</system-reminder>\n直接提问", "直接提问"},
		{"纯文本", "直接提问", "直接提问"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractQoderAppQuestion(tc.value); got != tc.want {
				t.Fatalf("extractQoderAppQuestion = %q, 期望 %q", got, tc.want)
			}
		})
	}
}

func TestParseQoderAppSession(t *testing.T) {
	lines := []string{
		`{"role":"user","message":{"content":[{"type":"text","text":"<user_query>\n问题一\n</user_query>"}]}}`,
		`{"role":"user","message":{"content":[{"type":"text","text":"<user_query>\n问题一\n</user_query>"}]}}`,
		`{"role":"assistant","message":{"content":[{"type":"text","text":"中间进展"}]}}`,
		`{"role":"assistant","message":{"content":[{"type":"text","text":"回答一"}]}}`,
		`{"role":"user","message":{"content":[{"type":"text","text":"<system-reminder>\n时间提醒</system-reminder>\n\n<user_query>\n问题二</user_query>"}]}}`,
		`{"role":"assistant","message":{"content":[{"type":"text","text":"回答二"}]}}`,
	}
	sessionPath := writeQoderAppFixture(t, "my-project", "task-ab", lines)

	session, err := parseQoderAppSession(sessionPath, testLocation, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if session.Source != "qoder-app" || session.SessionID != "my-project/task-ab" {
		t.Fatalf("Source/SessionID = %s/%s", session.Source, session.SessionID)
	}
	if session.StartedAt == nil || session.EndedAt == nil {
		t.Fatal("时间应取自文件修改时间")
	}
	if len(session.Turns) != 2 {
		t.Fatalf("Turns 数 = %d, 期望 2（重复行应去重）", len(session.Turns))
	}
	if session.Turns[0].Question != "问题一" {
		t.Fatalf("Q1 = %q", session.Turns[0].Question)
	}
	if session.Turns[0].Answer != "回答一" {
		t.Fatalf("A1 = %q（应取最后一条助手消息）", session.Turns[0].Answer)
	}
	if session.Turns[1].Question != "问题二" || session.Turns[1].Answer != "回答二" {
		t.Fatalf("第二轮 = %+v", session.Turns[1])
	}
	if session.FinalOutput != "回答二" {
		t.Fatalf("FinalOutput = %q", session.FinalOutput)
	}

	// 启用思考提取时，中间助手消息应进入 Thinking
	session, err = parseQoderAppSession(sessionPath, testLocation, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if session.Turns[0].Thinking != "中间进展" {
		t.Fatalf("Thinking = %q", session.Turns[0].Thinking)
	}
	if session.Turns[0].Answer != "回答一" {
		t.Fatalf("启用思考后 A1 = %q", session.Turns[0].Answer)
	}
}

func TestResolveQoderAppSession(t *testing.T) {
	lines := []string{
		`{"role":"user","message":{"content":[{"type":"text","text":"<user_query>问题</user_query>"}]}}`,
		`{"role":"assistant","message":{"content":[{"type":"text","text":"回答"}]}}`,
	}
	sessionPath := writeQoderAppFixture(t, "my-project", "task-ab", lines)
	rootDirectory := filepath.Dir(filepath.Dir(filepath.Dir(sessionPath)))

	cases := []struct {
		name      string
		sessionID string
	}{
		{"完整文件路径", sessionPath},
		{"完整会话ID", "my-project/task-ab"},
		{"task ID", "task-ab"},
		{"唯一前缀", "task-a"},
		{"项目前缀", "my-project"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resolved, err := resolveQoderAppSession(tc.sessionID, rootDirectory)
			if err != nil {
				t.Fatalf("resolveQoderAppSession(%s) 报错：%v", tc.sessionID, err)
			}
			if resolved != sessionPath {
				t.Fatalf("resolved = %s, 期望 %s", resolved, sessionPath)
			}
		})
	}

	if _, err := resolveQoderAppSession("task-none", rootDirectory); err == nil {
		t.Fatal("不存在的会话应报错")
	}
}
