package convert

import (
	"bytes"
	"fmt"
	"io"
)

// OpenAIConverter 负责 OpenAI Chat Completions 与 Responses 之间的转换。
type OpenAIConverter struct{}

// NewOpenAIConverter 创建 OpenAI 转换器。
func NewOpenAIConverter() Converter {
	return OpenAIConverter{}
}

func (OpenAIConverter) Provider() string {
	return "openai"
}

func (OpenAIConverter) Supports(source, target Protocol) bool {
	return isOpenAIProtocol(source) && isOpenAIProtocol(target)
}

func (OpenAIConverter) ConvertRequest(source, target Protocol, input []byte, options Options) ([]byte, error) {
	if source == target {
		return bytes.Clone(input), nil
	}
	switch {
	case source == ProtocolChatCompletions && target == ProtocolResponses:
		return convertChatRequestToResponses(input, options.DefaultModel)
	case source == ProtocolResponses && target == ProtocolChatCompletions:
		return convertResponsesRequestToChat(input, options.DefaultModel, options.Mode)
	default:
		return nil, unsupportedConversion(source, target)
	}
}

func (OpenAIConverter) ConvertResponse(source, target Protocol, input []byte, options Options) ([]byte, error) {
	if source == target {
		return bytes.Clone(input), nil
	}
	switch {
	case source == ProtocolResponses && target == ProtocolChatCompletions:
		return convertResponsesResponseToChat(input)
	case source == ProtocolChatCompletions && target == ProtocolResponses:
		return convertChatResponseToResponses(input)
	default:
		return nil, unsupportedConversion(source, target)
	}
}

func (OpenAIConverter) ConvertStream(source, target Protocol, reader io.Reader, writer io.Writer, options Options) error {
	if source == target {
		_, err := io.Copy(writer, reader)
		return err
	}
	switch {
	case source == ProtocolResponses && target == ProtocolChatCompletions:
		return responsesToChatStream(reader, writer, options.EmitReasoning)
	case source == ProtocolChatCompletions && target == ProtocolResponses:
		return chatToResponsesStream(reader, writer)
	default:
		return unsupportedConversion(source, target)
	}
}

func (OpenAIConverter) AggregateStream(source, target Protocol, reader io.Reader, options Options) ([]byte, error) {
	switch {
	case source == ProtocolResponses && target == ProtocolChatCompletions:
		return responsesStreamToChatResponse(reader, options.EmitReasoning)
	case source == ProtocolChatCompletions && target == ProtocolResponses:
		return chatStreamToResponsesResponse(reader)
	case source == ProtocolResponses && target == ProtocolResponses:
		return responsesStreamToResponse(reader)
	case source == ProtocolChatCompletions && target == ProtocolChatCompletions:
		return chatStreamToResponse(reader)
	default:
		return nil, unsupportedConversion(source, target)
	}
}

func isOpenAIProtocol(protocol Protocol) bool {
	return protocol == ProtocolChatCompletions || protocol == ProtocolResponses
}

func unsupportedConversion(source, target Protocol) error {
	return fmt.Errorf("不支持协议转换: %s -> %s", source, target)
}

var defaultConverter Converter = OpenAIConverter{}

// ChatRequestToResponses 将 Chat Completions 请求转换为 Responses 请求。
func ChatRequestToResponses(input []byte, defaultModel string) ([]byte, error) {
	return defaultConverter.ConvertRequest(ProtocolChatCompletions, ProtocolResponses, input, Options{DefaultModel: defaultModel})
}

// ResponsesRequestToChat 将 Responses 请求转换为 Chat Completions 请求。
func ResponsesRequestToChat(input []byte, defaultModel string, mode Mode) ([]byte, error) {
	return defaultConverter.ConvertRequest(ProtocolResponses, ProtocolChatCompletions, input, Options{DefaultModel: defaultModel, Mode: mode})
}

// ResponsesResponseToChat 将 Responses 响应投影为 Chat Completion 响应。
func ResponsesResponseToChat(input []byte) ([]byte, error) {
	return defaultConverter.ConvertResponse(ProtocolResponses, ProtocolChatCompletions, input, Options{})
}

// ChatResponseToResponses 将 Chat Completion 响应转换为 Responses 响应。
func ChatResponseToResponses(input []byte) ([]byte, error) {
	return defaultConverter.ConvertResponse(ProtocolChatCompletions, ProtocolResponses, input, Options{})
}

// ResponsesToChatStream 将一个 Responses SSE 流转换为 Chat SSE 流。
func ResponsesToChatStream(reader io.Reader, writer io.Writer, emitReasoning bool) error {
	return defaultConverter.ConvertStream(ProtocolResponses, ProtocolChatCompletions, reader, writer, Options{EmitReasoning: emitReasoning})
}

// ChatToResponsesStream 将一个 Chat SSE 流转换为 Responses SSE 流。
func ChatToResponsesStream(reader io.Reader, writer io.Writer) error {
	return defaultConverter.ConvertStream(ProtocolChatCompletions, ProtocolResponses, reader, writer, Options{})
}

// ResponsesStreamToResponse 将 Responses SSE 流聚合为单个 Responses 响应。
func ResponsesStreamToResponse(reader io.Reader) ([]byte, error) {
	return defaultConverter.AggregateStream(ProtocolResponses, ProtocolResponses, reader, Options{})
}

// ChatStreamToResponse 将 Chat Completions SSE 流聚合为单个 Chat 响应。
func ChatStreamToResponse(reader io.Reader) ([]byte, error) {
	return defaultConverter.AggregateStream(ProtocolChatCompletions, ProtocolChatCompletions, reader, Options{})
}

// ResponsesStreamToChatResponse 将 Responses SSE 流转换并聚合为 Chat 响应。
func ResponsesStreamToChatResponse(reader io.Reader, emitReasoning bool) ([]byte, error) {
	return defaultConverter.AggregateStream(ProtocolResponses, ProtocolChatCompletions, reader, Options{EmitReasoning: emitReasoning})
}

// ChatStreamToResponsesResponse 将 Chat Completions SSE 流转换并聚合为 Responses 响应。
func ChatStreamToResponsesResponse(reader io.Reader) ([]byte, error) {
	return defaultConverter.AggregateStream(ProtocolChatCompletions, ProtocolResponses, reader, Options{})
}
