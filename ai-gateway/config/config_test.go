package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadExpandsEnvironmentAndDefaultsConversion(t *testing.T) {
	t.Setenv("TEST_UPSTREAM_TOKEN", "secret")
	path := filepath.Join(t.TempDir(), "config.yaml")
	content := []byte("routes:\n  - id: demo-route\n    upstream:\n      protocol: responses\n      base_url: https://example.test/v1\n      token_env: TEST_UPSTREAM_TOKEN\n")
	if err := os.WriteFile(path, content, 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	conversion := cfg.Routes[0].Conversion
	if cfg.Server.Address != "127.0.0.1:8787" || conversion.Mode != "preserve" || !conversion.EmitReasoningContent || cfg.Routes[0].Upstream.TokenEnv != "TEST_UPSTREAM_TOKEN" {
		t.Fatalf("config = %#v", cfg)
	}
	if token, err := cfg.Routes[0].Upstream.ResolveToken(); err != nil || token != "secret" {
		t.Fatalf("resolved token = %q, error = %v", token, err)
	}
}

func TestLoadKeepsExplicitlyDisabledReasoningContent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	content := []byte("routes:\n  - id: demo-route\n    upstream:\n      protocol: responses\n      base_url: https://example.test/v1\n    conversion:\n      emit_reasoning_content: false\n")
	if err := os.WriteFile(path, content, 0600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Routes[0].Conversion.Mode != "preserve" || cfg.Routes[0].Conversion.EmitReasoningContent {
		t.Fatalf("conversion = %#v", cfg.Routes[0].Conversion)
	}
}

func TestLoadExample(t *testing.T) {
	t.Setenv("AI_GATEWAY_TOKEN", "in")
	t.Setenv("OPENAI_API_KEY", "out")
	cfg, err := Load(filepath.Join("..", "config.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.WriteTimeout.String() != "30m0s" || cfg.Routes[0].Upstream.Provider != "openai" {
		t.Fatalf("config = %#v", cfg)
	}
}
