package ir

// Provider 是适配器所属的厂商或兼容实现。
type Provider string

const (
	ProviderOpenAI    Provider = "openai"
	ProviderAnthropic Provider = "anthropic"
	ProviderGoogle    Provider = "google"
)

// Capability 表示适配器可以声明的能力集合。
type Capability string

const (
	CapabilityStreaming       Capability = "streaming"
	CapabilityTools           Capability = "tools"
	CapabilityCustomTools     Capability = "custom_tools"
	CapabilityReasoning       Capability = "reasoning"
	CapabilityEncryptedReason Capability = "encrypted_reasoning"
	CapabilityVision          Capability = "vision"
	CapabilityAudio           Capability = "audio"
	CapabilityStructuredText  Capability = "structured_text"
	CapabilityBuiltInTools    Capability = "built_in_tools"
	CapabilityConversation    Capability = "conversation_state"
)
