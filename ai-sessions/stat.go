package main

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	column "github.com/weaming/printable/go-column"
)

const (
	formatJSONL = "jsonl"
	formatTable = "table"
	formatCSV   = "csv"
	formatMD    = "md"
)

// statColumns 定义 -stat 的元信息列。
var statColumns = []string{"SOURCE", "ID", "START_TIME", "END_TIME", "TURNS", "MODELS", "TOKENS_IN", "TOKENS_OUT", "CACHE_HIT", "CACHE_HIT%"}

// sessionStatRow 生成一个会话的元信息行，单元格不含逗号以便表格排版。
func sessionStatRow(session *SessionData, loc *time.Location) []string {
	startText := "（无）"
	if session.StartedAt != nil {
		startText = session.StartedAt.Format("01-02 15:04")
	}
	endText := "（无）"
	if session.EndedAt != nil {
		endText = session.EndedAt.Format("01-02 15:04")
	}
	inputTokens := ""
	outputTokens := ""
	cacheHit := ""
	cacheHitRate := ""
	if session.TokenStats.HasData() {
		inputTokens = formatTokenCount(session.TokenStats.TotalInputTokens())
		outputTokens = formatTokenCount(session.TokenStats.OutputTokens)
		cacheHit = formatTokenCount(session.TokenStats.CacheHitTokens)
		cacheHitRate = formatPercent(session.TokenStats.CacheHitRate())
	}
	return []string{
		session.Source,
		session.SessionID,
		startText,
		endText,
		strconv.Itoa(len(session.Turns)),
		strings.Join(session.Models, "+"),
		inputTokens,
		outputTokens,
		cacheHit,
		cacheHitRate,
	}
}

// printSessionStats 以表格或 CSV 输出会话元信息。
func printSessionStats(sessions []*SessionData, loc *time.Location, format string) error {
	var rows [][]string
	for _, session := range sessions {
		rows = append(rows, sessionStatRow(session, loc))
	}
	if format == formatCSV {
		writer := csv.NewWriter(os.Stdout)
		if err := writer.WriteAll(append([][]string{statColumns}, rows...)); err != nil {
			return newHistoryError("输出 CSV 失败：%v", err)
		}
		return nil
	}
	var buffer bytes.Buffer
	for _, row := range rows {
		buffer.WriteString(strings.Join(row, ","))
		buffer.WriteString("\n")
	}
	columns := strings.Join(statColumns, ",")
	separator := ","
	output, err := column.Render(buffer.Bytes(), []column.ColumnOption{
		{Name: "table"},
		{Name: "table-columns", Value: &columns},
		{Name: "separator", Value: &separator},
	})
	if err != nil {
		return newHistoryError("格式化表格失败：%v", err)
	}
	fmt.Print(output)
	return nil
}
