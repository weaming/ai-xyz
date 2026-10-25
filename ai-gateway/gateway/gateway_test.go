package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/weaming/ai-xyz/ai-gateway/config"
	"github.com/weaming/ai-xyz/ai-gateway/convert"
)

func TestParseAPIPath(t *testing.T) {
	tests := []struct {
		path     string
		id       string
		protocol convert.Protocol
		ok       bool
	}{
		{path: "/provider/codex/v1/chat/completions", id: "codex", protocol: convert.ProtocolChatCompletions, ok: true},
		{path: "/provider/codex/v1/responses", id: "codex", protocol: convert.ProtocolResponses, ok: true},
		{path: "/provider/codex/v1/models", ok: false},
	}
	for _, test := range tests {
		id, protocol, ok := parseAPIPath(test.path)
		if id != test.id || protocol != test.protocol || ok != test.ok {
			t.Fatalf("parseAPIPath(%q) = %q, %q, %v", test.path, id, protocol, ok)
		}
	}
}

func TestGatewayHealthz(t *testing.T) {
	handler, err := New(config.Config{Routes: []config.Route{{ID: "demo", Upstream: config.Upstream{Protocol: "responses", BaseURL: "https://example.test/v1"}}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest("GET", "/healthz", nil)
	handler.ServeHTTP(recorder, request)
	if recorder.Code != 200 || recorder.Body.String() != "{\"status\":\"ok\"}\n" {
		t.Fatalf("status=%d body=%q", recorder.Code, recorder.Body.String())
	}
}

func TestGatewayErrorKeepsReadablePathPlaceholder(t *testing.T) {
	handler, err := New(config.Config{Routes: []config.Route{{ID: "demo", Upstream: config.Upstream{Protocol: "responses", BaseURL: "https://example.test/v1"}}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/invalid", nil))
	if strings.Contains(recorder.Body.String(), `\u003c`) || !strings.Contains(recorder.Body.String(), "<id>") {
		t.Fatalf("body = %q", recorder.Body.String())
	}
}

func TestGatewayTranslatesChatToResponses(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/responses" {
			t.Errorf("upstream path = %s", request.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if body["instructions"] != "be concise" {
			t.Errorf("instructions = %#v", body["instructions"])
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"id":"resp_1","created_at":10,"model":"m","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}`))
	}))
	defer upstream.Close()

	handler, err := New(config.Config{Routes: []config.Route{{ID: "demo", Auth: config.Auth{Token: "in"}, Upstream: config.Upstream{Protocol: "responses", BaseURL: upstream.URL + "/v1", Token: "out"}}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/provider/demo/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"system","content":"be concise"},{"role":"user","content":"hi"}]}`))
	request.Header.Set("Authorization", "Bearer in")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	choice := response["choices"].([]any)[0].(map[string]any)
	if choice["message"].(map[string]any)["content"] != "ok" {
		t.Fatalf("response = %#v", response)
	}
}

func TestGatewayUsesDeepSeekResponsesPath(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/responses" {
			t.Errorf("upstream path = %s", request.URL.Path)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"id":"resp_1"}`))
	}))
	defer upstream.Close()

	handler, err := New(config.Config{Routes: []config.Route{{
		ID: "deepseek-responses",
		Upstream: config.Upstream{
			Provider: "deepseek",
			Protocol: convert.ProtocolResponses,
			BaseURL:  upstream.URL,
		},
	}}}, nil)
	if err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodPost, "/provider/deepseek-responses/v1/responses", strings.NewReader(`{"model":"deepseek-flash","input":"hi"}`))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || recorder.Body.String() != `{"id":"resp_1"}` {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestGatewayGeminiRoute(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if !strings.Contains(request.URL.Path, "models/gemini-test:generateContent") && !strings.Contains(request.URL.Path, "models/gemini-test:streamGenerateContent") {
			t.Errorf("Gemini path = %s", request.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Errorf("decode Gemini request: %v", err)
		}
		if _, exists := body["contents"]; !exists {
			t.Errorf("Gemini contents missing: %#v", body)
		}
		if strings.Contains(request.URL.Path, ":streamGenerateContent") {
			writer.Header().Set("Content-Type", "text/event-stream")
			_, _ = writer.Write([]byte("data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"hello\"}]},\"finishReason\":\"STOP\"}]}\n\n"))
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"hello"}]},"finishReason":"STOP"}],"modelVersion":"gemini-test"}`))
	}))
	defer upstream.Close()

	cfg := config.Config{Routes: []config.Route{{
		ID: "gemini", Auth: config.Auth{Token: "incoming"},
		Upstream: config.Upstream{Provider: "google", Protocol: convert.ProtocolGenerateContent, BaseURL: upstream.URL, Token: "outgoing"},
		Defaults: config.Defaults{Model: "gemini-test"},
	}}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	handler, err := New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/provider/gemini/v1/models", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized models status=%d", unauthorized.Code)
	}
	request := httptest.NewRequest(http.MethodPost, "/provider/gemini/v1/chat/completions", strings.NewReader(`{"messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Set("Authorization", "Bearer incoming")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"content":"hello"`) {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	responseRequest := httptest.NewRequest(http.MethodPost, "/provider/gemini/v1/responses", strings.NewReader(`{"input":"hi"}`))
	responseRequest.Header.Set("Authorization", "Bearer incoming")
	responses := httptest.NewRecorder()
	handler.ServeHTTP(responses, responseRequest)
	if responses.Code != http.StatusOK || !strings.Contains(responses.Body.String(), `"output_text"`) {
		t.Fatalf("responses status=%d body=%s", responses.Code, responses.Body.String())
	}
	if !strings.Contains(responses.Body.String(), `"status":"completed"`) {
		t.Fatalf("responses status missing: %s", responses.Body.String())
	}
	unsupported := httptest.NewRequest(http.MethodPost, "/provider/gemini/v1/responses", strings.NewReader(`{"input":"hi","previous_response_id":"resp_1"}`))
	unsupported.Header.Set("Authorization", "Bearer incoming")
	unsupportedRecorder := httptest.NewRecorder()
	handler.ServeHTTP(unsupportedRecorder, unsupported)
	if unsupportedRecorder.Code != http.StatusBadRequest || !strings.Contains(unsupportedRecorder.Body.String(), "previous_response_id") {
		t.Fatalf("unsupported status=%d body=%s", unsupportedRecorder.Code, unsupportedRecorder.Body.String())
	}

	streamRequest := httptest.NewRequest(http.MethodPost, "/provider/gemini/v1/chat/completions", strings.NewReader(`{"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	streamRequest.Header.Set("Authorization", "Bearer incoming")
	stream := httptest.NewRecorder()
	handler.ServeHTTP(stream, streamRequest)
	if stream.Code != http.StatusOK || !strings.Contains(stream.Body.String(), "data: [DONE]") || !strings.Contains(stream.Body.String(), `"content":"hello"`) {
		t.Fatalf("stream status=%d body=%s", stream.Code, stream.Body.String())
	}
	responseStreamRequest := httptest.NewRequest(http.MethodPost, "/provider/gemini/v1/responses", strings.NewReader(`{"stream":true,"input":"hi"}`))
	responseStreamRequest.Header.Set("Authorization", "Bearer incoming")
	responseStream := httptest.NewRecorder()
	handler.ServeHTTP(responseStream, responseStreamRequest)
	if responseStream.Code != http.StatusOK || !strings.Contains(responseStream.Body.String(), "event: response.completed\n") {
		t.Fatalf("responses stream status=%d body=%s", responseStream.Code, responseStream.Body.String())
	}

	models := httptest.NewRecorder()
	modelRequest := httptest.NewRequest(http.MethodGet, "/provider/gemini/v1/models", nil)
	modelRequest.Header.Set("Authorization", "Bearer incoming")
	handler.ServeHTTP(models, modelRequest)
	if models.Code != http.StatusOK || !strings.Contains(models.Body.String(), "gemini-test") {
		t.Fatalf("models status=%d body=%s", models.Code, models.Body.String())
	}
}

func TestGatewayAggregatesForcedResponsesStream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte("event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"model\":\"m\",\"created_at\":10}}\n\n"))
		_, _ = writer.Write([]byte("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n"))
		_, _ = writer.Write([]byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"model\":\"m\",\"created_at\":10,\"status\":\"completed\"}}\n\n"))
	}))
	defer upstream.Close()

	handler, err := New(config.Config{Routes: []config.Route{{ID: "demo", Upstream: config.Upstream{Protocol: "responses", BaseURL: upstream.URL + "/v1"}}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/provider/demo/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["content"] != "ok" {
		t.Fatalf("response = %#v", response)
	}
}

func TestGatewayTranslatesResponsesStreamToChat(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte("event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"model\":\"m\",\"created_at\":10}}\n\n"))
		_, _ = writer.Write([]byte("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n"))
		_, _ = writer.Write([]byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"model\":\"m\",\"created_at\":10,\"status\":\"completed\"}}\n\n"))
	}))
	defer upstream.Close()

	handler, err := New(config.Config{Routes: []config.Route{{ID: "demo", Upstream: config.Upstream{Protocol: "responses", BaseURL: upstream.URL + "/v1"}}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/provider/demo/v1/chat/completions", strings.NewReader(`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"content":"ok"`) || !strings.Contains(recorder.Body.String(), "[DONE]") {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}
