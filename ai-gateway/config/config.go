package config

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/weaming/ai-xyz/ai-gateway/convert"
	"github.com/weaming/ai-xyz/ai-gateway/ir"
	yaml "gopkg.in/yaml.v3"
)

// Config 是网关配置文件的根结构。
type Config struct {
	Server Server  `yaml:"server"`
	Routes []Route `yaml:"routes"`
}

// Server 定义监听和 HTTP 超时。
type Server struct {
	Address         string        `yaml:"address"`
	ReadTimeout     time.Duration `yaml:"read_timeout"`
	WriteTimeout    time.Duration `yaml:"write_timeout"`
	IdleTimeout     time.Duration `yaml:"idle_timeout"`
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout"`
}

// Route 是一个可独立鉴权、代理和选择上游协议的实例。
type Route struct {
	ID         string     `yaml:"id"`
	Auth       Auth       `yaml:"auth"`
	Upstream   Upstream   `yaml:"upstream"`
	Defaults   Defaults   `yaml:"defaults"`
	Conversion Conversion `yaml:"conversion"`
}

// Auth 配置入口 Bearer 鉴权。
type Auth struct {
	Token string `yaml:"token"`
}

// Upstream 定义上游协议、地址、鉴权和可选 HTTP 代理。
type Upstream struct {
	Provider        ir.Provider      `yaml:"provider"`
	Protocol        convert.Protocol `yaml:"protocol"`
	BaseURL         string           `yaml:"base_url"`
	Token           string           `yaml:"token"`
	TokenEnv        string           `yaml:"token_env"`
	Proxy           string           `yaml:"proxy"`
	Timeout         time.Duration    `yaml:"timeout"`
	Capabilities    []string         `yaml:"capabilities"`
	GenerateContent GenerateContent  `yaml:"generate_content"`
	Vertex          Vertex           `yaml:"vertex"`
}

// GenerateContent 定义 generate_content 协议的模型映射和默认思考级别。
type GenerateContent struct {
	ThinkingLevel string            `yaml:"thinking_level"`
	ModelAliases  map[string]string `yaml:"model_aliases"`
}

// Vertex 定义 Vertex AI 连接所需的项目和区域。
type Vertex struct {
	Project  string `yaml:"project"`
	Location string `yaml:"location"`
}

// Defaults 是请求级默认值。
type Defaults struct {
	Model string `yaml:"model"`
}

// Conversion 控制 IR 到目标协议的降级策略。
type Conversion struct {
	Mode                 string `yaml:"mode"`
	EmitReasoningContent bool   `yaml:"emit_reasoning_content"`
}

func defaultConversion() Conversion {
	return Conversion{
		Mode:                 "preserve",
		EmitReasoningContent: true,
	}
}

// UnmarshalYAML 使用兼容协议的默认转换配置。
func (conversion *Conversion) UnmarshalYAML(node *yaml.Node) error {
	type conversionAlias Conversion
	defaults := conversionAlias(defaultConversion())
	if err := node.Decode(&defaults); err != nil {
		return err
	}
	*conversion = Conversion(defaults)
	return nil
}

// UnmarshalYAML 确保省略 conversion 配置时也应用默认值。
func (route *Route) UnmarshalYAML(node *yaml.Node) error {
	type routeAlias Route
	defaults := routeAlias{
		Conversion: defaultConversion(),
	}
	if err := node.Decode(&defaults); err != nil {
		return err
	}
	*route = Route(defaults)
	return nil
}

var routeIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// Load 从 YAML 文件加载配置，并展开进程环境变量。
func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("读取配置文件 %q: %w", path, err)
	}

	expanded := os.Expand(string(data), func(key string) string {
		return os.Getenv(key)
	})

	var cfg Config
	if err := yaml.Unmarshal([]byte(expanded), &cfg); err != nil {
		return Config{}, fmt.Errorf("解析配置文件 %q: %w", path, err)
	}

	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Validate 检查配置的结构和必填项。
func (cfg *Config) Validate() error {
	if cfg.Server.Address == "" {
		cfg.Server.Address = "127.0.0.1:8787"
	}
	if len(cfg.Routes) == 0 {
		return fmt.Errorf("至少需要配置一个 route")
	}

	seen := make(map[string]struct{}, len(cfg.Routes))
	for index := range cfg.Routes {
		route := &cfg.Routes[index]
		if route.Upstream.Provider == "" {
			route.Upstream.Provider = ir.ProviderOpenAI
		}
		if route.Upstream.Provider != ir.ProviderOpenAI && route.Upstream.Provider != ir.ProviderDeepSeek && route.Upstream.Provider != ir.ProviderGoogle && route.Upstream.Provider != ir.ProviderVertex {
			return fmt.Errorf("route %q 的 provider %q 尚未实现，当前支持 openai、deepseek、google、vertex", route.ID, route.Upstream.Provider)
		}
		if route.ID == "" || !routeIDPattern.MatchString(route.ID) {
			return fmt.Errorf("routes[%d].id 无效: %q", index, route.ID)
		}
		if _, exists := seen[route.ID]; exists {
			return fmt.Errorf("route id 重复: %s", route.ID)
		}
		seen[route.ID] = struct{}{}

		isGeminiProvider := route.Upstream.Provider == ir.ProviderGoogle || route.Upstream.Provider == ir.ProviderVertex
		if isGeminiProvider && route.Upstream.Protocol != convert.ProtocolGenerateContent {
			return fmt.Errorf("route %q 的 upstream.protocol 必须是 generate_content", route.ID)
		}
		if route.Upstream.Provider == ir.ProviderVertex && (route.Upstream.Vertex.Project == "" || route.Upstream.Vertex.Location == "") {
			return fmt.Errorf("route %q 的 Vertex AI 需要 upstream.vertex.project 和 location", route.ID)
		}
		if route.Upstream.Provider == ir.ProviderVertex && (route.Upstream.Token != "" || route.Upstream.TokenEnv != "") {
			return fmt.Errorf("route %q 的 Vertex AI 使用默认凭据，不支持 upstream.token 或 token_env", route.ID)
		}
		if route.Upstream.Provider == ir.ProviderGoogle && (route.Upstream.Vertex.Project != "" || route.Upstream.Vertex.Location != "") {
			return fmt.Errorf("route %q 的 Google Gemini API 不支持 upstream.vertex", route.ID)
		}
		if isGeminiProvider {
			switch strings.ToLower(route.Upstream.GenerateContent.ThinkingLevel) {
			case "", "low", "medium", "high":
			default:
				return fmt.Errorf("route %q 的 upstream.generate_content.thinking_level 无效: %q", route.ID, route.Upstream.GenerateContent.ThinkingLevel)
			}
			for alias, model := range route.Upstream.GenerateContent.ModelAliases {
				if strings.TrimSpace(alias) == "" || strings.TrimSpace(model) == "" {
					return fmt.Errorf("route %q 的 upstream.generate_content.model_aliases 不能包含空别名或模型", route.ID)
				}
			}
		} else if route.Upstream.Vertex.Project != "" || route.Upstream.Vertex.Location != "" ||
			route.Upstream.GenerateContent.ThinkingLevel != "" || len(route.Upstream.GenerateContent.ModelAliases) > 0 {
			return fmt.Errorf("route %q 的 upstream.vertex 和 generate_content 只适用于 google 或 vertex", route.ID)
		}
		if !isGeminiProvider && route.Upstream.Protocol != convert.ProtocolResponses && route.Upstream.Protocol != convert.ProtocolChatCompletions {
			return fmt.Errorf("route %q 的 upstream.protocol 必须是 responses 或 chat", route.ID)
		}
		if route.Upstream.BaseURL == "" && !isGeminiProvider {
			return fmt.Errorf("route %q 缺少 upstream.base_url", route.ID)
		}
		if route.Conversion.Mode == "" {
			route.Conversion.Mode = "preserve"
		}
		if route.Conversion.Mode != "portable" && route.Conversion.Mode != "preserve" && route.Conversion.Mode != "strict" {
			return fmt.Errorf("route %q 的 conversion.mode 无效: %s", route.ID, route.Conversion.Mode)
		}
	}
	return nil
}

// ResolveToken 从配置中的环境变量名读取上游 token。
func (upstream Upstream) ResolveToken() (string, error) {
	if upstream.Token != "" {
		return upstream.Token, nil
	}
	if upstream.TokenEnv == "" {
		return "", nil
	}
	token, exists := os.LookupEnv(upstream.TokenEnv)
	if !exists || strings.TrimSpace(token) == "" {
		return "", fmt.Errorf("环境变量 %q 未设置", upstream.TokenEnv)
	}
	return token, nil
}

// RouteMap 返回按 id 索引的路由。
func (cfg Config) RouteMap() map[string]Route {
	result := make(map[string]Route, len(cfg.Routes))
	for _, route := range cfg.Routes {
		result[strings.TrimSpace(route.ID)] = route
	}
	return result
}
