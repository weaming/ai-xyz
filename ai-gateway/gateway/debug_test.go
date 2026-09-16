package gateway

import (
	"net/http"
	"testing"
)

func TestDebugHubPublishesOnlyToSubscribers(t *testing.T) {
	hub := newDebugHub()
	hub.publish([]byte("ignored"))
	if hub.count.Load() != 0 {
		t.Fatalf("subscriber count = %d", hub.count.Load())
	}

	events, unsubscribe := hub.subscribe()
	defer unsubscribe()
	hub.publish([]byte("event"))
	select {
	case event := <-events:
		if string(event) != "event" {
			t.Fatalf("event = %q", event)
		}
	default:
		t.Fatal("subscriber did not receive event")
	}
}

func TestCloneDebugHeadersRedactsCredentials(t *testing.T) {
	headers := cloneDebugHeaders(http.Header{
		"Authorization": []string{"Bearer secret"},
		"Cookie":        []string{"session=secret"},
		"X-Test":        []string{"value"},
	})
	if headers["Authorization"][0] == "Bearer secret" || headers["Cookie"][0] == "session=secret" {
		t.Fatalf("sensitive headers were not redacted: %#v", headers)
	}
	if headers["X-Test"][0] != "value" {
		t.Fatalf("normal header changed: %#v", headers)
	}
}
