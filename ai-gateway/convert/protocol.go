package convert

import "io"

// Mode 控制无法映射字段的处理方式。
type Mode string

const (
	ModePortable Mode = "portable"
	ModePreserve Mode = "preserve"
	ModeStrict   Mode = "strict"
)

// Protocol 标识请求或响应所使用的 API 协议。
type Protocol string

const (
	ProtocolChatCompletions Protocol = "chat"
	ProtocolResponses       Protocol = "responses"
)

// Options 是协议转换所需的运行时选项。
type Options struct {
	DefaultModel  string
	Mode          Mode
	EmitReasoning bool
}

// Converter 是一个厂商协议转换器。
type Converter interface {
	Provider() string
	Supports(source, target Protocol) bool
	ConvertRequest(source, target Protocol, input []byte, options Options) ([]byte, error)
	ConvertResponse(source, target Protocol, input []byte, options Options) ([]byte, error)
	ConvertStream(source, target Protocol, reader io.Reader, writer io.Writer, options Options) error
	AggregateStream(source, target Protocol, reader io.Reader, options Options) ([]byte, error)
}
