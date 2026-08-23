package main

import (
	"os"
	"path/filepath"
	"time"
)

// findPlanMD 根据会话来源查找关联的 plan 文件。
func findPlanMD(session *SessionData) string {
	switch session.Source {
	case "claude":
		return findClaudePlanBySlug(session)
	case "qoder":
		return findQoderPlanByTime(session)
	default:
		return ""
	}
}

// findClaudePlanBySlug 根据 slug 查找 Claude plan 文件。
func findClaudePlanBySlug(session *SessionData) string {
	if session.PlanSlug == "" {
		return ""
	}
	planFile := filepath.Join(homeDir(), ".claude", "plans", session.PlanSlug+".md")
	if info, err := os.Stat(planFile); err == nil && info.Mode().IsRegular() {
		return planFile
	}
	return ""
}

// findQoderPlanByTime 根据时间窗口查找 Qoder plan 文件。
func findQoderPlanByTime(session *SessionData) string {
	plansDirectory := filepath.Join(homeDir(), ".qoder-cn", "plans")
	if _, err := os.Stat(plansDirectory); err != nil || session.StartedAt == nil || session.EndedAt == nil {
		return ""
	}
	startTimestamp := session.StartedAt.Unix()
	endTimestamp := session.EndedAt.Unix()

	var candidates []string
	entries, err := os.ReadDir(plansDirectory)
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".md" {
			continue
		}
		planPath := filepath.Join(plansDirectory, entry.Name())
		info, err := entry.Info()
		if err != nil {
			continue
		}
		mtime := info.ModTime().Unix()
		if startTimestamp <= mtime && mtime <= endTimestamp {
			candidates = append(candidates, planPath)
		}
	}
	if len(candidates) == 0 {
		return ""
	}

	latest := candidates[0]
	latestTime := time.Time{}
	for _, candidate := range candidates {
		info, err := os.Stat(candidate)
		if err != nil {
			continue
		}
		if info.ModTime().After(latestTime) {
			latest = candidate
			latestTime = info.ModTime()
		}
	}
	return latest
}

// homeDir 返回用户主目录，失败时返回空字符串。
func homeDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return home
}
