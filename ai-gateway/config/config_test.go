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

func TestLoadDeepSeekResponsesRoute(t *testing.T) {
	t.Setenv("DEEPSEEK_API_KEY", "secret")
	path := filepath.Join(t.TempDir(), "config.yaml")
	content := []byte("routes:\n  - id: deepseek-responses\n    upstream:\n      provider: deepseek\n      protocol: responses\n      base_url: https://api.deepseek.com\n      token_env: DEEPSEEK_API_KEY\n")
	if err := os.WriteFile(path, content, 0600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	route := cfg.Routes[0]
	if route.Upstream.Provider != "deepseek" || route.Upstream.Protocol != "responses" {
		t.Fatalf("route = %#v", route)
	}
}

func TestGeminiProviderProtocol(t *testing.T) {
	tests := []struct {
		name     string
		upstream Upstream
		isValid  bool
	}{
		{name: "google", upstream: Upstream{Provider: "google", Protocol: "generate_content"}, isValid: true},
		{name: "vertex", upstream: Upstream{Provider: "vertex", Protocol: "generate_content", Vertex: Vertex{Project: "project", Location: "us-central1"}}, isValid: true},
		{name: "google with chat protocol", upstream: Upstream{Provider: "google", Protocol: "chat"}},
		{name: "vertex without location", upstream: Upstream{Provider: "vertex", Protocol: "generate_content", Vertex: Vertex{Project: "project"}}},
		{name: "vertex with API key", upstream: Upstream{Provider: "vertex", Protocol: "generate_content", Vertex: Vertex{Project: "project", Location: "us-central1"}, Token: "key"}},
		{name: "google with vertex settings", upstream: Upstream{Provider: "google", Protocol: "generate_content", Vertex: Vertex{Project: "project"}}},
		{name: "invalid thinking level", upstream: Upstream{Provider: "google", Protocol: "generate_content", GenerateContent: GenerateContent{ThinkingLevel: "max"}}},
		{name: "empty alias target", upstream: Upstream{Provider: "google", Protocol: "generate_content", GenerateContent: GenerateContent{ModelAliases: map[string]string{"fast": ""}}}},
		{name: "openai with generate_content protocol", upstream: Upstream{Provider: "openai", Protocol: "generate_content", BaseURL: "https://example.test"}},
		{name: "openai with gemini options", upstream: Upstream{Provider: "openai", Protocol: "chat", BaseURL: "https://example.test", GenerateContent: GenerateContent{ThinkingLevel: "medium"}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := Config{Routes: []Route{{ID: "test", Upstream: test.upstream}}}
			err := cfg.Validate()
			if (err == nil) != test.isValid {
				t.Fatalf("Validate() error = %v, want valid = %v", err, test.isValid)
			}
		})
	}
}

func TestLoadVertexOptions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	content := []byte("routes:\n  - id: vertex\n    upstream:\n      provider: vertex\n      protocol: generate_content\n      vertex:\n        project: example-project\n        location: us-central1\n      generate_content:\n        thinking_level: medium\n        model_aliases:\n          fast: gemini-2.5-flash\n")
	if err := os.WriteFile(path, content, 0600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	upstream := cfg.Routes[0].Upstream
	if upstream.Vertex.Project != "example-project" || upstream.Vertex.Location != "us-central1" {
		t.Fatalf("vertex options = %#v", upstream.Vertex)
	}
	if upstream.GenerateContent.ThinkingLevel != "medium" || upstream.GenerateContent.ModelAliases["fast"] != "gemini-2.5-flash" {
		t.Fatalf("generate content options = %#v", upstream.GenerateContent)
	}
}
