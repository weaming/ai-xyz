package main

import (
	"sort"
	"strings"
	"time"
)

// 会话来源名称。
const (
	sourceAll      = "all"
	sourceClaude   = "claude"
	sourceCodex    = "codex"
	sourceQoder    = "qoder"
	sourceQoderApp = "qoder-app"
)

// TokenUsage 汇总会话的 token 使用量。
type TokenUsage struct {
	InputTokens     int
	OutputTokens    int
	CacheHitTokens  int
	CacheMissTokens int
}

// HasData 判断是否有有效的 token 统计数据。
func (t *TokenUsage) HasData() bool {
	return t.InputTokens > 0 || t.OutputTokens > 0 || t.CacheHitTokens > 0 || t.CacheMissTokens > 0
}

// TotalInputTokens 返回输入 token 总量（含缓存命中）。
func (t *TokenUsage) TotalInputTokens() int {
	return t.InputTokens + t.CacheHitTokens + t.CacheMissTokens
}

// CacheHitRate 返回缓存命中率百分比（缓存命中占总输入的比例）。
func (t *TokenUsage) CacheHitRate() float64 {
	total := t.TotalInputTokens()
	if total == 0 {
		return 0
	}
	return float64(t.CacheHitTokens) / float64(total) * 100
}

// ToolCall 表示一次完整的工具调用，含输入和输出。
type ToolCall struct {
	Name   string
	Input  string
	Output string
}

// ConversationTurn 表示一轮用户输入、工具调用和助手回答。
type ConversationTurn struct {
	Question  string
	Thinking  string
	Tools     []string
	ToolCalls []ToolCall
	Answer    string
	StartedAt *time.Time
	EndedAt   *time.Time
}

// addTool 去重添加工具名。
func (t *ConversationTurn) addTool(name string) {
	if name == "" {
		return
	}
	for _, existing := range t.Tools {
		if existing == name {
			return
		}
	}
	t.Tools = append(t.Tools, name)
}

// appendThinking 按出现顺序追加思考内容。
func (t *ConversationTurn) appendThinking(content string) {
	content = strings.TrimSpace(content)
	if content == "" {
		return
	}
	if t.Thinking == "" {
		t.Thinking = content
		return
	}
	t.Thinking += "\n" + content
}

// appendToolCall 记录一次工具调用详情，返回其在列表中的下标。
func (t *ConversationTurn) appendToolCall(name, input, output string) int {
	t.ToolCalls = append(t.ToolCalls, ToolCall{Name: name, Input: input, Output: output})
	return len(t.ToolCalls) - 1
}

// addTimestamp 记录该轮活动时间并更新起止时间。
func (t *ConversationTurn) addTimestamp(activityTime *time.Time) {
	if activityTime == nil {
		return
	}
	if t.StartedAt == nil || activityTime.Before(*t.StartedAt) {
		t.StartedAt = activityTime
	}
	if t.EndedAt == nil || activityTime.After(*t.EndedAt) {
		t.EndedAt = activityTime
	}
}

// Duration 返回该轮用时，无时间数据时返回 nil。
func (t *ConversationTurn) Duration() *time.Duration {
	if t.StartedAt == nil || t.EndedAt == nil {
		return nil
	}
	duration := t.EndedAt.Sub(*t.StartedAt)
	return &duration
}

// SessionData 表示一个解析后的会话。
type SessionData struct {
	Source          string
	SessionID       string
	Path            string
	IsArchived      bool
	Inputs          []string
	Tools           []string
	FinalOutput     string
	StartedAt       *time.Time
	EndedAt         *time.Time
	Turns           []ConversationTurn
	WorkingDir      string
	CompactSummary  string
	PlanSlug        string
	TokenStats      TokenUsage
	RequestHitRates []float64
}

// sourceConfig 描述会话来源的能力差异，只放短字段。
type sourceConfig struct {
	hasArchive bool                      // 是否提供归档状态
	findPlan   func(*SessionData) string // 查找关联 plan 文件，nil 表示无
}

// sourceConfigs 各来源能力配置，未列出的来源使用零值。
var sourceConfigs = map[string]sourceConfig{
	sourceClaude:   {findPlan: findClaudePlanBySlug},
	sourceCodex:    {hasArchive: true},
	sourceQoder:    {findPlan: findQoderPlanBySlug},
	sourceQoderApp: {},
}

// isValidSource 判断是否为已知的会话来源。
func isValidSource(source string) bool {
	_, ok := sourceConfigs[source]
	return ok
}

// knownSources 返回排序后的全部来源名。
func knownSources() []string {
	names := make([]string, 0, len(sourceConfigs))
	for name := range sourceConfigs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// getSourceConfig 返回来源配置，未知来源返回零值。
func getSourceConfig(source string) sourceConfig {
	return sourceConfigs[source]
}

// refreshSummary 根据对话轮次刷新会话级摘要字段。
func (s *SessionData) refreshSummary() {
	filtered := make([]ConversationTurn, 0, len(s.Turns))
	for _, turn := range s.Turns {
		if turn.Question != "" || strings.TrimSpace(turn.Thinking) != "" || len(turn.Tools) > 0 || len(turn.ToolCalls) > 0 || strings.TrimSpace(turn.Answer) != "" {
			filtered = append(filtered, turn)
		}
	}
	s.Turns = filtered

	s.Inputs = nil
	for _, turn := range s.Turns {
		if turn.Question != "" {
			s.Inputs = append(s.Inputs, turn.Question)
		}
	}

	s.Tools = nil
	seen := make(map[string]struct{})
	for _, turn := range s.Turns {
		for _, name := range turn.Tools {
			if _, ok := seen[name]; !ok {
				seen[name] = struct{}{}
				s.Tools = append(s.Tools, name)
			}
		}
	}

	s.FinalOutput = ""
	for index := len(s.Turns) - 1; index >= 0; index-- {
		if strings.TrimSpace(s.Turns[index].Answer) != "" {
			s.FinalOutput = s.Turns[index].Answer
			break
		}
	}
}

// matchesQuery 判断查询词是否出现在用户输入或最终输出中。
func (s *SessionData) matchesQuery(query string) bool {
	if query == "" {
		return true
	}
	normalizedQuery := strings.ToLower(query)
	contains := func(text string) bool {
		return strings.Contains(strings.ToLower(text), normalizedQuery)
	}
	for _, input := range s.Inputs {
		if contains(input) {
			return true
		}
	}
	for _, turn := range s.Turns {
		if contains(turn.Answer) {
			return true
		}
	}
	return false
}

// addActivityTimestamp 记录时间戳并更新起止时间。
func (s *SessionData) addActivityTimestamp(timestamp any, loc *time.Location) {
	s.addActivityTime(parseTimestamp(timestamp, loc))
}

// addActivityTime 记录已解析的时间并更新起止时间。
func (s *SessionData) addActivityTime(activityTime *time.Time) {
	if activityTime == nil {
		return
	}
	if s.StartedAt == nil || activityTime.Before(*s.StartedAt) {
		s.StartedAt = activityTime
	}
	if s.EndedAt == nil || activityTime.After(*s.EndedAt) {
		s.EndedAt = activityTime
	}
}

// endedDate 返回会话结束日期，无结束时间时返回 nil。
func (s *SessionData) endedDate() *time.Time {
	if s.EndedAt == nil {
		return nil
	}
	day := time.Date(s.EndedAt.Year(), s.EndedAt.Month(), s.EndedAt.Day(), 0, 0, 0, 0, s.EndedAt.Location())
	return &day
}

// addTokenUsage 累加单次 API 调用的 token 使用量，并记录该次请求的命中率。
func (s *SessionData) addTokenUsage(input, output, cacheHit, cacheMiss int) {
	s.TokenStats.InputTokens += input
	s.TokenStats.OutputTokens += output
	s.TokenStats.CacheHitTokens += cacheHit
	s.TokenStats.CacheMissTokens += cacheMiss
	total := input + cacheHit + cacheMiss
	if total > 0 {
		rate := float64(cacheHit) / float64(total) * 100
		s.RequestHitRates = append(s.RequestHitRates, rate)
	}
}

// TurnDurationSummary 汇总各轮用时整体情况。
type TurnDurationSummary struct {
	Count            int
	Total, Avg       time.Duration
	Min, Max, Median time.Duration
	MinTurn, MaxTurn int // 最短/最长轮的序号（1 基）
}

// TurnDurationStats 返回各轮用时统计，无时间数据时返回 false。
func (s *SessionData) TurnDurationStats() (TurnDurationSummary, bool) {
	type timedTurn struct {
		index    int
		duration time.Duration
	}
	var timed []timedTurn
	for index, turn := range s.Turns {
		if duration := turn.Duration(); duration != nil {
			timed = append(timed, timedTurn{index + 1, *duration})
		}
	}
	if len(timed) == 0 {
		return TurnDurationSummary{}, false
	}
	sort.Slice(timed, func(i, j int) bool { return timed[i].duration < timed[j].duration })
	var total time.Duration
	for _, item := range timed {
		total += item.duration
	}
	count := len(timed)
	median := timed[count/2].duration
	if count%2 == 0 {
		median = (timed[count/2-1].duration + timed[count/2].duration) / 2
	}
	return TurnDurationSummary{
		Count:   count,
		Total:   total,
		Avg:     total / time.Duration(count),
		Min:     timed[0].duration,
		Max:     timed[count-1].duration,
		Median:  median,
		MinTurn: timed[0].index,
		MaxTurn: timed[count-1].index,
	}, true
}

// getLastAnswerIndex 获取最后一个助手回答所在的轮次编号。
func (s *SessionData) getLastAnswerIndex() int {
	for index := len(s.Turns) - 1; index >= 0; index-- {
		if s.Turns[index].Answer != "" {
			return index + 1
		}
	}
	if len(s.Turns) > 0 {
		return len(s.Turns)
	}
	return 1
}

// HitRateStats 返回命中率统计（最小、最大、平均、中位数）。
func (s *SessionData) HitRateStats() (min, max, avg, median float64, hasData bool) {
	rates := s.RequestHitRates
	if len(rates) == 0 {
		return 0, 0, 0, 0, false
	}
	sorted := make([]float64, len(rates))
	copy(sorted, rates)
	sort.Float64s(sorted)

	min = sorted[0]
	max = sorted[len(sorted)-1]

	sum := 0.0
	for _, rate := range rates {
		sum += rate
	}
	avg = sum / float64(len(rates))

	n := len(sorted)
	if n%2 == 0 {
		median = (sorted[n/2-1] + sorted[n/2]) / 2
	} else {
		median = sorted[n/2]
	}

	return min, max, avg, median, true
}
