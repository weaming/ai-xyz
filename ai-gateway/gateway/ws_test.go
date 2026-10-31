package gateway

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/weaming/ai-xyz/ai-gateway/config"
)

// echoUpstream 模拟 101 升级后按字节回显的上游 responses WebSocket。
func echoUpstream(t *testing.T, requests *[]*http.Request) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		*requests = append(*requests, request.Clone(request.Context()))
		if !isWebSocketUpgrade(request) {
			t.Errorf("上游未收到升级请求: %v", request.Header)
			return
		}
		conn, buffered, err := http.NewResponseController(writer).Hijack()
		if err != nil {
			t.Errorf("上游 hijack 失败: %v", err)
			return
		}
		defer conn.Close()
		_, _ = buffered.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		if err := buffered.Flush(); err != nil {
			t.Errorf("上游 flush 失败: %v", err)
			return
		}
		payload := make([]byte, 4)
		if _, err := io.ReadFull(buffered, payload); err != nil {
			t.Errorf("上游读取负载失败: %v", err)
			return
		}
		_, _ = buffered.WriteString("pong")
		_ = buffered.Flush()
	}))
}

// dialWebSocket 用裸 TCP 发升级请求，返回连接与已读缓冲。
func dialWebSocket(t *testing.T, serverURL, path, token string) (net.Conn, *bufio.Reader) {
	t.Helper()
	addr := strings.TrimPrefix(serverURL, "http://")
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatalf("连接网关失败: %v", err)
	}
	request := "GET " + path + " HTTP/1.1\r\nHost: " + addr +
		"\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n"
	if token != "" {
		request += "Authorization: Bearer " + token + "\r\n"
	}
	if _, err := conn.Write([]byte(request + "\r\n")); err != nil {
		t.Fatalf("发送升级请求失败: %v", err)
	}
	return conn, bufio.NewReader(conn)
}

// readStatusLine 读并返回响应状态行，跳过其余响应头。
func readStatusLine(t *testing.T, reader *bufio.Reader) string {
	t.Helper()
	line, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("读取状态行失败: %v", err)
	}
	for {
		header, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("读取响应头失败: %v", err)
		}
		if strings.TrimSpace(header) == "" {
			return strings.TrimSpace(line)
		}
	}
}

func TestGatewayForwardsResponsesWebSocket(t *testing.T) {
	var upstreamRequests []*http.Request
	upstream := echoUpstream(t, &upstreamRequests)
	defer upstream.Close()

	handler, err := New(config.Config{Routes: []config.Route{{
		ID:       "demo",
		Auth:     config.Auth{Token: "in"},
		Upstream: config.Upstream{Protocol: "responses", BaseURL: upstream.URL + "/v1", Token: "out"},
	}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()

	conn, reader := dialWebSocket(t, server.URL, "/provider/demo/v1/responses", "in")
	defer conn.Close()
	if status := readStatusLine(t, reader); !strings.Contains(status, "101") {
		t.Fatalf("状态行 = %q", status)
	}
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatalf("发送帧失败: %v", err)
	}
	reply := make([]byte, 4)
	if _, err := io.ReadFull(reader, reply); err != nil {
		t.Fatalf("读取回显失败: %v", err)
	}
	if string(reply) != "pong" {
		t.Fatalf("回显 = %q", reply)
	}
	if len(upstreamRequests) != 1 {
		t.Fatalf("上游请求数 = %d", len(upstreamRequests))
	}
	if path := upstreamRequests[0].URL.Path; path != "/v1/responses" {
		t.Fatalf("上游路径 = %q", path)
	}
	if token := upstreamRequests[0].Header.Get("Authorization"); token != "Bearer out" {
		t.Fatalf("上游 Authorization = %q", token)
	}
}

func TestGatewayRejectsWebSocketOnOtherProtocols(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("上游不应被调用")
	}))
	defer upstream.Close()

	handler, err := New(config.Config{Routes: []config.Route{{
		ID:       "chat",
		Upstream: config.Upstream{Protocol: "chat", BaseURL: upstream.URL + "/v1"},
	}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()

	conn, reader := dialWebSocket(t, server.URL, "/provider/chat/v1/responses", "")
	defer conn.Close()
	if status := readStatusLine(t, reader); !strings.Contains(status, "400") {
		t.Fatalf("状态行 = %q", status)
	}
}
