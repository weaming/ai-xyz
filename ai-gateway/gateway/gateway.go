package gateway

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/weaming/ai-xyz/ai-gateway/config"
	"github.com/weaming/ai-xyz/ai-gateway/convert"
)

const maxRequestBytes = 32 * 1024 * 1024

// Gateway 将带 id 的路由分发到不同上游，并负责协议转换。
type Gateway struct {
	routes map[string]routeHandler
	logger *slog.Logger
	debug  *debugHub
}

type routeHandler struct {
	config        config.Route
	upstreamToken string
	client        *http.Client
	converter     convert.Converter
}

// New 创建网关 HTTP handler。
func New(cfg config.Config, logger *slog.Logger) (*Gateway, error) {
	if logger == nil {
		logger = slog.Default()
	}
	routes := make(map[string]routeHandler, len(cfg.Routes))
	converters := convert.DefaultRegistry()
	for _, route := range cfg.Routes {
		converter, err := converters.ForProvider(route.Upstream.Provider)
		if err != nil {
			return nil, fmt.Errorf("route %q: %w", route.ID, err)
		}
		upstreamToken, err := route.Upstream.ResolveToken()
		if err != nil {
			return nil, fmt.Errorf("route %q: %w", route.ID, err)
		}
		client, err := newHTTPClient(route.Upstream)
		if err != nil {
			return nil, fmt.Errorf("route %q: %w", route.ID, err)
		}
		routes[route.ID] = routeHandler{config: route, upstreamToken: upstreamToken, client: client, converter: converter}
	}
	return &Gateway{routes: routes, logger: logger, debug: newDebugHub()}, nil
}

// ServeHTTP 暴露 healthz 和 /provider/<id>/ 协议路径。
func (gateway *Gateway) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	observedWriter := &observedResponseWriter{ResponseWriter: writer}
	writer = observedWriter
	var debug *debugRequest
	if request.URL.Path != debugPath {
		debug = gateway.debug.request(request)
	}
	var responseCapture *captureResponseWriter
	if debug != nil {
		responseCapture = &captureResponseWriter{ResponseWriter: writer}
		responseCapture.debug = debug
		writer = responseCapture
		defer func() {
			responseCapture.finish(observedWriter.statusCode)
		}()
	}
	startedAt := time.Now()
	routeID := ""
	incomingProtocol := convert.Protocol("")
	upstreamProtocol := convert.Protocol("")
	stream := false
	var failure string
	defer func() {
		attrs := []any{
			"method", request.Method,
			"path", request.URL.Path,
			"route", routeID,
			"incoming_protocol", incomingProtocol,
			"upstream_protocol", upstreamProtocol,
			"stream", stream,
			"status", observedWriter.statusCode,
			"duration_ms", time.Since(startedAt).Milliseconds(),
		}
		if failure != "" {
			attrs = append(attrs, "error", failure)
		}
		gateway.logger.Info("请求完成", attrs...)
	}()

	if request.URL.Path == "/healthz" {
		writeJSON(writer, http.StatusOK, map[string]string{"status": "ok"})
		return
	}
	if request.URL.Path == debugPath {
		gateway.serveDebug(writer, request)
		return
	}

	var ok bool
	routeID, incomingProtocol, ok = parseAPIPath(request.URL.Path)
	if !ok {
		writeError(writer, http.StatusNotFound, "路径必须是 /provider/<id>/v1/chat/completions 或 /provider/<id>/v1/responses")
		return
	}
	route, exists := gateway.routes[routeID]
	if !exists {
		writeError(writer, http.StatusNotFound, "未知 route id: "+routeID)
		return
	}
	if request.Method != http.MethodPost {
		writeError(writer, http.StatusMethodNotAllowed, "只支持 POST")
		return
	}
	if !authorize(request, route.config.Auth.Token) {
		writeError(writer, http.StatusUnauthorized, "缺少有效的 Bearer token")
		return
	}

	body, err := io.ReadAll(io.LimitReader(request.Body, maxRequestBytes+1))
	if err != nil {
		failure = err.Error()
		writeError(writer, http.StatusBadRequest, "读取请求体失败: "+err.Error())
		return
	}
	if debug != nil {
		debug.publish("request", map[string]any{
			"method":  request.Method,
			"path":    request.URL.RequestURI(),
			"headers": cloneDebugHeaders(request.Header),
			"body":    string(body),
		})
	}
	if len(body) > maxRequestBytes {
		writeError(writer, http.StatusRequestEntityTooLarge, "请求体超过 32 MiB")
		return
	}
	stream, err = requestStream(body)
	if err != nil {
		failure = err.Error()
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}

	upstreamProtocol = route.config.Upstream.Protocol
	if !route.converter.Supports(incomingProtocol, upstreamProtocol) {
		failure = fmt.Sprintf("provider %q 不支持 %s -> %s", route.converter.Provider(), incomingProtocol, upstreamProtocol)
		writeError(writer, http.StatusBadRequest, failure)
		return
	}
	translatedRequest, translatedResponse := incomingProtocol != upstreamProtocol, incomingProtocol != upstreamProtocol
	upstreamBody := body
	if translatedRequest {
		upstreamBody, err = route.converter.ConvertRequest(
			incomingProtocol,
			upstreamProtocol,
			body,
			convert.Options{DefaultModel: route.config.Defaults.Model, Mode: convert.Mode(route.config.Conversion.Mode)},
		)
		if err != nil {
			failure = err.Error()
			writeError(writer, http.StatusBadRequest, err.Error())
			return
		}
	}

	upstreamPath := "/v1/" + upstreamProtocolPath(upstreamProtocol)
	upstreamURL := joinUpstreamURL(route.config.Upstream.BaseURL, upstreamPath)
	upstreamRequest, err := http.NewRequestWithContext(request.Context(), http.MethodPost, upstreamURL, strings.NewReader(string(upstreamBody)))
	if err != nil {
		failure = err.Error()
		writeError(writer, http.StatusBadGateway, "创建上游请求失败: "+err.Error())
		return
	}
	copyRequestHeaders(upstreamRequest.Header, request.Header)
	upstreamRequest.Header.Set("Content-Type", "application/json")
	upstreamRequest.Header.Del("Authorization")
	if route.upstreamToken != "" {
		upstreamRequest.Header.Set("Authorization", "Bearer "+route.upstreamToken)
	}
	if debug != nil {
		debug.publish("upstream_request", map[string]any{
			"method":  upstreamRequest.Method,
			"url":     upstreamRequest.URL.String(),
			"headers": cloneDebugHeaders(upstreamRequest.Header),
			"body":    string(upstreamBody),
		})
	}

	response, err := route.client.Do(upstreamRequest)
	if err != nil {
		failure = err.Error()
		writeError(writer, http.StatusBadGateway, "上游请求失败: "+err.Error())
		return
	}
	if debug != nil {
		debug.publish("upstream_response", map[string]any{
			"status":  response.StatusCode,
			"headers": cloneDebugHeaders(response.Header),
		})
		response.Body = &captureReadCloser{
			ReadCloser:  response.Body,
			debug:       debug,
			kind:        "upstream_response",
			contentType: response.Header.Get("Content-Type"),
		}
		capture := response.Body.(*captureReadCloser)
		defer func() {
			capture.finish(response.StatusCode, response.Header)
		}()
	}
	defer response.Body.Close()

	if !translatedResponse || response.StatusCode >= http.StatusBadRequest {
		if !stream && response.StatusCode < http.StatusBadRequest && isEventStream(response) {
			output, conversionErr := aggregateStreamResponse(route.converter, response.Body, incomingProtocol, upstreamProtocol, route.config.Conversion.EmitReasoningContent)
			if conversionErr != nil {
				failure = conversionErr.Error()
				writeError(writer, http.StatusBadGateway, "聚合上游流式响应失败: "+conversionErr.Error())
				return
			}
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(response.StatusCode)
			_, _ = writer.Write(output)
			return
		}
		copyResponse(writer, response)
		return
	}
	if stream {
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.Header().Set("Cache-Control", "no-cache")
		writer.Header().Set("Connection", "keep-alive")
		writer.WriteHeader(response.StatusCode)
		err = route.converter.ConvertStream(
			upstreamProtocol,
			incomingProtocol,
			response.Body,
			writer,
			convert.Options{EmitReasoning: route.config.Conversion.EmitReasoningContent},
		)
		if err != nil {
			failure = err.Error()
		}
		return
	}
	if isEventStream(response) {
		output, err := aggregateStreamResponse(route.converter, response.Body, incomingProtocol, upstreamProtocol, route.config.Conversion.EmitReasoningContent)
		if err != nil {
			failure = err.Error()
			writeError(writer, http.StatusBadGateway, "聚合上游流式响应失败: "+err.Error())
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(response.StatusCode)
		_, _ = writer.Write(output)
		return
	}

	responseBody, err := io.ReadAll(io.LimitReader(response.Body, maxRequestBytes+1))
	if err != nil {
		failure = err.Error()
		writeError(writer, http.StatusBadGateway, "读取上游响应失败: "+err.Error())
		return
	}
	if looksLikeSSE(responseBody) {
		output, err := aggregateStreamResponse(route.converter, bytes.NewReader(responseBody), incomingProtocol, upstreamProtocol, route.config.Conversion.EmitReasoningContent)
		if err != nil {
			failure = err.Error()
			writeError(writer, http.StatusBadGateway, "聚合上游流式响应失败: "+err.Error())
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(response.StatusCode)
		_, _ = writer.Write(output)
		return
	}
	var output []byte
	output, err = route.converter.ConvertResponse(
		upstreamProtocol,
		incomingProtocol,
		responseBody,
		convert.Options{},
	)
	if err != nil {
		failure = err.Error()
		writeError(writer, http.StatusBadGateway, "转换上游响应失败: "+err.Error())
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(response.StatusCode)
	_, _ = writer.Write(output)
}

func aggregateStreamResponse(converter convert.Converter, reader io.Reader, incomingProtocol, upstreamProtocol convert.Protocol, emitReasoning bool) ([]byte, error) {
	return converter.AggregateStream(
		upstreamProtocol,
		incomingProtocol,
		reader,
		convert.Options{EmitReasoning: emitReasoning},
	)
}

func isEventStream(response *http.Response) bool {
	contentType := strings.ToLower(response.Header.Get("Content-Type"))
	return strings.HasPrefix(contentType, "text/event-stream")
}

func looksLikeSSE(body []byte) bool {
	body = bytes.TrimSpace(bytes.TrimPrefix(body, []byte{0xef, 0xbb, 0xbf}))
	if len(body) == 0 {
		return false
	}
	line, _, _ := bytes.Cut(body, []byte("\n"))
	line = bytes.TrimSpace(line)
	return bytes.HasPrefix(line, []byte("event:")) ||
		bytes.HasPrefix(line, []byte("data:")) ||
		bytes.HasPrefix(line, []byte(":"))
}

type observedResponseWriter struct {
	http.ResponseWriter
	statusCode  int
	wroteHeader bool
}

func (writer *observedResponseWriter) WriteHeader(statusCode int) {
	if writer.wroteHeader {
		return
	}
	writer.statusCode = statusCode
	writer.wroteHeader = true
	writer.ResponseWriter.WriteHeader(statusCode)
}

func (writer *observedResponseWriter) Write(body []byte) (int, error) {
	if !writer.wroteHeader {
		writer.WriteHeader(http.StatusOK)
	}
	return writer.ResponseWriter.Write(body)
}

func (writer *observedResponseWriter) Flush() {
	if !writer.wroteHeader {
		writer.WriteHeader(http.StatusOK)
	}
	if flusher, ok := writer.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func newHTTPClient(upstream config.Upstream) (*http.Client, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if upstream.Proxy != "" {
		proxyURL, err := url.Parse(upstream.Proxy)
		if err != nil {
			return nil, fmt.Errorf("解析 proxy: %w", err)
		}
		transport.Proxy = http.ProxyURL(proxyURL)
	} else {
		transport.Proxy = http.ProxyFromEnvironment
	}
	timeout := upstream.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}
	return &http.Client{Transport: transport, Timeout: timeout}, nil
}

func parseAPIPath(path string) (string, convert.Protocol, bool) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) < 4 || parts[0] != "provider" || parts[2] != "v1" {
		return "", "", false
	}
	if len(parts) == 5 && parts[3] == "chat" && parts[4] == "completions" {
		return parts[1], convert.ProtocolChatCompletions, true
	}
	if len(parts) == 4 && parts[3] == "responses" {
		return parts[1], convert.ProtocolResponses, true
	}
	return "", "", false
}

func upstreamProtocolPath(protocol convert.Protocol) string {
	if protocol == convert.ProtocolChatCompletions {
		return "chat/completions"
	}
	return "responses"
}

func joinUpstreamURL(baseURL, path string) string {
	baseURL = strings.TrimRight(baseURL, "/")
	if strings.HasSuffix(baseURL, "/v1") {
		return baseURL + strings.TrimPrefix(path, "/v1")
	}
	return baseURL + path
}

func requestStream(body []byte) (bool, error) {
	var values map[string]json.RawMessage
	if err := json.Unmarshal(body, &values); err != nil {
		return false, fmt.Errorf("请求必须是合法 JSON: %w", err)
	}
	var stream bool
	if raw := values["stream"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &stream); err != nil {
			return false, fmt.Errorf("stream 必须是布尔值")
		}
	}
	return stream, nil
}

func authorize(request *http.Request, expected string) bool {
	if expected == "" {
		return true
	}
	value := strings.TrimSpace(request.Header.Get("Authorization"))
	prefix := "Bearer "
	if len(value) <= len(prefix) || !strings.EqualFold(value[:len(prefix)], prefix) {
		return false
	}
	token := strings.TrimSpace(value[len(prefix):])
	return len(token) == len(expected) && subtle.ConstantTimeCompare([]byte(token), []byte(expected)) == 1
}

func copyRequestHeaders(target, source http.Header) {
	for key, values := range source {
		if strings.EqualFold(key, "Authorization") || strings.EqualFold(key, "Host") || strings.EqualFold(key, "Content-Length") {
			continue
		}
		for _, value := range values {
			target.Add(key, value)
		}
	}
}

func copyResponse(writer http.ResponseWriter, response *http.Response) {
	for key, values := range response.Header {
		for _, value := range values {
			writer.Header().Add(key, value)
		}
	}
	writer.WriteHeader(response.StatusCode)
	_, _ = io.Copy(writer, response.Body)
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	encoder := json.NewEncoder(writer)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(value)
}

func writeError(writer http.ResponseWriter, status int, message string) {
	writeJSON(writer, status, map[string]any{"error": map[string]any{"message": message, "type": "ai_gateway_error"}})
}
