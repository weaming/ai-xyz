package convert

import (
	"fmt"
	"strings"

	"github.com/weaming/ai-xyz/ai-gateway/ir"
)

// Registry 按厂商名称管理协议转换器。
type Registry struct {
	converters map[ir.Provider]Converter
}

// NewRegistry 创建转换器注册表。
func NewRegistry(converters ...Converter) *Registry {
	registry := &Registry{converters: make(map[ir.Provider]Converter, len(converters))}
	for _, converter := range converters {
		if converter == nil {
			continue
		}
		registry.Register(converter)
	}
	return registry
}

// Register 注册或替换一个厂商转换器。
func (registry *Registry) Register(converter Converter) {
	if registry.converters == nil {
		registry.converters = make(map[ir.Provider]Converter)
	}
	registry.converters[normalizeProvider(converter.Provider())] = converter
}

// ForProvider 返回指定厂商的转换器。
func (registry *Registry) ForProvider(provider ir.Provider) (Converter, error) {
	key := normalizeProvider(provider)
	if key == "" {
		key = ir.ProviderOpenAI
	}
	converter, exists := registry.converters[key]
	if !exists {
		return nil, fmt.Errorf("未注册 provider 转换器: %s", provider)
	}
	return converter, nil
}

func normalizeProvider(provider ir.Provider) ir.Provider {
	return ir.Provider(strings.ToLower(strings.TrimSpace(string(provider))))
}

// NewOpenAICompatibleConverter 创建 Chat Completions 与 Responses 两协议的中枢转换器。
func NewOpenAICompatibleConverter(provider ir.Provider) Converter {
	return NewHub(provider, ChatCodec{}, ResponsesCodec{})
}

// DefaultRegistry 返回当前内置厂商转换器。
func DefaultRegistry() *Registry {
	return NewRegistry(
		NewOpenAICompatibleConverter(ir.ProviderOpenAI),
		NewOpenAICompatibleConverter(ir.ProviderDeepSeek),
	)
}
