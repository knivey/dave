# Chat Template Kwargs Examples

This document shows how to use `chat_template_kwargs` for different AI providers and features.

## OpenRouter Reasoning

Enable reasoning for models that support step-by-step thinking:

```toml
[reasoning-chat]
description = "OpenRouter reasoning-enabled chat"
service = "openrouter"
model = "z-ai/glm-4.7-flash"
chat_template_kwargs = {reasoning = {enabled = true}}
system = "You are a helpful assistant that shows your reasoning process."
```

The model will include `reasoning_details` in its response, which are captured by the `ExtendedMessage` wrapper.

## Qwen3 Thinking Control

Control whether Qwen3 models use thinking mode:

```toml
# Disable thinking for faster responses
[no-thinking]
description = "Qwen3 without thinking mode"
service = "vllm"
model = "Qwen/Qwen3-8B"
chat_template_kwargs = {enable_thinking = false}
system = "You are a direct assistant who answers quickly."

# Enable thinking (default for Qwen3)
[with-thinking]
description = "Qwen3 with thinking mode"
service = "vllm"
model = "Qwen/Qwen3-8B"
chat_template_kwargs = {enable_thinking = true}
system = "You are a thoughtful assistant who shows your work."
```

## vLLM Custom Sampling

Add vLLM-specific sampling parameters:

```toml
[vllm-custom]
description = "vLLM with top_k sampling"
service = "vllm"
model = "some-model"
chat_template_kwargs = {top_k = 20}
system = "You are a helpful assistant."
```

## Combined with Standard Parameters

You can combine `chat_template_kwargs` with standard OpenAI parameters:

```toml
[advanced-chat]
description = "Advanced chat with custom parameters"
service = "openrouter"
model = "gpt-4o-mini"
temperature = 0.8
topp = 0.9
presencepenalty = 0.5
frequencypenalty = 0.3
chat_template_kwargs = {reasoning = {enabled = true}}
system = "You are an advanced assistant with creativity enabled."
```

## Accessing Provider-Specific Fields

When providers return extra fields (like `reasoning_details`), they're captured in the `ExtendedMessage` wrapper:

```go
var extMsg ExtendedMessage
json.Unmarshal(responseData, &extMsg)

// Check if reasoning_details is present
if extMsg.HasExtraField("reasoning_details") {
    var details []ReasoningStep
    extMsg.GetExtraField("reasoning_details", &details)
    // Use the reasoning details
}

// Get standard message for API calls
stdMsg := extMsg.ToChatCompletionMessage()
```

## Notes

- `chat_template_kwargs` is a flexible map that can contain any provider-specific parameters
- Parameters are passed directly to the underlying API via the `extra_body` mechanism
- The `ExtendedMessage` wrapper automatically captures any non-standard response fields
- Not all providers support all parameters - check your provider's documentation
