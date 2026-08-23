package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
)

func TestFetchModelsPassesSortParam(t *testing.T) {
	originalURL := openrouterAPI
	t.Cleanup(func() { openrouterAPI = originalURL })

	var gotSort string
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("Authorization = %q", request.Header.Get("Authorization"))
		}
		gotSort = request.URL.Query().Get("sort")
		response.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(response).Encode(ModelResponse{
			Data: []Model{{ID: "model-1", Name: "Model 1"}},
		})
	}))
	defer server.Close()
	openrouterAPI = server.URL

	if _, err := fetchModels("test-key", "most-popular"); err != nil {
		t.Fatalf("fetch failed: %v", err)
	}
	if gotSort != "most-popular" {
		t.Fatalf("sort = %q, want most-popular", gotSort)
	}

	if _, err := fetchModels("test-key", ""); err != nil {
		t.Fatalf("fetch without sort failed: %v", err)
	}
	if gotSort != "" {
		t.Fatalf("sort = %q, want empty", gotSort)
	}
}

func TestFetchModelEndpoints(t *testing.T) {
	originalURL := openrouterAPI
	t.Cleanup(func() { openrouterAPI = originalURL })

	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/openai/gpt-4/endpoints" {
			t.Errorf("path = %q, want /openai/gpt-4/endpoints", request.URL.Path)
		}
		response.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(response).Encode(EndpointsResponse{
			Data: ModelEndpointsInfo{
				ID:   "openai/gpt-4",
				Name: "GPT-4",
				Endpoints: []ModelEndpoint{{
					Name:          "OpenAI: GPT-4",
					ProviderName:  "OpenAI",
					ContextLength: 8192,
					Pricing:       EndpointPricing{Prompt: "0.00003", Completion: "0.00006"},
				}},
			},
		})
	}))
	defer server.Close()
	openrouterAPI = server.URL

	info, err := fetchModelEndpoints("test-key", "openai", "gpt-4")
	if err != nil {
		t.Fatalf("fetch failed: %v", err)
	}
	if info.ID != "openai/gpt-4" || len(info.Endpoints) != 1 || info.Endpoints[0].ProviderName != "OpenAI" {
		t.Fatalf("unexpected info: %+v", info)
	}
}

func TestSplitModelID(t *testing.T) {
	author, slug, err := splitModelID("anthropic/claude-opus-5")
	if err != nil || author != "anthropic" || slug != "claude-opus-5" {
		t.Fatalf("split failed: author=%q slug=%q err=%v", author, slug, err)
	}

	if _, _, err := splitModelID("invalid"); err == nil {
		t.Fatal("expected error for missing separator")
	}
	if _, _, err := splitModelID("/gpt-4"); err == nil {
		t.Fatal("expected error for empty author")
	}
}

func TestFetchAAModelsUsesFreshCache(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	originalURL := aaFreeAPI
	t.Cleanup(func() { aaFreeAPI = originalURL })

	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		if request.Header.Get("x-api-key") != "test-key" {
			t.Errorf("x-api-key = %q", request.Header.Get("x-api-key"))
		}
		response.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(response).Encode(AAResponse{
			Data: []AAModel{{ID: "model-1", Name: "Model 1"}},
		})
	}))
	defer server.Close()
	aaFreeAPI = server.URL

	first, err := fetchAAModels("test-key")
	if err != nil {
		t.Fatalf("first fetch failed: %v", err)
	}
	second, err := fetchAAModels("test-key")
	if err != nil {
		t.Fatalf("cached fetch failed: %v", err)
	}
	if requests.Load() != 1 {
		t.Fatalf("request count = %d, want 1", requests.Load())
	}
	if len(first) != 1 || len(second) != 1 || second[0].ID != "model-1" {
		t.Fatalf("unexpected cached models: first=%v second=%v", first, second)
	}

	resolvedPath, err := aaCachePath()
	if err != nil {
		t.Fatalf("resolve cache path failed: %v", err)
	}
	if _, err := os.Stat(resolvedPath); err != nil {
		t.Fatalf("cache file was not written: %v", err)
	}
}
