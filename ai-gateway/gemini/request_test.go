package gemini

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestServeChatRequiresModel(t *testing.T) {
	server, err := NewServer(nil, ServerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	recorder := httptest.NewRecorder()
	server.ServeChat(recorder, request, []byte(`{"messages":[{"role":"user","content":"hello"}]}`))
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "model") {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestDecodeKnownRequestRejectsNestedUnknownField(t *testing.T) {
	var request chatRequest
	err := decodeKnownRequest([]byte(`{"messages":[],"stream_options":{"unknown":true}}`), &request, chatRequestFields)
	if err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("error = %v", err)
	}
}

func TestRequestIDsAreUnique(t *testing.T) {
	seen := make(map[string]struct{}, 1000)
	for range 1000 {
		id := requestID()
		if _, exists := seen[id]; exists {
			t.Fatalf("duplicate request ID: %s", id)
		}
		seen[id] = struct{}{}
	}
}
