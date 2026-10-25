package convert

import (
	"bytes"
	"fmt"
	"io"

	"github.com/weaming/ai-xyz/ai-gateway/ir"
)

// Hub 以 IR 为中枢组合各协议 Codec，实现 N×N 两两转换。
type Hub struct {
	provider ir.Provider
	codecs   map[Protocol]Codec
}

// NewHub 用给定 Codec 构造中枢转换器。
func NewHub(provider ir.Provider, codecs ...Codec) *Hub {
	hub := &Hub{provider: provider, codecs: make(map[Protocol]Codec, len(codecs))}
	for _, codec := range codecs {
		if codec != nil {
			hub.codecs[codec.Protocol()] = codec
		}
	}
	return hub
}

// Provider 返回中枢所属厂商。
func (hub *Hub) Provider() ir.Provider {
	return hub.provider
}

// Supports 表示两个协议都有对应 Codec。
func (hub *Hub) Supports(source, target Protocol) bool {
	_, sourceOK := hub.codecs[source]
	_, targetOK := hub.codecs[target]
	return sourceOK && targetOK
}

// Codec 返回指定协议的 Codec。
func (hub *Hub) Codec(protocol Protocol) (Codec, error) {
	codec, exists := hub.codecs[protocol]
	if !exists {
		return nil, fmt.Errorf("未注册 %s 协议 Codec", protocol)
	}
	return codec, nil
}

// ConvertRequest 先解析成 IR 再写出目标协议；同协议直接透传。
func (hub *Hub) ConvertRequest(source, target Protocol, input []byte, options Options) ([]byte, error) {
	if source == target {
		return bytes.Clone(input), nil
	}
	sourceCodec, err := hub.Codec(source)
	if err != nil {
		return nil, err
	}
	targetCodec, err := hub.Codec(target)
	if err != nil {
		return nil, err
	}
	request, err := sourceCodec.ParseRequest(input, options)
	if err != nil {
		return nil, err
	}
	applyDefaultModel(request, options)
	return targetCodec.WriteRequest(request, options)
}

// ConvertResponse 先解析成 IR 再写出目标协议；同协议直接透传。
func (hub *Hub) ConvertResponse(source, target Protocol, input []byte, options Options) ([]byte, error) {
	if source == target {
		return bytes.Clone(input), nil
	}
	sourceCodec, err := hub.Codec(source)
	if err != nil {
		return nil, err
	}
	targetCodec, err := hub.Codec(target)
	if err != nil {
		return nil, err
	}
	response, err := sourceCodec.ParseResponse(input, options)
	if err != nil {
		return nil, err
	}
	return targetCodec.WriteResponse(response, options)
}

// ConvertStream 逐事件把源协议 SSE 翻译成目标协议 SSE；同协议直接透传。
func (hub *Hub) ConvertStream(source, target Protocol, reader io.Reader, writer io.Writer, options Options) error {
	if source == target {
		_, err := io.Copy(writer, reader)
		return err
	}
	sourceCodec, err := hub.Codec(source)
	if err != nil {
		return err
	}
	targetCodec, err := hub.Codec(target)
	if err != nil {
		return err
	}
	streamWriter := targetCodec.NewStreamWriter(writer, options)
	if err := sourceCodec.ReadStream(reader, options, streamWriter.Write); err != nil {
		return err
	}
	_, err = streamWriter.Close()
	return err
}

// AggregateStream 把源协议 SSE 聚合成目标协议的非流式响应；同协议优先用 Codec 自己的聚合实现。
func (hub *Hub) AggregateStream(source, target Protocol, reader io.Reader, options Options) ([]byte, error) {
	sourceCodec, err := hub.Codec(source)
	if err != nil {
		return nil, err
	}
	targetCodec, err := hub.Codec(target)
	if err != nil {
		return nil, err
	}
	if source == target {
		if aggregateCodec, ok := sourceCodec.(AggregateCodec); ok {
			return aggregateCodec.AggregateStream(reader, options)
		}
	}
	accumulator := NewAccumulator()
	if err := sourceCodec.ReadStream(reader, options, accumulator.Apply); err != nil {
		return nil, err
	}
	response, err := accumulator.Response()
	if err != nil {
		return nil, err
	}
	return targetCodec.WriteResponse(response, options)
}

func applyDefaultModel(request *ir.Request, options Options) {
	if request.Model == "" {
		request.Model = options.DefaultModel
	}
}
