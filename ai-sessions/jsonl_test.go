package main

import (
	"os"
	"path/filepath"
	"testing"
)

// setupSessionDir 创建带会话文件的临时目录，返回目录路径和会话 ID。
func setupSessionDir(t *testing.T, sessionID string) string {
	t.Helper()
	dir := t.TempDir()
	writeFile := func(name string) {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(sessionID + ".jsonl")
	writeFile(sessionID + ".jsonl.wakatime")
	return dir
}

func TestSessionCandidatesPrefix(t *testing.T) {
	const sessionID = "019f0e6b-3d3c-7ddc-a96a-6b6d6e2cbf01"
	dir := setupSessionDir(t, sessionID)
	cases := []struct {
		name      string
		prefix    string
		wantCount int
	}{
		{"完整 ID", sessionID, 1},
		{"唯一前缀", sessionID[:8], 1},
		{"极短前缀", sessionID[:3], 1},
		{"无匹配", "deadbeef", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			candidates := sessionCandidates(tc.prefix, []string{dir}, true)
			if len(candidates) != tc.wantCount {
				t.Fatalf("sessionCandidates(%q) = %d 个候选, 期望 %d", tc.prefix, len(candidates), tc.wantCount)
			}
			for _, path := range candidates {
				if !filepath.HasPrefix(path, dir) {
					t.Fatalf("候选路径不在搜索目录内：%s", path)
				}
			}
		})
	}
}

func TestSessionCandidatesAmbiguous(t *testing.T) {
	dir := t.TempDir()
	for _, id := range []string{"019a-0001", "019a-0002"} {
		if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	candidates := sessionCandidates("019a", []string{dir}, true)
	if len(candidates) != 2 {
		t.Fatalf("前缀命中多个文件时应返回全部候选, 得到 %d 个", len(candidates))
	}
}
