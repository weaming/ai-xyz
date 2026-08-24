package main

import (
	"os"
	"path/filepath"
)

// findPlanMD 按来源配置查找关联的 plan 文件。
func findPlanMD(session *SessionData) string {
	find := getSourceConfig(session.Source).findPlan
	if find == nil {
		return ""
	}
	return find(session)
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

// findQoderPlanBySlug 按会话记录的 planFilePath 对应的 slug 查找 Qoder plan 文件。
func findQoderPlanBySlug(session *SessionData) string {
	if session.PlanSlug == "" {
		return ""
	}
	plansDirectory := filepath.Join(homeDir(), ".qoder-cn", "plans")
	planFile := filepath.Join(plansDirectory, session.PlanSlug+".md")
	if info, err := os.Stat(planFile); err == nil && info.Mode().IsRegular() {
		return planFile
	}
	return ""
}

// homeDir 返回用户主目录，失败时返回空字符串。
func homeDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return home
}
