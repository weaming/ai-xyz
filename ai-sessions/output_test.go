package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestShortHomePath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skipf("无法获取主目录：%v", err)
	}

	underHome := filepath.Join(home, ".claude", "projects", "x.jsonl")
	want := "~" + string(os.PathSeparator) + filepath.Join(".claude", "projects", "x.jsonl")
	if got := shortHomePath(underHome); got != want {
		t.Fatalf("主目录下路径 = %q, 期望 %q", got, want)
	}

	if got := shortHomePath(home); got != "~" {
		t.Fatalf("主目录本身 = %q, 期望 ~", got)
	}

	outside := "/tmp/not-home.jsonl"
	if got := shortHomePath(outside); got != outside {
		t.Fatalf("主目录外路径 = %q, 期望原样 %q", got, outside)
	}
}
