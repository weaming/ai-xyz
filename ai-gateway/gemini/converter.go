package gemini

import (
	"io"

	"github.com/weaming/ai-xyz/ai-gateway/convert"
	"github.com/weaming/ai-xyz/ai-gateway/ir"
	"google.golang.org/genai"
)

// Converter 是 google/vertex 的 IR 中枢转换器：chat、responses 与 generate_content 两两互通。
type Converter struct {
	*convert.Hub
}

// NewConverter 创建 Gemini 中枢转换器。
func NewConverter(provider ir.Provider, defaultThinking genai.ThinkingLevel) *Converter {
	return &Converter{Hub: convert.NewHub(provider,
		codec{defaultThinking: defaultThinking},
		convert.ChatCodec{},
		convert.ResponsesCodec{},
	)}
}

// ParseRequest 把入口协议请求解析成 IR。
func (converter *Converter) ParseRequest(protocol convert.Protocol, body []byte, options convert.Options) (*ir.Request, error) {
	codec, err := converter.Hub.Codec(protocol)
	if err != nil {
		return nil, err
	}
	return codec.ParseRequest(body, options)
}

// WriteResponse 把 IR 响应写成出口协议响应。
func (converter *Converter) WriteResponse(protocol convert.Protocol, response *ir.Response, options convert.Options) ([]byte, error) {
	codec, err := converter.Hub.Codec(protocol)
	if err != nil {
		return nil, err
	}
	return codec.WriteResponse(response, options)
}

// NewStreamWriter 创建出口协议的流式写入器。
func (converter *Converter) NewStreamWriter(protocol convert.Protocol, writer io.Writer, options convert.Options) (convert.StreamWriter, error) {
	codec, err := converter.Hub.Codec(protocol)
	if err != nil {
		return nil, err
	}
	return codec.NewStreamWriter(writer, options), nil
}

// ConvertRequest 通过 IR 完成任意两个入口/上游协议之间的请求转换。
func (converter *Converter) ConvertRequest(source, target convert.Protocol, input []byte, options convert.Options) ([]byte, error) {
	return converter.Hub.ConvertRequest(source, target, input, options)
}
