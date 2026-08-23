package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mattn/go-runewidth"
)

const (
	defaultTimezone = "Asia/Shanghai"
	blue            = "\033[94m"
	yellow          = "\033[33m"
	faint           = "\033[2m"
	reset           = "\033[0m"
)

// historyError 表示会话历史无法解析。
type historyError struct {
	message string
}

func (e *historyError) Error() string {
	return e.message
}

func newHistoryError(format string, args ...any) error {
	return &historyError{message: fmt.Sprintf(format, args...)}
}

// getDisplayTimezone 读取 TZ 环境变量，未设置时使用 Asia/Shanghai。
func getDisplayTimezone() (*time.Location, error) {
	timezoneName := os.Getenv("TZ")
	if timezoneName == "" {
		timezoneName = defaultTimezone
	}
	loc, err := time.LoadLocation(timezoneName)
	if err != nil {
		return nil, newHistoryError("无效的 TZ 时区：%s", timezoneName)
	}
	return loc, nil
}

// parseTimestamp 将 Unix 毫秒或 ISO 时间戳转换为显示时区时间。
func parseTimestamp(timestamp any, loc *time.Location) *time.Time {
	switch value := timestamp.(type) {
	case float64:
		parsed := time.UnixMilli(int64(value)).In(loc)
		return &parsed
	case string:
		if value == "" {
			return nil
		}
		// 带时区的格式
		for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999Z07:00"} {
			if parsed, err := time.Parse(layout, value); err == nil {
				parsed = parsed.In(loc)
				return &parsed
			}
		}
		// 无时区的格式，按显示时区解释
		for _, layout := range []string{"2006-01-02T15:04:05.999999999", "2006-01-02 15:04:05.999999999", "2006-01-02"} {
			if parsed, err := time.Parse(layout, value); err == nil {
				parsed = time.Date(parsed.Year(), parsed.Month(), parsed.Day(),
					parsed.Hour(), parsed.Minute(), parsed.Second(), parsed.Nanosecond(), loc)
				return &parsed
			}
		}
	}
	return nil
}

// getToday 获取显示时区中的今天。
func getToday(loc *time.Location) time.Time {
	now := time.Now().In(loc)
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
}

// getDateTimestampBounds 获取显示时区指定日期的 Unix 毫秒边界。
func getDateTimestampBounds(targetDate time.Time, loc *time.Location) (int64, int64) {
	start := time.Date(targetDate.Year(), targetDate.Month(), targetDate.Day(), 0, 0, 0, 0, loc)
	end := start.AddDate(0, 0, 1)
	return start.UnixMilli(), end.UnixMilli()
}

// toInt 将 JSON 解析出的数值安全转换为 int。
func toInt(value any) int {
	switch number := value.(type) {
	case float64:
		return int(number)
	case int:
		return number
	case int64:
		return int(number)
	}
	return 0
}

// getText 从字符串或消息内容块中提取可展示文本。
func getText(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case []any:
		var parts []string
		for _, block := range typed {
			switch item := block.(type) {
			case string:
				parts = append(parts, item)
			case map[string]any:
				if item["type"] == "text" {
					if text, ok := item["text"].(string); ok {
						parts = append(parts, text)
					}
				}
			}
		}
		return strings.Join(parts, "\n")
	default:
		return ""
	}
}

// getThinkingText 从思考字段中提取文本，兼容字符串、数组和内容块。
func getThinkingText(value any) string {
	var parts []string
	var walk func(any)
	walk = func(current any) {
		switch typed := current.(type) {
		case string:
			if strings.TrimSpace(typed) != "" {
				parts = append(parts, strings.TrimSpace(typed))
			}
		case []any:
			for _, item := range typed {
				walk(item)
			}
		case map[string]any:
			for _, key := range []string{"text", "thinking", "reasoning_content", "reasoning", "analysis", "summary", "content"} {
				if nested, ok := typed[key]; ok {
					walk(nested)
				}
			}
		}
	}
	walk(value)
	return strings.Join(parts, "\n")
}

// getThinkingBlocksText 从消息内容中提取 thinking/reasoning 内容块。
func getThinkingBlocksText(value any) string {
	var parts []string
	var walk func(any)
	walk = func(current any) {
		switch typed := current.(type) {
		case []any:
			for _, item := range typed {
				walk(item)
			}
		case map[string]any:
			typeName, _ := typed["type"].(string)
			lowerType := strings.ToLower(typeName)
			if strings.Contains(lowerType, "think") || strings.Contains(lowerType, "reason") {
				parts = append(parts, getThinkingText(typed))
			}
		}
	}
	walk(value)
	return strings.Join(parts, "\n")
}

// forEachToolBlock 递归遍历内容中的 tool_use 和 tool_result 块。
func forEachToolBlock(value any, handle func(block map[string]any)) {
	var walk func(any)
	walk = func(current any) {
		switch item := current.(type) {
		case map[string]any:
			if item["type"] == "tool_use" || item["type"] == "tool_result" {
				handle(item)
			}
			for _, nested := range item {
				walk(nested)
			}
		case []any:
			for _, nested := range item {
				walk(nested)
			}
		}
	}
	walk(value)
}

// toolBlockName 提取工具块中的工具名。
func toolBlockName(block map[string]any) string {
	if name, ok := block["name"].(string); ok {
		return name
	}
	if name, ok := block["tool_name"].(string); ok {
		return name
	}
	return ""
}

// getToolNames 递归提取消息内容中的工具名称。
func getToolNames(value any) []string {
	var names []string
	forEachToolBlock(value, func(block map[string]any) {
		if block["type"] == "tool_use" {
			names = append(names, toolBlockName(block))
		}
	})
	return names
}

// jsonString 将任意值序列化为紧凑 JSON 字符串，失败时返回空字符串。
func jsonString(value any) string {
	if value == nil {
		return ""
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return string(encoded)
}

// expandUser 展开路径开头的 ~。
func expandUser(path string) string {
	if !strings.HasPrefix(path, "~/") {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	return home + path[1:]
}

// displayWidth 计算文本在终端中的显示列数。
func displayWidth(text string) int {
	width := 0
	for _, character := range text {
		if character < utf8.RuneSelf {
			width++
			continue
		}
		width += runewidth.RuneWidth(character)
	}
	return width
}

// shortenToTerminalWidth 按终端宽度将文本压成不会换行的单行。
func shortenToTerminalWidth(content, prefix string, terminalWidth int) string {
	singleLine := "（无）"
	if content != "" {
		singleLine = strings.Join(strings.Fields(content), " ")
	}
	availableWidth := terminalWidth - displayWidth(prefix) - 1
	if availableWidth < 1 {
		availableWidth = 1
	}
	if displayWidth(singleLine) <= availableWidth {
		return singleLine
	}

	targetWidth := availableWidth - 1
	var truncated strings.Builder
	currentWidth := 0
	for _, character := range singleLine {
		characterWidth := displayWidth(string(character))
		if currentWidth+characterWidth > targetWidth {
			break
		}
		truncated.WriteRune(character)
		currentWidth += characterWidth
	}
	return truncated.String() + "…"
}

var errNoSessions = errors.New("没有找到可解析的会话")
