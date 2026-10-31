package gateway

import (
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/weaming/ai-xyz/ai-gateway/convert"
)

// isWebSocketUpgrade 判断请求是否要求升级为 WebSocket。
func isWebSocketUpgrade(request *http.Request) bool {
	return strings.EqualFold(strings.TrimSpace(request.Header.Get("Upgrade")), "websocket") &&
		headerHasToken(request.Header, "Connection", "upgrade")
}

// headerHasToken 按逗号切分查找 token（Connection 可能是多值列表）。
func headerHasToken(header http.Header, name, token string) bool {
	for _, value := range header.Values(name) {
		for _, part := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}

// serveResponsesWebSocket 把 /provider/<id>/v1/responses 的升级请求原样透传到上游 responses WebSocket，双向帧不做转换。
func (gateway *Gateway) serveResponsesWebSocket(
	writer http.ResponseWriter,
	request *http.Request,
	routeID string,
	markUpgrade func(),
) {
	route := gateway.routes[routeID]
	if route.config.Upstream.Protocol != convert.ProtocolResponses {
		writeError(writer, http.StatusBadRequest, fmt.Sprintf("route %q 的上游不是 responses 协议，不支持 WebSocket", routeID))
		return
	}
	path := resolveUpstreamPath(route.config.Upstream.Provider, route.config.Upstream.Protocol)
	upstreamURL, err := url.Parse(joinUpstreamURL(route.config.Upstream.BaseURL, path))
	if err != nil {
		writeError(writer, http.StatusBadGateway, "解析上游 WebSocket 地址失败: "+err.Error())
		return
	}
	proxy := &httputil.ReverseProxy{
		Transport: route.client.Transport,
		Director: func(outbound *http.Request) {
			markUpgrade()
			outbound.URL.Scheme = upstreamURL.Scheme
			outbound.URL.Host = upstreamURL.Host
			outbound.URL.Path = upstreamURL.Path
			outbound.URL.RawQuery = ""
			outbound.Host = upstreamURL.Host
			outbound.Header.Del("Authorization")
			if route.upstreamToken != "" {
				outbound.Header.Set("Authorization", "Bearer "+route.upstreamToken)
			}
		},
		ErrorHandler: func(writer http.ResponseWriter, _ *http.Request, proxyErr error) {
			gateway.logger.Error("responses WebSocket 透传失败", "route", routeID, "error", proxyErr)
			writeError(writer, http.StatusBadGateway, "上游 WebSocket 连接失败: "+proxyErr.Error())
		},
	}
	proxy.ServeHTTP(writer, request)
}
