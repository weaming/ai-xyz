package main

import (
	"strings"
	"testing"
	"time"
)

func TestSessionStatRow(t *testing.T) {
	startedAt := time.Date(2026, 8, 25, 5, 16, 0, 0, testLocation)
	endedAt := time.Date(2026, 8, 25, 5, 44, 0, 0, testLocation)
	session := &SessionData{
		Source:    sourceClaude,
		SessionID: "ses_test",
		StartedAt: &startedAt,
		EndedAt:   &endedAt,
		Turns:     []ConversationTurn{{}, {}, {}},
		Models:    []string{"model-a", "model-b"},
		TokenStats: TokenUsage{
			InputTokens:     1000,
			OutputTokens:    400,
			CacheHitTokens:  2500,
			CacheMissTokens: 500,
		},
	}
	row := sessionStatRow(session, testLocation)
	if len(row) != len(statColumns) {
		t.Fatalf("列数 = %d, 期望 %d", len(row), len(statColumns))
	}
	if row[0] != sourceClaude || row[1] != "ses_test" {
		t.Fatalf("SOURCE/ID = %s/%s", row[0], row[1])
	}
	if row[2] != "08-25 05:16" || row[3] != "08-25 05:44" {
		t.Fatalf("START/END = %q/%q", row[2], row[3])
	}
	if row[4] != "3" {
		t.Fatalf("TURNS = %q", row[4])
	}
	if row[5] != "model-a+model-b" {
		t.Fatalf("MODELS = %q", row[5])
	}
	if row[6] != "4.0k" || row[7] != "400" {
		t.Fatalf("TOKENS_IN/OUT = %q/%q", row[6], row[7])
	}
	if row[8] != "2.5k" || row[9] != "62.50%" {
		t.Fatalf("CACHE_HIT/RATE = %q/%q", row[8], row[9])
	}
	for _, cell := range row {
		if strings.Contains(cell, ",") {
			t.Fatalf("单元格不应含逗号：%q", cell)
		}
	}

	// 无时间数据时占位
	empty := &SessionData{Source: sourceQoderApp}
	if row = sessionStatRow(empty, testLocation); row[2] != "（无）" || row[3] != "（无）" {
		t.Fatalf("缺时间时 START/END = %q/%q", row[2], row[3])
	}
}
