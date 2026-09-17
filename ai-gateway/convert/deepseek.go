package convert

// DeepSeekConverter 复用 OpenAI wire protocol 的转换逻辑。
type DeepSeekConverter struct {
	OpenAIConverter
}

// NewDeepSeekConverter 创建 DeepSeek 转换器。
func NewDeepSeekConverter() Converter {
	return DeepSeekConverter{}
}

func (DeepSeekConverter) Provider() string {
	return "deepseek"
}
