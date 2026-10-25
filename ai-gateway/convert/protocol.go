package convert

import (
	"io"

	"github.com/weaming/ai-xyz/ai-gateway/ir"
)

// Protocol 标识请求或响应所使用的 API 协议。
type Protocol string

const (
	ProtocolChatCompletions Protocol = "chat"
	ProtocolResponses       Protocol = "responses"
	ProtocolGenerateContent Protocol = "generate_content"
)

// IR 流事件的 output index 约定：推理、正文与工具各占独立 item，
// 这样同一路流里的 reasoning 与 content 不会互相覆盖。
const (
	OutputIndexReasoning = 0
	OutputIndexMessage   = 1
	OutputIndexFirstTool = 2
)

// Options 是协议转换所需的运行时选项。
type Options struct {
	DefaultModel  string
	EmitReasoning bool
	IncludeUsage  bool
}

// Converter 是一个厂商协议转换器。
type Converter interface {
	Provider() ir.Provider
	Supports(source, target Protocol) bool
	ConvertRequest(source, target Protocol, input []byte, options Options) ([]byte, error)
	ConvertResponse(source, target Protocol, input []byte, options Options) ([]byte, error)
	ConvertStream(source, target Protocol, reader io.Reader, writer io.Writer, options Options) error
	AggregateStream(source, target Protocol, reader io.Reader, options Options) ([]byte, error)
}

// Codec 把单个协议与 IR 互相翻译。厂商适配器通过 IR 中枢组合 Codec 完成两两转换。
type Codec interface {
	Protocol() Protocol
	ParseRequest(body []byte, options Options) (*ir.Request, error)
	WriteRequest(request *ir.Request, options Options) ([]byte, error)
	ParseResponse(body []byte, options Options) (*ir.Response, error)
	WriteResponse(response *ir.Response, options Options) ([]byte, error)
	ReadStream(reader io.Reader, options Options, emit func(ir.StreamEvent) error) error
	NewStreamWriter(writer io.Writer, options Options) StreamWriter
}

// AggregateCodec 是可选接口：同协议聚合时保留源协议自身的额外字段。
type AggregateCodec interface {
	AggregateStream(reader io.Reader, options Options) ([]byte, error)
}

// StreamWriter 把 IR 流事件翻译成目标协议的 SSE。Close 返回聚合后的响应。
type StreamWriter interface {
	Write(event ir.StreamEvent) error
	Close() (*ir.Response, error)
}
