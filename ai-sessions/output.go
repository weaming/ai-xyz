package main

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"
)

// useTerminalColor 判断当前输出是否适合使用 ANSI 颜色。
func useTerminalColor() bool {
	if _, ok := os.LookupEnv("NO_COLOR"); ok {
		return false
	}
	return term.IsTerminal(int(os.Stdout.Fd()))
}

// getTerminalWidth 返回终端宽度。管道场景（如 | less）stdout 不是终端，
// 此时改用 stderr 探测；再回退 COLUMNS 环境变量，最小为 1。
func getTerminalWidth() int {
	for _, fd := range []int{int(os.Stdout.Fd()), int(os.Stderr.Fd())} {
		if width, _, err := term.GetSize(fd); err == nil && width >= 1 {
			return width
		}
	}
	if columnsText := os.Getenv("COLUMNS"); columnsText != "" {
		if columns, err := strconv.Atoi(columnsText); err == nil && columns >= 1 {
			return columns
		}
	}
	return 80
}

// printLabeledText 以紧凑标签格式输出多行文本。
func printLabeledText(label, content string, indentContinuation bool, separator string) {
	if strings.TrimSpace(content) == "" {
		fmt.Printf("%s%s （无）\n", label, separator)
		return
	}
	lines := strings.Split(strings.TrimSpace(content), "\n")
	fmt.Printf("%s%s %s\n", label, separator, lines[0])
	for _, line := range lines[1:] {
		if indentContinuation {
			fmt.Printf("   %s\n", line)
		} else {
			fmt.Println(line)
		}
	}
}

// printLine 输出终端宽度、指定线型的淡色分隔线。
func printLine(lineCharacter string, useColor, withBlankLines bool) {
	separator := strings.Repeat(lineCharacter, getTerminalWidth())
	if withBlankLines {
		fmt.Println()
	}
	if useColor {
		fmt.Printf("%s%s%s\n", faint, separator, reset)
	} else {
		fmt.Println(separator)
	}
	if withBlankLines {
		fmt.Println()
	}
}

// printSeparator 输出终端宽度的淡色实线分隔线。
func printSeparator(useColor, withBlankLines bool) {
	printLine("─", useColor, withBlankLines)
}

// printSessionTime 输出会话起止时间和显示时区。
func printSessionTime(session *SessionData, loc *time.Location) {
	fmt.Printf("Time: %s\n", formatSessionTime(session.StartedAt, session.EndedAt, loc))
}

// formatTokenCount 将 token 数格式化为紧凑可读形式。
func formatTokenCount(count int) string {
	if count >= 1000000 {
		return fmt.Sprintf("%.1fM", float64(count)/1000000)
	}
	if count >= 1000 {
		return fmt.Sprintf("%.1fk", float64(count)/1000)
	}
	return fmt.Sprintf("%d", count)
}

// formatPercent 格式化百分比，0 显示为 0%，其他显示为两位小数。
func formatPercent(value float64) string {
	if value == 0 {
		return "0%"
	}
	return fmt.Sprintf("%.2f%%", value)
}

// printTokenUsage 输出 token 使用量和缓存命中率。
func printTokenUsage(session *SessionData) {
	stats := &session.TokenStats
	if !stats.HasData() {
		return
	}
	totalInput := stats.TotalInputTokens()
	fmt.Printf("Tokens: %s 输入 | %s 输出", formatTokenCount(totalInput), formatTokenCount(stats.OutputTokens))
	if stats.CacheHitTokens > 0 && stats.CacheMissTokens > 0 {
		fmt.Printf(" | 缓存: %s 命中 (%.2f%%) | %s 创建",
			formatTokenCount(stats.CacheHitTokens), stats.CacheHitRate(),
			formatTokenCount(stats.CacheMissTokens))
	} else if stats.CacheHitTokens > 0 {
		fmt.Printf(" | 缓存: %s 命中 (%.2f%%)",
			formatTokenCount(stats.CacheHitTokens), stats.CacheHitRate())
	} else if stats.CacheMissTokens > 0 {
		fmt.Printf(" | 缓存: %s 创建", formatTokenCount(stats.CacheMissTokens))
	}
	if min, max, avg, median, ok := session.HitRateStats(); ok {
		if max == 0 {
			fmt.Printf(" | 命中率: 0%%")
		} else if min == max {
			fmt.Printf(" | 命中率: %s, avg %s, median %s",
				formatPercent(min), formatPercent(avg), formatPercent(median))
		} else {
			minStr := formatPercent(min)
			if len(minStr) > 0 && minStr[len(minStr)-1] == '%' {
				minStr = minStr[:len(minStr)-1]
			}
			fmt.Printf(" | 命中率: %s-%s, avg %s, median %s",
				minStr, formatPercent(max), formatPercent(avg), formatPercent(median))
		}
	} else if stats.CacheHitTokens == 0 {
		fmt.Printf(" | 命中率: 0%%")
	}
	fmt.Println()
}

// printSession 以紧凑格式输出会话正文。
func printSession(session *SessionData, loc *time.Location, useColor bool, fullSummary, showThinking bool) {
	sessionID := session.SessionID
	if useColor {
		sessionID = blue + sessionID + reset
	}
	fmt.Printf("[%s] UUID %s\n", session.Source, sessionID)
	printSessionTime(session, loc)
	if session.WorkingDir != "" {
		fmt.Printf("CWD: %s\n", session.WorkingDir)
	}
	if planPath := findPlanMD(session); planPath != "" {
		fmt.Printf("Plan: %s\n", planPath)
	}
	printTokenUsage(session)
	fmt.Println()

	if len(session.Turns) == 0 {
		fmt.Println(" Q: （无）")
		printLabeledText(" A:", session.FinalOutput, false, "")
		return
	}

	labelWidth := len(fmt.Sprintf("Q%d:", len(session.Turns))) + 1
	skipNext := false
	for index, turn := range session.Turns {
		if skipNext {
			skipNext = false
			continue
		}
		questionLabel := fmt.Sprintf("%*s", labelWidth, fmt.Sprintf("Q%d:", index+1))
		if strings.HasPrefix(turn.Question, "/compact") {
			fmt.Printf("%s %s\n", questionLabel, turn.Question)
			if index+1 < len(session.Turns) {
				nextTurn := session.Turns[index+1]
				if nextTurn.Question != "" {
					nextLabel := fmt.Sprintf("%*s", labelWidth, fmt.Sprintf("Q%d:", index+2))
					if fullSummary {
						fmt.Printf("%s %s\n", nextLabel, nextTurn.Question)
					} else {
						prefix := nextLabel + " "
						fmt.Printf("%s%s\n", prefix, shortenToTerminalWidth(nextTurn.Question, prefix, getTerminalWidth()))
					}
					skipNext = true
				}
			}
			continue
		}
		printLabeledText(questionLabel, turn.Question, true, "")
		if showThinking {
			printThinking(turn, useColor)
		}
		if len(turn.Tools) > 0 {
			toolLabel := fmt.Sprintf("%*s", labelWidth, fmt.Sprintf("T%d:", index+1))
			fmt.Printf("%s %s\n", toolLabel, strings.Join(turn.Tools, ", "))
		}
	}

	answerIndex := session.getLastAnswerIndex()
	answerLabel := fmt.Sprintf("%*s", labelWidth, fmt.Sprintf("A%d:", answerIndex))
	answerPrefix := answerLabel + " "
	answerText := shortenToTerminalWidth(session.FinalOutput, answerPrefix, getTerminalWidth())
	printLabeledText(answerLabel, answerText, false, "")
}

// printTurnDetail 输出指定轮次的问题、全部工具调用输入输出和回答。
func printTurnDetail(session *SessionData, turnNumber int, loc *time.Location, useColor, showThinking bool) {
	turn := session.Turns[turnNumber-1]
	sessionID := session.SessionID
	if useColor {
		sessionID = blue + sessionID + reset
	}
	fmt.Printf("[%s] UUID %s\n", session.Source, sessionID)
	printSessionTime(session, loc)
	if session.WorkingDir != "" {
		fmt.Printf("CWD: %s\n", session.WorkingDir)
	}
	fmt.Println()

	printLabeledText(fmt.Sprintf("Q%d:", turnNumber), turn.Question, true, "")
	if showThinking {
		printThinking(turn, useColor)
	}
	inputLabel, outputLabel := "  Input", "  Output"
	if useColor {
		inputLabel = yellow + inputLabel + reset
		outputLabel = yellow + outputLabel + reset
	}
	for callIndex, call := range turn.ToolCalls {
		printSeparator(useColor, false)
		toolName := call.Name
		if useColor {
			toolName = blue + toolName + reset
		}
		fmt.Printf("T%d.%d %s\n", turnNumber, callIndex+1, toolName)
		printLabeledText(inputLabel, call.Input, true, ":")
		if strings.TrimSpace(call.Output) != "" {
			printLabeledText(outputLabel, call.Output, true, ":")
		}
	}
	printSeparator(useColor, false)
	printLabeledText(fmt.Sprintf("A%d:", turnNumber), turn.Answer, false, "")
}

// printThinking 输出一轮会话的中间思考过程。
func printThinking(turn ConversationTurn, useColor bool) {
	content := strings.TrimSpace(turn.Thinking)
	if content == "" {
		return
	}

	printSeparator(useColor, false)
	label := "  Think"
	if useColor {
		label = yellow + label + reset
	}
	printLabeledText(label, content, true, ":")
	printSeparator(useColor, false)
}

// printSessionIndex 输出会话索引。
func printSessionIndex(sessions []*SessionData, loc *time.Location) {
	useColor := useTerminalColor()
	sortedSessions := sortSessionsByEndedAt(sessions)
	for index, session := range sortedSessions {
		if index > 0 {
			printSeparator(useColor, false)
		}
		printSession(session, loc, useColor, false, false)
	}
}

// printMatchingSessions 输出匹配查询词的会话。
func printMatchingSessions(sessions []*SessionData, query string, loc *time.Location, targetDate *time.Time) {
	var matching []*SessionData
	for _, session := range sessions {
		if session.matchesQuery(query) {
			matching = append(matching, session)
		}
	}
	if len(matching) == 0 {
		fmt.Fprintf(os.Stderr, "错误：没有找到请求或最终响应包含\"%s\"的会话\n", query)
		os.Exit(1)
	}

	matching = sortSessionsByEndedAt(matching)
	useColor := useTerminalColor()
	scope := "全部日期"
	if targetDate != nil {
		scope = targetDate.Format("2006-01-02")
	}
	fmt.Printf("范围=%s TZ=%s | 匹配=%d\n", scope, loc, len(matching))
	for index, session := range matching {
		if index > 0 {
			printSeparator(useColor, true)
		}
		printSession(session, loc, useColor, true, false)
	}
}

// sortSessionsByEndedAt 按结束时间升序排序，无结束时间的排最前。
func sortSessionsByEndedAt(sessions []*SessionData) []*SessionData {
	sorted := append([]*SessionData(nil), sessions...)
	sort.SliceStable(sorted, func(i, j int) bool {
		left := sorted[i]
		right := sorted[j]
		if left.EndedAt == nil {
			return true
		}
		if right.EndedAt == nil {
			return false
		}
		return left.EndedAt.Before(*right.EndedAt)
	})
	return sorted
}
