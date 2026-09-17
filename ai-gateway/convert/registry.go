package convert

import (
	"fmt"
	"strings"
)

// Registry 按厂商名称管理协议转换器。
type Registry struct {
	converters map[string]Converter
}

// NewRegistry 创建转换器注册表。
func NewRegistry(converters ...Converter) *Registry {
	registry := &Registry{converters: make(map[string]Converter, len(converters))}
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
		registry.converters = make(map[string]Converter)
	}
	registry.converters[strings.ToLower(strings.TrimSpace(converter.Provider()))] = converter
}

// ForProvider 返回指定厂商的转换器。
func (registry *Registry) ForProvider(provider string) (Converter, error) {
	key := strings.ToLower(strings.TrimSpace(provider))
	if key == "" {
		key = "openai"
	}
	converter, exists := registry.converters[key]
	if !exists {
		return nil, fmt.Errorf("未注册 provider 转换器: %s", provider)
	}
	return converter, nil
}

// DefaultRegistry 返回当前内置厂商转换器。
func DefaultRegistry() *Registry {
	return NewRegistry(NewOpenAIConverter(), NewDeepSeekConverter())
}
