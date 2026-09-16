package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const debugPath = "/debug/requests"

type debugHub struct {
	mu          sync.RWMutex
	nextID      atomic.Uint64
	subscribers map[uint64]*debugSubscriber
	count       atomic.Int64
}

type debugSubscriber struct {
	events chan []byte
	done   chan struct{}
	once   sync.Once
}

type debugRequest struct {
	hub *debugHub
	id  string
}

type debugRecord struct {
	Time      string `json:"time"`
	RequestID string `json:"request_id"`
	Kind      string `json:"kind"`
	Data      any    `json:"data"`
}

func newDebugHub() *debugHub {
	return &debugHub{subscribers: make(map[uint64]*debugSubscriber)}
}

func (hub *debugHub) subscribe() (<-chan []byte, func()) {
	id := hub.nextID.Add(1)
	subscriber := &debugSubscriber{
		events: make(chan []byte, 256),
		done:   make(chan struct{}),
	}
	hub.mu.Lock()
	hub.subscribers[id] = subscriber
	hub.count.Add(1)
	hub.mu.Unlock()

	unsubscribe := func() {
		subscriber.once.Do(func() {
			hub.mu.Lock()
			delete(hub.subscribers, id)
			hub.count.Add(-1)
			hub.mu.Unlock()
			close(subscriber.done)
		})
	}
	return subscriber.events, unsubscribe
}

func (hub *debugHub) request(request *http.Request) *debugRequest {
	if hub.count.Load() == 0 {
		return nil
	}
	requestID := strings.TrimSpace(request.Header.Get("X-Request-ID"))
	if requestID == "" {
		requestID = fmt.Sprintf("req_%d", hub.nextID.Add(1))
	}
	return &debugRequest{hub: hub, id: requestID}
}

func (hub *debugHub) publish(record []byte) {
	if hub.count.Load() == 0 {
		return
	}
	hub.mu.RLock()
	subscribers := make([]*debugSubscriber, 0, len(hub.subscribers))
	for _, subscriber := range hub.subscribers {
		subscribers = append(subscribers, subscriber)
	}
	hub.mu.RUnlock()
	for _, subscriber := range subscribers {
		select {
		case subscriber.events <- record:
		case <-subscriber.done:
		}
	}
}

func (debug *debugRequest) publish(kind string, data any) {
	if debug == nil || debug.hub.count.Load() == 0 {
		return
	}
	record, err := json.Marshal(debugRecord{
		Time:      time.Now().Format(time.RFC3339Nano),
		RequestID: debug.id,
		Kind:      kind,
		Data:      data,
	})
	if err != nil {
		return
	}
	debug.hub.publish(record)
}

func (gateway *Gateway) serveDebug(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		writeError(writer, http.StatusMethodNotAllowed, "只支持 GET")
		return
	}
	events, unsubscribe := gateway.debug.subscribe()
	defer unsubscribe()
	writer.Header().Set("Content-Type", "text/event-stream")
	writer.Header().Set("Cache-Control", "no-cache")
	writer.Header().Set("Connection", "keep-alive")
	writer.WriteHeader(http.StatusOK)
	if flusher, ok := writer.(http.Flusher); ok {
		flusher.Flush()
	}
	for {
		select {
		case <-request.Context().Done():
			return
		case record := <-events:
			if _, err := fmt.Fprintf(writer, "event: debug\ndata: %s\n\n", record); err != nil {
				return
			}
			if flusher, ok := writer.(http.Flusher); ok {
				flusher.Flush()
			}
		}
	}
}

type debugSSECapture struct {
	debug   *debugRequest
	kind    string
	pending string
	raw     strings.Builder
	event   string
	data    []string
	done    bool
}

func newDebugSSECapture(debug *debugRequest, kind string) *debugSSECapture {
	return &debugSSECapture{debug: debug, kind: kind}
}

func (capture *debugSSECapture) feed(data []byte) {
	if capture.done || len(data) == 0 {
		return
	}
	capture.pending += string(data)
	for {
		index := strings.IndexByte(capture.pending, '\n')
		if index < 0 {
			return
		}
		line := capture.pending[:index]
		capture.pending = capture.pending[index+1:]
		capture.consumeLine(strings.TrimSuffix(line, "\r"))
	}
}

func (capture *debugSSECapture) finish() {
	if capture.done {
		return
	}
	capture.done = true
	if capture.pending != "" {
		capture.consumeLine(strings.TrimSuffix(capture.pending, "\r"))
		capture.pending = ""
	}
	capture.flush()
}

func (capture *debugSSECapture) consumeLine(line string) {
	capture.raw.WriteString(line)
	capture.raw.WriteByte('\n')
	if line == "" {
		capture.flush()
		return
	}
	if strings.HasPrefix(line, ":") {
		return
	}
	key, value, found := strings.Cut(line, ":")
	if !found {
		return
	}
	value = strings.TrimPrefix(value, " ")
	switch key {
	case "event":
		capture.event = value
	case "data":
		capture.data = append(capture.data, value)
	}
}

func (capture *debugSSECapture) flush() {
	if capture.raw.Len() == 0 {
		return
	}
	rawData := strings.Join(capture.data, "\n")
	semantic := any(rawData)
	var decoded any
	if json.Unmarshal([]byte(rawData), &decoded) == nil {
		semantic = decoded
	}
	capture.debug.publish(capture.kind, map[string]any{
		"raw":      capture.raw.String(),
		"event":    capture.event,
		"data":     rawData,
		"semantic": semantic,
	})
	capture.raw.Reset()
	capture.event = ""
	capture.data = nil
}

type captureReadCloser struct {
	io.ReadCloser
	debug       *debugRequest
	kind        string
	contentType string
	buffer      bytes.Buffer
	sse         *debugSSECapture
	finished    bool
}

func (reader *captureReadCloser) Read(data []byte) (int, error) {
	count, err := reader.ReadCloser.Read(data)
	if count > 0 && reader.debug.hub.count.Load() > 0 {
		reader.capture(data[:count])
	}
	return count, err
}

func (reader *captureReadCloser) capture(data []byte) {
	if reader.sse != nil {
		reader.sse.feed(data)
		return
	}
	if reader.contentTypeIsSSE() {
		reader.startSSE()
		reader.sse.feed(data)
		return
	}
	_, _ = reader.buffer.Write(data)
	if looksLikeSSE(reader.buffer.Bytes()) {
		reader.startSSE()
	}
}

func (reader *captureReadCloser) startSSE() {
	if reader.sse != nil {
		return
	}
	reader.sse = newDebugSSECapture(reader.debug, reader.kind+"_sse")
	buffered := reader.buffer.Bytes()
	reader.buffer.Reset()
	reader.sse.feed(buffered)
}

func (reader *captureReadCloser) finish(status int, headers http.Header) {
	if reader.finished {
		return
	}
	reader.finished = true
	if reader.sse != nil {
		reader.sse.finish()
		return
	}
	reader.debug.publish(reader.kind+"_http", map[string]any{
		"status":       status,
		"headers":      cloneDebugHeaders(headers),
		"content_type": reader.contentType,
		"body":         reader.buffer.String(),
	})
}

func (reader *captureReadCloser) contentTypeIsSSE() bool {
	return strings.HasPrefix(strings.ToLower(reader.contentType), "text/event-stream")
}

type captureResponseWriter struct {
	http.ResponseWriter
	debug    *debugRequest
	buffer   bytes.Buffer
	sse      *debugSSECapture
	finished bool
}

func (writer *captureResponseWriter) Write(data []byte) (int, error) {
	if writer.sse == nil && (isEventStreamHeader(writer.Header()) || looksLikeSSE(data)) {
		writer.sse = newDebugSSECapture(writer.debug, "response_sse")
	}
	if writer.sse != nil {
		writer.sse.feed(data)
	} else {
		_, _ = writer.buffer.Write(data)
	}
	return writer.ResponseWriter.Write(data)
}

func (writer *captureResponseWriter) Flush() {
	if flusher, ok := writer.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (writer *captureResponseWriter) finish(status int) {
	if writer.finished {
		return
	}
	writer.finished = true
	if writer.sse != nil {
		writer.sse.finish()
		return
	}
	writer.debug.publish("response_http", map[string]any{
		"status":       status,
		"headers":      cloneDebugHeaders(writer.Header()),
		"content_type": writer.Header().Get("Content-Type"),
		"body":         writer.buffer.String(),
	})
}

func isEventStreamHeader(headers http.Header) bool {
	return strings.HasPrefix(strings.ToLower(headers.Get("Content-Type")), "text/event-stream")
}

func cloneDebugHeaders(headers http.Header) map[string][]string {
	result := make(map[string][]string, len(headers))
	for name, values := range headers {
		cloned := append([]string(nil), values...)
		if strings.EqualFold(name, "Authorization") || strings.EqualFold(name, "Cookie") || strings.EqualFold(name, "Set-Cookie") {
			cloned = []string{"<redacted>"}
		}
		result[name] = cloned
	}
	return result
}
