package gemini

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"

	"github.com/weaming/ai-xyz/ai-gateway/ir"
	"google.golang.org/genai"
)

// ClientOptions 配置 Google Gemini API 或 Vertex AI 的连接。
type ClientOptions struct {
	Provider       ir.Provider
	GoogleAPIKey   string
	VertexProject  string
	VertexLocation string
	BaseURL        string
	Proxy          string
}

// NewClient 创建 Gemini API 或 Vertex AI 客户端。
func NewClient(ctx context.Context, options ClientOptions) (*genai.Client, error) {
	clientConfig := &genai.ClientConfig{}
	switch options.Provider {
	case ir.ProviderVertex:
		if options.VertexProject == "" || options.VertexLocation == "" {
			return nil, fmt.Errorf("Vertex AI 需要同时配置 project 和 location")
		}
		clientConfig.Backend = genai.BackendVertexAI
		clientConfig.Project = options.VertexProject
		clientConfig.Location = options.VertexLocation
	case ir.ProviderGoogle:
		clientConfig.Backend = genai.BackendGeminiAPI
		clientConfig.APIKey = options.GoogleAPIKey
		if clientConfig.APIKey == "" {
			clientConfig.APIKey = os.Getenv("GEMINI_API_KEY")
		}
		if clientConfig.APIKey == "" {
			clientConfig.APIKey = os.Getenv("GOOGLE_API_KEY")
		}
		if clientConfig.APIKey == "" {
			return nil, fmt.Errorf("Gemini API 需要 token、token_env 或 GEMINI_API_KEY")
		}
	default:
		return nil, fmt.Errorf("Gemini 客户端不支持 provider %q", options.Provider)
	}
	clientConfig.HTTPOptions.BaseURL = options.BaseURL
	if options.Proxy != "" {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		proxyURL, err := url.Parse(options.Proxy)
		if err != nil || proxyURL.Scheme == "" || proxyURL.Host == "" {
			return nil, fmt.Errorf("Gemini proxy 无效: %q", options.Proxy)
		}
		transport.Proxy = http.ProxyURL(proxyURL)
		clientConfig.HTTPClient = &http.Client{Transport: transport}
		if clientConfig.Backend == genai.BackendVertexAI {
			if err := clientConfig.UseDefaultCredentials(); err != nil {
				return nil, fmt.Errorf("加载 Vertex AI 凭据: %w", err)
			}
			quotaProject, err := clientConfig.Credentials.QuotaProjectID(ctx)
			if err != nil {
				return nil, fmt.Errorf("读取 Vertex AI quota project: %w", err)
			}
			if quotaProject != "" {
				clientConfig.HTTPOptions.Headers = http.Header{"X-Goog-User-Project": []string{quotaProject}}
			}
		}
	}
	client, err := genai.NewClient(ctx, clientConfig)
	if err != nil {
		return nil, fmt.Errorf("创建 Gemini 客户端: %w", err)
	}
	return client, nil
}
