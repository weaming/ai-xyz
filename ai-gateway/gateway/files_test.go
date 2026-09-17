package gateway

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/weaming/ai-xyz/ai-gateway/config"
)

func TestParseFilesPath(t *testing.T) {
	routeID, ok := parseFilesPath("/provider/deepseek/v1/files")
	if !ok || routeID != "deepseek" {
		t.Fatalf("parseFilesPath = %q, %v", routeID, ok)
	}
	if _, ok := parseFilesPath("/provider/deepseek/v1/chat/completions"); ok {
		t.Fatal("chat path should not match Files API")
	}
}

func TestFilesUploadValidatesBeforeProxyingAndPreservesRawBody(t *testing.T) {
	var expectedBody bytes.Buffer
	writer := multipart.NewWriter(&expectedBody)
	if err := writer.WriteField("purpose", "user_data"); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteField("expires_after[anchor]", "created_at"); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteField("expires_after[seconds]", "3600"); err != nil {
		t.Fatal(err)
	}
	part, err := writer.CreateFormFile("file", "image.png")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write([]byte("image bytes")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	var upstreamBody []byte
	var upstreamContentType string
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/files" || request.Method != http.MethodPost {
			t.Errorf("upstream request = %s %s", request.Method, request.URL.Path)
		}
		upstreamContentType = request.Header.Get("Content-Type")
		upstreamBody, _ = io.ReadAll(request.Body)
		response.Header().Set("Content-Type", "application/octet-stream")
		response.Header().Set("X-Upstream", "preserved")
		response.WriteHeader(http.StatusCreated)
		_, _ = response.Write([]byte("raw response"))
	}))
	defer upstream.Close()

	handler, err := New(config.Config{Routes: []config.Route{{
		ID:   "deepseek",
		Auth: config.Auth{Token: "gateway-token"},
		Upstream: config.Upstream{
			Provider: "deepseek",
			Protocol: "chat",
			BaseURL:  upstream.URL + "/v1",
			Token:    "upstream-token",
		},
	}}}, nil)
	if err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodPost, "/provider/deepseek/v1/files", bytes.NewReader(expectedBody.Bytes()))
	request.Header.Set("Authorization", "Bearer gateway-token")
	request.Header.Set("Content-Type", writer.FormDataContentType())
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusCreated || recorder.Body.String() != "raw response" {
		t.Fatalf("response = %d %q", recorder.Code, recorder.Body.String())
	}
	if !bytes.Equal(upstreamBody, expectedBody.Bytes()) {
		t.Fatal("upstream body was changed")
	}
	if upstreamContentType != writer.FormDataContentType() {
		t.Fatalf("content type = %q, want %q", upstreamContentType, writer.FormDataContentType())
	}
}

func TestFilesUploadRejectsInvalidRequestBeforeProxying(t *testing.T) {
	called := false
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		called = true
	}))
	defer upstream.Close()

	handler, err := New(config.Config{Routes: []config.Route{{
		ID:       "deepseek",
		Upstream: config.Upstream{Provider: "deepseek", Protocol: "chat", BaseURL: upstream.URL + "/v1"},
	}}}, nil)
	if err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodPost, "/provider/deepseek/v1/files", strings.NewReader("not multipart"))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusBadRequest || called {
		t.Fatalf("status=%d called=%v body=%s", recorder.Code, called, recorder.Body.String())
	}
}

func TestValidateFilesRequestRejectsInvalidParameters(t *testing.T) {
	tests := []struct {
		name   string
		fields map[string]string
	}{
		{name: "purpose", fields: map[string]string{"purpose": "assistants"}},
		{name: "missing expiry pair", fields: map[string]string{"purpose": "user_data", "expires_after[seconds]": "3600"}},
		{name: "expiry range", fields: map[string]string{"purpose": "user_data", "expires_after[anchor]": "created_at", "expires_after[seconds]": "3599"}},
		{name: "unknown field", fields: map[string]string{"purpose": "user_data", "metadata": "x"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body, contentType := filesTestBody(t, test.fields)
			if err := validateDeepSeekFilesRequest(contentType, body); err == nil {
				t.Fatal("invalid Files request should be rejected")
			}
		})
	}
}

func TestFilesUploadRejectsNonDeepSeekRoute(t *testing.T) {
	handler, err := New(config.Config{Routes: []config.Route{{
		ID:       "openai",
		Upstream: config.Upstream{Provider: "openai", Protocol: "responses", BaseURL: "https://example.test/v1"},
	}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/provider/openai/v1/files", nil))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func filesTestBody(t *testing.T, fields map[string]string) ([]byte, string) {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for name, value := range fields {
		if err := writer.WriteField(name, value); err != nil {
			t.Fatal(err)
		}
	}
	part, err := writer.CreateFormFile("file", "image.png")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write([]byte("image bytes")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return body.Bytes(), writer.FormDataContentType()
}
