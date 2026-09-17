package gateway

import (
	"bytes"
	"io"
	"net/http"
	"strings"
)

const maxFilesRequestSize int64 = 128 * 1024 * 1024

type filesProvider interface {
	ValidateUpload(contentType string, body []byte) error
	UpstreamURL(baseURL string) string
}

var filesProviders = map[string]filesProvider{
	"deepseek": deepSeekFilesProvider{},
}

func filesProviderFor(provider string) (filesProvider, bool) {
	filesProvider, ok := filesProviders[provider]
	return filesProvider, ok
}

// parseFilesPath 判断是否为独立的 DeepSeek Files 上传路径。
func parseFilesPath(path string) (string, bool) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) == 4 && parts[0] == "provider" && parts[2] == "v1" && parts[3] == "files" {
		return parts[1], true
	}
	return "", false
}

// handleFilesUpload 校验 Files 请求后原样代理请求和响应。
func (gateway *Gateway) handleFilesUpload(writer http.ResponseWriter, request *http.Request, routeID string) {
	route, exists := gateway.routes[routeID]
	if !exists {
		writeError(writer, http.StatusNotFound, "未知 route id: "+routeID)
		return
	}
	provider, exists := filesProviderFor(route.config.Upstream.Provider)
	if !exists {
		writeError(writer, http.StatusBadRequest, "Files API 暂不支持 provider: "+route.config.Upstream.Provider)
		return
	}
	if request.Method != http.MethodPost {
		writeError(writer, http.StatusMethodNotAllowed, "Files API 只支持 POST")
		return
	}
	if !authorize(request, route.config.Auth.Token) {
		writeError(writer, http.StatusUnauthorized, "缺少有效的 Bearer token")
		return
	}

	body, err := io.ReadAll(io.LimitReader(request.Body, maxFilesRequestSize+1))
	if err != nil {
		writeError(writer, http.StatusBadRequest, "读取文件上传请求失败: "+err.Error())
		return
	}
	if int64(len(body)) > maxFilesRequestSize {
		writeError(writer, http.StatusRequestEntityTooLarge, "文件上传请求超过 128 MiB")
		return
	}
	if err := provider.ValidateUpload(request.Header.Get("Content-Type"), body); err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}

	upstreamURL := provider.UpstreamURL(route.config.Upstream.BaseURL)
	upstreamRequest, err := http.NewRequestWithContext(request.Context(), http.MethodPost, upstreamURL, bytes.NewReader(body))
	if err != nil {
		writeError(writer, http.StatusBadGateway, "创建 Files 上游请求失败: "+err.Error())
		return
	}
	copyRequestHeaders(upstreamRequest.Header, request.Header)
	upstreamRequest.Header.Set("Content-Type", request.Header.Get("Content-Type"))
	upstreamRequest.Header.Del("Content-Length")
	if route.upstreamToken != "" {
		upstreamRequest.Header.Set("Authorization", "Bearer "+route.upstreamToken)
	}

	response, err := route.client.Do(upstreamRequest)
	if err != nil {
		writeError(writer, http.StatusBadGateway, "Files 上游请求失败: "+err.Error())
		return
	}
	defer response.Body.Close()
	copyResponse(writer, response)
}
