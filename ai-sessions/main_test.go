package main

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var testLocation = time.FixedZone("Asia/Shanghai", 8*3600)

func TestParseTimestamp(t *testing.T) {
	cases := []struct {
		name  string
		value any
		want  string
	}{
		{"unix毫秒", float64(1784880000000), "2026-07-24 16:00:00"},
		{"带偏移", "2026-07-24T16:00:00+08:00", "2026-07-24 16:00:00"},
		{"Z结尾", "2026-07-24T08:00:00Z", "2026-07-24 16:00:00"},
		{"无时区", "2026-07-24 16:00:00", "2026-07-24 16:00:00"},
		{"空格分隔", "2026-07-24 16:00:00", "2026-07-24 16:00:00"},
		{"带小数", "2026-07-24T16:00:00.123+08:00", "2026-07-24 16:00:00"},
		{"空字符串", "", ""},
		{"非法格式", "not-a-time", ""},
		{"其他类型", true, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseTimestamp(tc.value, testLocation)
			if tc.want == "" {
				if got != nil {
					t.Fatalf("parseTimestamp(%v) = %v, 期望 nil", tc.value, got)
				}
				return
			}
			if got == nil {
				t.Fatalf("parseTimestamp(%v) = nil, 期望 %s", tc.value, tc.want)
			}
			if formatted := got.Format("2006-01-02 15:04:05"); formatted != tc.want {
				t.Fatalf("parseTimestamp(%v) = %s, 期望 %s", tc.value, formatted, tc.want)
			}
		})
	}
}

func TestGetText(t *testing.T) {
	cases := []struct {
		name  string
		value any
		want  string
	}{
		{"字符串", "hello", "hello"},
		{"文本块", []any{map[string]any{"type": "text", "text": "a"}, map[string]any{"type": "text", "text": "b"}}, "a\nb"},
		{"混合块", []any{"plain", map[string]any{"type": "tool_use", "name": "x"}}, "plain"},
		{"空列表", []any{}, ""},
		{"其他类型", 42, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := getText(tc.value); got != tc.want {
				t.Fatalf("getText(%v) = %q, 期望 %q", tc.value, got, tc.want)
			}
		})
	}
}

func TestGetToolNames(t *testing.T) {
	item := map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"content": []any{
				map[string]any{"type": "tool_use", "name": "Read"},
				map[string]any{"type": "tool_use", "name": "Read"},
				map[string]any{"type": "tool_use", "tool_name": "Bash"},
			},
		},
	}
	got := getToolNames(item)
	want := []string{"Read", "Read", "Bash"}
	if len(got) != len(want) {
		t.Fatalf("getToolNames() = %v, 期望 %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("getToolNames() = %v, 期望 %v", got, want)
		}
	}
}

func TestRefreshSummary(t *testing.T) {
	session := &SessionData{
		Turns: []ConversationTurn{
			{Question: "第一个问题", Tools: []string{"Read", "Read"}, Answer: "   "},
			{},
			{Question: "", Tools: []string{"Bash"}, Answer: "最终回答\n多行"},
			{Answer: "上一个回答"},
		},
	}
	session.refreshSummary()
	if len(session.Turns) != 3 {
		t.Fatalf("过滤后轮次数 = %d, 期望 3", len(session.Turns))
	}
	if len(session.Inputs) != 1 || session.Inputs[0] != "第一个问题" {
		t.Fatalf("Inputs = %v, 期望 [第一个问题]", session.Inputs)
	}
	if len(session.Tools) != 2 || session.Tools[0] != "Read" || session.Tools[1] != "Bash" {
		t.Fatalf("Tools = %v, 期望 [Read Bash]", session.Tools)
	}
	if session.FinalOutput != "上一个回答" {
		t.Fatalf("FinalOutput = %q, 期望 上一个回答", session.FinalOutput)
	}
}

func TestMatchesQuery(t *testing.T) {
	session := &SessionData{
		Inputs: []string{"fix login bug"},
		Turns:  []ConversationTurn{{Question: "fix login bug", Answer: "修复登录模块"}},
	}
	cases := []struct {
		query string
		want  bool
	}{
		{"", true},
		{"login", true},
		{"LOGIN", true},
		{"登录模块", true},
		{"不存在", false},
	}
	for _, tc := range cases {
		if got := session.matchesQuery(tc.query); got != tc.want {
			t.Fatalf("matchesQuery(%q) = %v, 期望 %v", tc.query, got, tc.want)
		}
	}
}

func TestParseJSONLSession(t *testing.T) {
	dir := t.TempDir()
	sessionPath := filepath.Join(dir, "ses_test123.jsonl")
	lines := []string{
		`{"type":"user","timestamp":"2026-07-24T16:00:00+08:00","cwd":"/tmp/work","slug":"plan-abc","message":{"role":"user","content":"第一个问题"}}`,
		`{"type":"assistant","timestamp":"2026-07-24T16:01:00+08:00","message":{"role":"assistant","content":[{"type":"tool_use","name":"Read"},{"type":"tool_use","name":"Bash"}]}}`,
		`{"type":"user","timestamp":"2026-07-24T16:02:00+08:00","message":{"role":"user","content":"<command-name>/clear</command-name>"}}`,
		`{"type":"user","timestamp":"2026-07-24T16:03:00+08:00","message":{"role":"user","content":"第二个问题"}}`,
		`{"type":"assistant","timestamp":"2026-07-24T16:04:00+08:00","message":{"role":"assistant","content":[{"type":"text","text":"最终回答内容"}]}}`,
	}
	if err := os.WriteFile(sessionPath, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}

	session, err := parseJSONLSession("claude", sessionPath, testLocation, false, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if session.SessionID != "ses_test123" {
		t.Fatalf("SessionID = %s", session.SessionID)
	}
	if session.WorkingDir != "/tmp/work" {
		t.Fatalf("WorkingDir = %s", session.WorkingDir)
	}
	if session.PlanSlug != "plan-abc" {
		t.Fatalf("PlanSlug = %s", session.PlanSlug)
	}
	if len(session.Inputs) != 2 || session.Inputs[1] != "第二个问题" {
		t.Fatalf("Inputs = %v", session.Inputs)
	}
	// 内部命令 /clear 被过滤，不会成为独立轮次
	if len(session.Turns) != 2 {
		t.Fatalf("Turns 数 = %d, 期望 2", len(session.Turns))
	}
	if len(session.Tools) != 2 || session.Tools[0] != "Read" || session.Tools[1] != "Bash" {
		t.Fatalf("Tools = %v", session.Tools)
	}
	if session.FinalOutput != "最终回答内容" {
		t.Fatalf("FinalOutput = %q", session.FinalOutput)
	}
	if session.StartedAt == nil || session.StartedAt.Format("15:04") != "16:00" {
		t.Fatalf("StartedAt = %v", session.StartedAt)
	}
	if session.EndedAt == nil || session.EndedAt.Format("15:04") != "16:04" {
		t.Fatalf("EndedAt = %v", session.EndedAt)
	}
}

func TestParseJSONLSessionEmpty(t *testing.T) {
	dir := t.TempDir()
	sessionPath := filepath.Join(dir, "ses_empty.jsonl")
	if err := os.WriteFile(sessionPath, []byte(`{"type":"user","message":{"role":"user","content":"<command-name>/clear</command-name>"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := parseJSONLSession("claude", sessionPath, testLocation, false, false, false); err == nil {
		t.Fatal("空会话应返回错误")
	}
	if _, err := parseJSONLSession("claude", sessionPath, testLocation, true, false, false); err != nil {
		t.Fatalf("allowEmpty 时不应报错：%v", err)
	}
}

func TestShortenToTerminalWidth(t *testing.T) {
	// 前缀 Q12: 宽度 5+1，终端 20 列，可用 14
	got := shortenToTerminalWidth("这是一段非常长的中文文本用来测试截断功能", "Q12: ", 20)
	if len(got) == 0 || !strings.HasSuffix(got, "…") {
		t.Fatalf("截断结果缺少省略号：%q", got)
	}
	if displayWidth(got) > 19 {
		t.Fatalf("截断后宽度 %d 超出终端宽度", displayWidth(got))
	}
	short := shortenToTerminalWidth("短文本", "Q: ", 80)
	if short != "短文本" {
		t.Fatalf("短文本不应截断：%q", short)
	}
	empty := shortenToTerminalWidth("", "Q: ", 80)
	if empty != "（无）" {
		t.Fatalf("空文本应输出（无）：%q", empty)
	}
}

func TestDisplayWidth(t *testing.T) {
	cases := []struct {
		text string
		want int
	}{
		{"hello", 5},
		{"中文", 4},
		{"a中文b", 6},
		{"…", 1},
	}
	for _, tc := range cases {
		if got := displayWidth(tc.text); got != tc.want {
			t.Fatalf("displayWidth(%q) = %d, 期望 %d", tc.text, got, tc.want)
		}
	}
}

func TestGetDateTimestampBounds(t *testing.T) {
	day := time.Date(2026, 7, 24, 0, 0, 0, 0, testLocation)
	start, end := getDateTimestampBounds(day, testLocation)
	if start != time.Date(2026, 7, 24, 0, 0, 0, 0, testLocation).UnixMilli() {
		t.Fatalf("start = %d", start)
	}
	if end != time.Date(2026, 7, 25, 0, 0, 0, 0, testLocation).UnixMilli() {
		t.Fatalf("end = %d", end)
	}
}

func TestParseJSONLSessionToolDetails(t *testing.T) {
	dir := t.TempDir()
	sessionPath := filepath.Join(dir, "ses_tools.jsonl")
	lines := []string{
		`{"type":"user","message":{"role":"user","content":"问题一"}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"Read","input":{"file_path":"/a.go"}},{"type":"tool_use","id":"call_2","name":"Bash","input":{"command":"ls"}}]}}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":"文件内容"},{"type":"tool_result","tool_use_id":"call_2","content":[{"type":"text","text":"输出行"}]}]}}`,
		`{"type":"tool_use","tool_name":"Glob","input":{"pattern":"*.go"}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"回答一"}]}}`,
	}
	if err := os.WriteFile(sessionPath, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}

	session, err := parseJSONLSession("claude", sessionPath, testLocation, false, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(session.Turns) != 1 {
		t.Fatalf("Turns 数 = %d, 期望 1", len(session.Turns))
	}
	calls := session.Turns[0].ToolCalls
	if len(calls) != 3 {
		t.Fatalf("ToolCalls 数 = %d, 期望 3", len(calls))
	}
	if calls[0].Name != "Read" || calls[0].Input != `{"file_path":"/a.go"}` || calls[0].Output != "文件内容" {
		t.Fatalf("Read 调用 = %+v", calls[0])
	}
	if calls[1].Name != "Bash" || calls[1].Input != `{"command":"ls"}` || calls[1].Output != "输出行" {
		t.Fatalf("Bash 调用 = %+v", calls[1])
	}
	if calls[2].Name != "Glob" || calls[2].Input != `{"pattern":"*.go"}` {
		t.Fatalf("Glob 调用 = %+v", calls[2])
	}

	// 不采集详情时不记录工具调用输入输出
	session, err = parseJSONLSession("claude", sessionPath, testLocation, false, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(session.Turns[0].ToolCalls) != 0 {
		t.Fatalf("未采集详情时 ToolCalls 应为空：%+v", session.Turns[0].ToolCalls)
	}
	if len(session.Turns[0].Tools) != 3 {
		t.Fatalf("Tools = %v", session.Turns[0].Tools)
	}
}

func TestParseJSONLSessionThinking(t *testing.T) {
	dir := t.TempDir()
	sessionPath := filepath.Join(dir, "ses_thinking.jsonl")
	lines := []string{
		`{"type":"user","message":{"role":"user","content":"问题一"}}`,
		`{"type":"assistant","message":{"role":"assistant","reasoning_content":"先确认上下文","reasoning_details":[{"type":"reasoning.text","text":"再检查边界条件"}],"content":[{"type":"thinking","thinking":"最后验证结果"},{"type":"text","text":"回答一"}]}}`,
	}
	if err := os.WriteFile(sessionPath, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}

	session, err := parseJSONLSession("claude", sessionPath, testLocation, false, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if got := session.Turns[0].Thinking; got != "先确认上下文\n再检查边界条件\n最后验证结果" {
		t.Fatalf("Thinking = %q", got)
	}
	if session.Turns[0].Answer != "回答一" {
		t.Fatalf("Answer = %q", session.Turns[0].Answer)
	}

	session, err = parseJSONLSession("claude", sessionPath, testLocation, false, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if session.Turns[0].Thinking != "" {
		t.Fatalf("未启用思考提取时 Thinking 应为空：%q", session.Turns[0].Thinking)
	}
}

func TestParseCodexThinking(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "history.sqlite")
	db, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.Exec(`CREATE TABLE thread_items (
		thread_id TEXT,
		item_type TEXT,
		item_json TEXT,
		created_at_ms INTEGER,
		turn_id TEXT,
		rollout_ordinal INTEGER
	)`)
	if err != nil {
		t.Fatal(err)
	}
	items := []struct {
		itemType string
		itemJSON string
		ordinal  int
	}{
		{"userMessage", `{"content":"问题一"}`, 1},
		{"reasoning", `{"type":"reasoning","text":"检查实现"}`, 2},
		{"reasoning", `{"type":"reasoning","summary":["确认边界","完成验证"]}`, 3},
		{"agentMessage", `{"text":"回答一","phase":"final_answer"}`, 4},
	}
	for _, item := range items {
		if _, err := db.Exec(
			"INSERT INTO thread_items (thread_id, item_type, item_json, created_at_ms, turn_id, rollout_ordinal) VALUES (?, ?, ?, ?, ?, ?)",
			"thread-1", item.itemType, item.itemJSON, 1784880000000, "turn-1", item.ordinal,
		); err != nil {
			t.Fatal(err)
		}
	}
	rolloutDirectory := filepath.Join(filepath.Dir(databasePath), "sessions", "2026", "08", "23")
	if err := os.MkdirAll(rolloutDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	rolloutPath := filepath.Join(rolloutDirectory, "rollout-2026-08-23T00-00-00-thread-1.jsonl")
	rollout := strings.Join([]string{
		`{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":1000,"cached_input_tokens":600,"cache_write_input_tokens":50,"output_tokens":100},"last_token_usage":{"input_tokens":1000,"cached_input_tokens":600,"cache_write_input_tokens":50,"output_tokens":100}}}}`,
		`{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":3000,"cached_input_tokens":2500,"cache_write_input_tokens":100,"output_tokens":400},"last_token_usage":{"input_tokens":3000,"cached_input_tokens":2500,"cache_write_input_tokens":100,"output_tokens":400}}}}`,
	}, "\n")
	if err := os.WriteFile(rolloutPath, []byte(rollout), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	session, err := parseCodex("thread-1", databasePath, testLocation, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if got := session.Turns[0].Thinking; got != "检查实现\n确认边界\n完成验证" {
		t.Fatalf("Thinking = %q", got)
	}
	if got := session.TokenStats; got.InputTokens != 400 || got.OutputTokens != 400 || got.CacheHitTokens != 2500 || got.CacheMissTokens != 100 {
		t.Fatalf("TokenStats = %+v", got)
	}
	if got := session.TokenStats.TotalInputTokens(); got != 3000 {
		t.Fatalf("TotalInputTokens = %d, 期望 3000", got)
	}
	if len(session.RequestHitRates) != 2 {
		t.Fatalf("RequestHitRates = %v, 期望 2 条", session.RequestHitRates)
	}
}

func TestOptionsValidate(t *testing.T) {
	cases := []struct {
		name  string
		opts  options
		isErr bool
	}{
		{"默认", options{source: "all"}, false},
		{"无效来源", options{source: "gemini"}, true},
		{"问题序号为负", options{source: "all", turn: -1}, true},
		{"问题序号无会话", options{source: "all", turn: 2}, true},
		{"问题序号与会话", options{source: "all", session: "abc", turn: 2}, false},
		{"问题序号与查询冲突", options{source: "all", session: "abc", turn: 2, query: "x"}, true},
		{"查询与会话", options{source: "all", session: "abc", query: "x"}, false},
		{"思考无会话", options{source: "all", think: true}, true},
		{"思考与会话", options{source: "all", session: "abc", think: true}, false},
	}
	for _, tc := range cases {
		err := tc.opts.validate()
		if tc.isErr && err == nil {
			t.Fatalf("%s：期望报错", tc.name)
		}
		if !tc.isErr && err != nil {
			t.Fatalf("%s：%v", tc.name, err)
		}
	}
}

func TestGetLastAnswerIndex(t *testing.T) {
	session := &SessionData{
		Turns: []ConversationTurn{
			{Question: "q1", Answer: "a1"},
			{Question: "q2"},
			{Question: "q3", Answer: "a3"},
		},
	}
	if got := session.getLastAnswerIndex(); got != 3 {
		t.Fatalf("getLastAnswerIndex() = %d, 期望 3", got)
	}
	noAnswer := &SessionData{Turns: []ConversationTurn{{Question: "q"}}}
	if got := noAnswer.getLastAnswerIndex(); got != 1 {
		t.Fatalf("无回答时 getLastAnswerIndex() = %d, 期望 1", got)
	}
}

func TestTurnDurationStats(t *testing.T) {
	dir := t.TempDir()
	sessionPath := filepath.Join(dir, "ses_duration.jsonl")
	// 第一轮 10:00:00~10:01:30（90s）；用户隔 10 分钟才提第二问，
	// 这段空闲不应计入任何一轮；第二轮 10:11:30~10:11:50（20s）。
	lines := []string{
		`{"type":"user","timestamp":"2026-07-24T10:00:00+08:00","message":{"role":"user","content":"问题一"}}`,
		`{"type":"assistant","timestamp":"2026-07-24T10:01:30+08:00","message":{"role":"assistant","content":"回答一"}}`,
		`{"type":"user","timestamp":"2026-07-24T10:11:30+08:00","message":{"role":"user","content":"问题二"}}`,
		`{"type":"assistant","timestamp":"2026-07-24T10:11:50+08:00","message":{"role":"assistant","content":"回答二"}}`,
	}
	if err := os.WriteFile(sessionPath, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}

	session, err := parseJSONLSession(sourceClaude, sessionPath, testLocation, false, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(session.Turns) != 2 {
		t.Fatalf("Turns 数 = %d, 期望 2", len(session.Turns))
	}
	first, second := session.Turns[0].Duration(), session.Turns[1].Duration()
	if first == nil || *first != 90*time.Second {
		t.Fatalf("第一轮用时 = %v, 期望 90s（轮间空闲不应计入）", first)
	}
	if second == nil || *second != 20*time.Second {
		t.Fatalf("第二轮用时 = %v, 期望 20s", second)
	}

	summary, ok := session.TurnDurationStats()
	if !ok {
		t.Fatal("应能统计出轮次用时")
	}
	if summary.Count != 2 || summary.Total != 110*time.Second || summary.Avg != 55*time.Second {
		t.Fatalf("Count/Total/Avg = %d/%v/%v", summary.Count, summary.Total, summary.Avg)
	}
	if summary.Min != 20*time.Second || summary.MinTurn != 2 {
		t.Fatalf("Min = %v (Q%d), 期望 20s (Q2)", summary.Min, summary.MinTurn)
	}
	if summary.Max != 90*time.Second || summary.MaxTurn != 1 {
		t.Fatalf("Max = %v (Q%d), 期望 90s (Q1)", summary.Max, summary.MaxTurn)
	}
	if summary.Median != 55*time.Second {
		t.Fatalf("Median = %v, 期望 55s", summary.Median)
	}

	// 无时间数据的会话不应输出统计。
	if _, ok := (&SessionData{Turns: []ConversationTurn{{Question: "q"}}}).TurnDurationStats(); ok {
		t.Fatal("无时间数据时不应有统计")
	}
}

func TestFormatDuration(t *testing.T) {
	cases := []struct {
		value time.Duration
		want  string
	}{
		{500 * time.Millisecond, "<1s"},
		{45 * time.Second, "45s"},
		{125 * time.Second, "2m05s"},
		{3725 * time.Second, "1h02m"},
	}
	for _, tc := range cases {
		if got := formatDuration(tc.value); got != tc.want {
			t.Fatalf("formatDuration(%v) = %s, 期望 %s", tc.value, got, tc.want)
		}
	}
}
