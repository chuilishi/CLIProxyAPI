// Package claude provides response translation functionality for Claude Code API compatibility.
// This package handles the conversion of backend client responses into Claude Code-compatible
// Server-Sent Events (SSE) format, implementing a sophisticated state machine that manages
// different response types including text content, thinking processes, and function calls.
// The translation ensures proper sequencing of SSE events and maintains state across
// multiple response chunks to provide a seamless streaming experience.
package claude

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/cache"
	log "github.com/sirupsen/logrus"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// WebSearchResult represents a single web search result from grounding.
type WebSearchResult struct {
	Type             string `json:"type"`
	Title            string `json:"title"`
	URL              string `json:"url"`
	EncryptedContent string `json:"encrypted_content"`
	PageAge          *int   `json:"page_age"`
}

// WebSearchState holds the accumulated web search/grounding data during streaming.
type WebSearchState struct {
	ToolUseID         string                   // Unique ID for the server_tool_use block
	Query             string                   // The search query extracted from grounding metadata
	Results           []WebSearchResult        // Parsed grounding chunks as web search results
	Supports          []gjson.Result           // Raw grounding supports for building citations
	BufferedTextParts []string                 // Non-thinking text buffered until stream end
}

// Params holds parameters for response conversion and maintains state across streaming chunks.
// This structure tracks the current state of the response translation process to ensure
// proper sequencing of SSE events and transitions between different content types.
type Params struct {
	HasFirstResponse     bool   // Indicates if the initial message_start event has been sent
	ResponseType         int    // Current response type: 0=none, 1=content, 2=thinking, 3=function
	ResponseIndex        int    // Index counter for content blocks in the streaming response
	HasFinishReason      bool   // Tracks whether a finish reason has been observed
	FinishReason         string // The finish reason string returned by the provider
	HasUsageMetadata     bool   // Tracks whether usage metadata has been observed
	PromptTokenCount     int64  // Cached prompt token count from usage metadata
	CandidatesTokenCount int64  // Cached candidate token count from usage metadata
	ThoughtsTokenCount   int64  // Cached thinking token count from usage metadata
	TotalTokenCount      int64  // Cached total token count from usage metadata
	CachedTokenCount     int64  // Cached content token count (indicates prompt caching)
	HasSentFinalEvents   bool   // Indicates if final content/message events have been sent
	HasToolUse           bool   // Indicates if tool use was observed in the stream
	HasContent           bool   // Tracks whether any content (text, thinking, or tool use) has been output

	// Signature caching support
	SessionID           string          // Session ID derived from request for signature caching
	CurrentThinkingText strings.Builder // Accumulates thinking text for signature caching

	// Web search (grounding) mode support
	WebSearchMode bool           // Indicates if web search grounding is active
	WebSearch     WebSearchState // Accumulated grounding data
}

// toolUseIDCounter provides a process-wide unique counter for tool use identifiers.
var toolUseIDCounter uint64

// processThinkingPart processes a thinking part and outputs the appropriate SSE events.
// This is extracted to be reusable in both normal and web search modes.
func processThinkingPart(params *Params, output *string, partResult, partTextResult gjson.Result) {
	if thoughtSignature := partResult.Get("thoughtSignature"); thoughtSignature.Exists() && thoughtSignature.String() != "" {
		log.Debug("Branch: signature_delta (web search mode)")

		if params.SessionID != "" && params.CurrentThinkingText.Len() > 0 {
			cache.CacheSignature(params.SessionID, params.CurrentThinkingText.String(), thoughtSignature.String())
			log.Debugf("Cached signature for thinking block (sessionID=%s, textLen=%d)", params.SessionID, params.CurrentThinkingText.Len())
			params.CurrentThinkingText.Reset()
		}

		*output = *output + "event: content_block_delta\n"
		data, _ := sjson.Set(fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"signature_delta","signature":""}}`, params.ResponseIndex), "delta.signature", thoughtSignature.String())
		*output = *output + fmt.Sprintf("data: %s\n\n\n", data)
		params.HasContent = true
	} else if params.ResponseType == 2 { // Continue existing thinking block
		params.CurrentThinkingText.WriteString(partTextResult.String())
		*output = *output + "event: content_block_delta\n"
		data, _ := sjson.Set(fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"thinking_delta","thinking":""}}`, params.ResponseIndex), "delta.thinking", partTextResult.String())
		*output = *output + fmt.Sprintf("data: %s\n\n\n", data)
		params.HasContent = true
	} else {
		// Transition to thinking state
		if params.ResponseType != 0 {
			*output = *output + "event: content_block_stop\n"
			*output = *output + fmt.Sprintf(`data: {"type":"content_block_stop","index":%d}`, params.ResponseIndex)
			*output = *output + "\n\n\n"
			params.ResponseIndex++
		}

		*output = *output + "event: content_block_start\n"
		*output = *output + fmt.Sprintf(`data: {"type":"content_block_start","index":%d,"content_block":{"type":"thinking","thinking":""}}`, params.ResponseIndex)
		*output = *output + "\n\n\n"
		*output = *output + "event: content_block_delta\n"
		data, _ := sjson.Set(fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"thinking_delta","thinking":""}}`, params.ResponseIndex), "delta.thinking", partTextResult.String())
		*output = *output + fmt.Sprintf("data: %s\n\n\n", data)
		params.ResponseType = 2
		params.HasContent = true
		params.CurrentThinkingText.Reset()
		params.CurrentThinkingText.WriteString(partTextResult.String())
	}
}

// ConvertAntigravityResponseToClaude performs sophisticated streaming response format conversion.
// This function implements a complex state machine that translates backend client responses
// into Claude Code-compatible Server-Sent Events (SSE) format. It manages different response types
// and handles state transitions between content blocks, thinking processes, and function calls.
//
// Response type states: 0=none, 1=content, 2=thinking, 3=function
// The function maintains state across multiple calls to ensure proper SSE event sequencing.
//
// Parameters:
//   - ctx: The context for the request, used for cancellation and timeout handling
//   - modelName: The name of the model being used for the response (unused in current implementation)
//   - rawJSON: The raw JSON response from the Gemini CLI API
//   - param: A pointer to a parameter object for maintaining state between calls
//
// Returns:
//   - []string: A slice of strings, each containing a Claude Code-compatible JSON response
func ConvertAntigravityResponseToClaude(_ context.Context, _ string, originalRequestRawJSON, requestRawJSON, rawJSON []byte, param *any) []string {
	if *param == nil {
		*param = &Params{
			HasFirstResponse: false,
			ResponseType:     0,
			ResponseIndex:    0,
			SessionID:        deriveSessionID(originalRequestRawJSON),
		}
	}

	params := (*param).(*Params)

	if bytes.Equal(rawJSON, []byte("[DONE]")) {
		output := ""
		// Only send final events if we have actually output content
		if params.HasContent {
			appendFinalEvents(params, &output, true)
			return []string{
				output + "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n\n",
			}
		}
		return []string{}
	}

	output := ""

	// Initialize the streaming session with a message_start event
	// This is only sent for the very first response chunk to establish the streaming session
	if !params.HasFirstResponse {
		output = "event: message_start\n"

		// Create the initial message structure with default values according to Claude Code API specification
		// This follows the Claude Code API specification for streaming message initialization
		messageStartTemplate := `{"type": "message_start", "message": {"id": "msg_1nZdL29xx5MUA1yADyHTEsnR8uuvGzszyY", "type": "message", "role": "assistant", "content": [], "model": "claude-3-5-sonnet-20241022", "stop_reason": null, "stop_sequence": null, "usage": {"input_tokens": 0, "output_tokens": 0}}}`

		// Use cpaUsageMetadata within the message_start event for Claude.
		if promptTokenCount := gjson.GetBytes(rawJSON, "response.cpaUsageMetadata.promptTokenCount"); promptTokenCount.Exists() {
			messageStartTemplate, _ = sjson.Set(messageStartTemplate, "message.usage.input_tokens", promptTokenCount.Int())
		}
		if candidatesTokenCount := gjson.GetBytes(rawJSON, "response.cpaUsageMetadata.candidatesTokenCount"); candidatesTokenCount.Exists() {
			messageStartTemplate, _ = sjson.Set(messageStartTemplate, "message.usage.output_tokens", candidatesTokenCount.Int())
		}

		// Override default values with actual response metadata if available from the Gemini CLI response
		if modelVersionResult := gjson.GetBytes(rawJSON, "response.modelVersion"); modelVersionResult.Exists() {
			messageStartTemplate, _ = sjson.Set(messageStartTemplate, "message.model", modelVersionResult.String())
		}
		if responseIDResult := gjson.GetBytes(rawJSON, "response.responseId"); responseIDResult.Exists() {
			messageStartTemplate, _ = sjson.Set(messageStartTemplate, "message.id", responseIDResult.String())
		}
		output = output + fmt.Sprintf("data: %s\n\n\n", messageStartTemplate)

		params.HasFirstResponse = true
	}

	// Check for grounding data (web search mode)
	if !params.WebSearchMode && hasGroundingData(rawJSON) {
		params.WebSearchMode = true
		params.WebSearch.ToolUseID = makeServerToolUseID()
		log.Debug("Web search grounding mode activated")
	}

	// Update grounding state if in web search mode
	if params.WebSearchMode {
		updateWebSearchStateFromResponse(params, rawJSON)
	}

	// Process the response parts array from the backend client
	// Each part can contain text content, thinking content, or function calls
	partsResult := gjson.GetBytes(rawJSON, "response.candidates.0.content.parts")
	if partsResult.IsArray() {
		partResults := partsResult.Array()

		// In web search mode, buffer non-thinking text and output thinking in real-time
		if params.WebSearchMode {
			for i := 0; i < len(partResults); i++ {
				partResult := partResults[i]
				partTextResult := partResult.Get("text")

				if partTextResult.Exists() {
					if partResult.Get("thought").Bool() {
						// Output thinking content in real-time (same as normal mode)
						processThinkingPart(params, &output, partResult, partTextResult)
					} else {
						// Buffer non-thinking text for later output
						params.WebSearch.BufferedTextParts = append(params.WebSearch.BufferedTextParts, partTextResult.String())
					}
				}
			}
		} else {
			// Normal mode: process all parts normally
			for i := 0; i < len(partResults); i++ {
			partResult := partResults[i]

			// Extract the different types of content from each part
			partTextResult := partResult.Get("text")
			functionCallResult := partResult.Get("functionCall")

			// Handle text content (both regular content and thinking)
			if partTextResult.Exists() {
				// Process thinking content (internal reasoning)
				if partResult.Get("thought").Bool() {
					if thoughtSignature := partResult.Get("thoughtSignature"); thoughtSignature.Exists() && thoughtSignature.String() != "" {
						log.Debug("Branch: signature_delta")

						if params.SessionID != "" && params.CurrentThinkingText.Len() > 0 {
							cache.CacheSignature(params.SessionID, params.CurrentThinkingText.String(), thoughtSignature.String())
							log.Debugf("Cached signature for thinking block (sessionID=%s, textLen=%d)", params.SessionID, params.CurrentThinkingText.Len())
							params.CurrentThinkingText.Reset()
						}

						output = output + "event: content_block_delta\n"
						data, _ := sjson.Set(fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"signature_delta","signature":""}}`, params.ResponseIndex), "delta.signature", thoughtSignature.String())
						output = output + fmt.Sprintf("data: %s\n\n\n", data)
						params.HasContent = true
					} else if params.ResponseType == 2 { // Continue existing thinking block if already in thinking state
						params.CurrentThinkingText.WriteString(partTextResult.String())
						output = output + "event: content_block_delta\n"
						data, _ := sjson.Set(fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"thinking_delta","thinking":""}}`, params.ResponseIndex), "delta.thinking", partTextResult.String())
						output = output + fmt.Sprintf("data: %s\n\n\n", data)
						params.HasContent = true
					} else {
						// Transition from another state to thinking
						// First, close any existing content block
						if params.ResponseType != 0 {
							if params.ResponseType == 2 {
								// output = output + "event: content_block_delta\n"
								// output = output + fmt.Sprintf(`data: {"type":"content_block_delta","index":%d,"delta":{"type":"signature_delta","signature":null}}`, params.ResponseIndex)
								// output = output + "\n\n\n"
							}
							output = output + "event: content_block_stop\n"
							output = output + fmt.Sprintf(`data: {"type":"content_block_stop","index":%d}`, params.ResponseIndex)
							output = output + "\n\n\n"
							params.ResponseIndex++
						}

						// Start a new thinking content block
						output = output + "event: content_block_start\n"
						output = output + fmt.Sprintf(`data: {"type":"content_block_start","index":%d,"content_block":{"type":"thinking","thinking":""}}`, params.ResponseIndex)
						output = output + "\n\n\n"
						output = output + "event: content_block_delta\n"
						data, _ := sjson.Set(fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"thinking_delta","thinking":""}}`, params.ResponseIndex), "delta.thinking", partTextResult.String())
						output = output + fmt.Sprintf("data: %s\n\n\n", data)
						params.ResponseType = 2 // Set state to thinking
						params.HasContent = true
						// Start accumulating thinking text for signature caching
						params.CurrentThinkingText.Reset()
						params.CurrentThinkingText.WriteString(partTextResult.String())
					}
				} else {
					finishReasonResult := gjson.GetBytes(rawJSON, "response.candidates.0.finishReason")
					if partTextResult.String() != "" || !finishReasonResult.Exists() {
						// Process regular text content (user-visible output)
						// Continue existing text block if already in content state
						if params.ResponseType == 1 {
							output = output + "event: content_block_delta\n"
							data, _ := sjson.Set(fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"text_delta","text":""}}`, params.ResponseIndex), "delta.text", partTextResult.String())
							output = output + fmt.Sprintf("data: %s\n\n\n", data)
							params.HasContent = true
						} else {
							// Transition from another state to text content
							// First, close any existing content block
							if params.ResponseType != 0 {
								if params.ResponseType == 2 {
									// output = output + "event: content_block_delta\n"
									// output = output + fmt.Sprintf(`data: {"type":"content_block_delta","index":%d,"delta":{"type":"signature_delta","signature":null}}`, params.ResponseIndex)
									// output = output + "\n\n\n"
								}
								output = output + "event: content_block_stop\n"
								output = output + fmt.Sprintf(`data: {"type":"content_block_stop","index":%d}`, params.ResponseIndex)
								output = output + "\n\n\n"
								params.ResponseIndex++
							}
							if partTextResult.String() != "" {
								// Start a new text content block
								output = output + "event: content_block_start\n"
								output = output + fmt.Sprintf(`data: {"type":"content_block_start","index":%d,"content_block":{"type":"text","text":""}}`, params.ResponseIndex)
								output = output + "\n\n\n"
								output = output + "event: content_block_delta\n"
								data, _ := sjson.Set(fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"text_delta","text":""}}`, params.ResponseIndex), "delta.text", partTextResult.String())
								output = output + fmt.Sprintf("data: %s\n\n\n", data)
								params.ResponseType = 1 // Set state to content
								params.HasContent = true
							}
						}
					}
				}
			} else if functionCallResult.Exists() {
				// Handle function/tool calls from the AI model
				// This processes tool usage requests and formats them for Claude Code API compatibility
				params.HasToolUse = true
				fcName := functionCallResult.Get("name").String()

				// Handle state transitions when switching to function calls
				// Close any existing function call block first
				if params.ResponseType == 3 {
					output = output + "event: content_block_stop\n"
					output = output + fmt.Sprintf(`data: {"type":"content_block_stop","index":%d}`, params.ResponseIndex)
					output = output + "\n\n\n"
					params.ResponseIndex++
					params.ResponseType = 0
				}

				// Special handling for thinking state transition
				if params.ResponseType == 2 {
					// output = output + "event: content_block_delta\n"
					// output = output + fmt.Sprintf(`data: {"type":"content_block_delta","index":%d,"delta":{"type":"signature_delta","signature":null}}`, params.ResponseIndex)
					// output = output + "\n\n\n"
				}

				// Close any other existing content block
				if params.ResponseType != 0 {
					output = output + "event: content_block_stop\n"
					output = output + fmt.Sprintf(`data: {"type":"content_block_stop","index":%d}`, params.ResponseIndex)
					output = output + "\n\n\n"
					params.ResponseIndex++
				}

				// Start a new tool use content block
				// This creates the structure for a function call in Claude Code format
				output = output + "event: content_block_start\n"

				// Create the tool use block with unique ID and function details
				data := fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":"tool_use","id":"","name":"","input":{}}}`, params.ResponseIndex)
				data, _ = sjson.Set(data, "content_block.id", fmt.Sprintf("%s-%d-%d", fcName, time.Now().UnixNano(), atomic.AddUint64(&toolUseIDCounter, 1)))
				data, _ = sjson.Set(data, "content_block.name", fcName)
				output = output + fmt.Sprintf("data: %s\n\n\n", data)

				if fcArgsResult := functionCallResult.Get("args"); fcArgsResult.Exists() {
					output = output + "event: content_block_delta\n"
					data, _ = sjson.Set(fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"input_json_delta","partial_json":""}}`, params.ResponseIndex), "delta.partial_json", fcArgsResult.Raw)
					output = output + fmt.Sprintf("data: %s\n\n\n", data)
				}
				params.ResponseType = 3
				params.HasContent = true
			}
		}
		} // Close the else branch for normal mode
	}

	if finishReasonResult := gjson.GetBytes(rawJSON, "response.candidates.0.finishReason"); finishReasonResult.Exists() {
		params.HasFinishReason = true
		params.FinishReason = finishReasonResult.String()
	}

	if usageResult := gjson.GetBytes(rawJSON, "response.usageMetadata"); usageResult.Exists() {
		params.HasUsageMetadata = true
		params.CachedTokenCount = usageResult.Get("cachedContentTokenCount").Int()
		params.PromptTokenCount = usageResult.Get("promptTokenCount").Int() - params.CachedTokenCount
		params.CandidatesTokenCount = usageResult.Get("candidatesTokenCount").Int()
		params.ThoughtsTokenCount = usageResult.Get("thoughtsTokenCount").Int()
		params.TotalTokenCount = usageResult.Get("totalTokenCount").Int()
		if params.CandidatesTokenCount == 0 && params.TotalTokenCount > 0 {
			params.CandidatesTokenCount = params.TotalTokenCount - params.PromptTokenCount - params.ThoughtsTokenCount
			if params.CandidatesTokenCount < 0 {
				params.CandidatesTokenCount = 0
			}
		}
	}

	if params.HasUsageMetadata && params.HasFinishReason {
		appendFinalEvents(params, &output, false)
	}

	return []string{output}
}

func appendFinalEvents(params *Params, output *string, force bool) {
	if params.HasSentFinalEvents {
		return
	}

	if !params.HasUsageMetadata && !force {
		return
	}

	// Only send final events if we have actually output content
	if !params.HasContent && !params.WebSearchMode {
		return
	}

	// In web search mode, emit the web search blocks before the final message
	if params.WebSearchMode {
		emitWebSearchBlocks(params, output)
		params.HasContent = true // Ensure we mark content as present
	} else {
		// Normal mode: close any open content block
		if params.ResponseType != 0 {
			*output = *output + "event: content_block_stop\n"
			*output = *output + fmt.Sprintf(`data: {"type":"content_block_stop","index":%d}`, params.ResponseIndex)
			*output = *output + "\n\n\n"
			params.ResponseType = 0
		}
	}

	stopReason := resolveStopReason(params)
	usageOutputTokens := params.CandidatesTokenCount + params.ThoughtsTokenCount
	if usageOutputTokens == 0 && params.TotalTokenCount > 0 {
		usageOutputTokens = params.TotalTokenCount - params.PromptTokenCount
		if usageOutputTokens < 0 {
			usageOutputTokens = 0
		}
	}

	*output = *output + "event: message_delta\n"
	*output = *output + "data: "
	delta := fmt.Sprintf(`{"type":"message_delta","delta":{"stop_reason":"%s","stop_sequence":null},"usage":{"input_tokens":%d,"output_tokens":%d}}`, stopReason, params.PromptTokenCount, usageOutputTokens)
	// Add cache_read_input_tokens if cached tokens are present (indicates prompt caching is working)
	if params.CachedTokenCount > 0 {
		var err error
		delta, err = sjson.Set(delta, "usage.cache_read_input_tokens", params.CachedTokenCount)
		if err != nil {
			log.Warnf("antigravity claude response: failed to set cache_read_input_tokens: %v", err)
		}
	}
	*output = *output + delta + "\n\n\n"

	params.HasSentFinalEvents = true
}

func resolveStopReason(params *Params) string {
	if params.HasToolUse {
		return "tool_use"
	}

	switch params.FinishReason {
	case "MAX_TOKENS":
		return "max_tokens"
	case "STOP", "FINISH_REASON_UNSPECIFIED", "UNKNOWN":
		return "end_turn"
	}

	return "end_turn"
}

// ConvertAntigravityResponseToClaudeNonStream converts a non-streaming Gemini CLI response to a non-streaming Claude response.
//
// Parameters:
//   - ctx: The context for the request.
//   - modelName: The name of the model.
//   - rawJSON: The raw JSON response from the Gemini CLI API.
//   - param: A pointer to a parameter object for the conversion.
//
// Returns:
//   - string: A Claude-compatible JSON response.
func ConvertAntigravityResponseToClaudeNonStream(_ context.Context, _ string, originalRequestRawJSON, requestRawJSON, rawJSON []byte, _ *any) string {
	_ = originalRequestRawJSON
	_ = requestRawJSON

	root := gjson.ParseBytes(rawJSON)
	promptTokens := root.Get("response.usageMetadata.promptTokenCount").Int()
	candidateTokens := root.Get("response.usageMetadata.candidatesTokenCount").Int()
	thoughtTokens := root.Get("response.usageMetadata.thoughtsTokenCount").Int()
	totalTokens := root.Get("response.usageMetadata.totalTokenCount").Int()
	cachedTokens := root.Get("response.usageMetadata.cachedContentTokenCount").Int()
	outputTokens := candidateTokens + thoughtTokens
	if outputTokens == 0 && totalTokens > 0 {
		outputTokens = totalTokens - promptTokens
		if outputTokens < 0 {
			outputTokens = 0
		}
	}

	responseJSON := `{"id":"","type":"message","role":"assistant","model":"","content":null,"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0}}`
	responseJSON, _ = sjson.Set(responseJSON, "id", root.Get("response.responseId").String())
	responseJSON, _ = sjson.Set(responseJSON, "model", root.Get("response.modelVersion").String())
	responseJSON, _ = sjson.Set(responseJSON, "usage.input_tokens", promptTokens)
	responseJSON, _ = sjson.Set(responseJSON, "usage.output_tokens", outputTokens)
	// Add cache_read_input_tokens if cached tokens are present (indicates prompt caching is working)
	if cachedTokens > 0 {
		var err error
		responseJSON, err = sjson.Set(responseJSON, "usage.cache_read_input_tokens", cachedTokens)
		if err != nil {
			log.Warnf("antigravity claude response: failed to set cache_read_input_tokens: %v", err)
		}
	}

	contentArrayInitialized := false
	ensureContentArray := func() {
		if contentArrayInitialized {
			return
		}
		responseJSON, _ = sjson.SetRaw(responseJSON, "content", "[]")
		contentArrayInitialized = true
	}

	parts := root.Get("response.candidates.0.content.parts")
	textBuilder := strings.Builder{}
	thinkingBuilder := strings.Builder{}
	thinkingSignature := ""
	toolIDCounter := 0
	hasToolCall := false

	flushText := func() {
		if textBuilder.Len() == 0 {
			return
		}
		ensureContentArray()
		block := `{"type":"text","text":""}`
		block, _ = sjson.Set(block, "text", textBuilder.String())
		responseJSON, _ = sjson.SetRaw(responseJSON, "content.-1", block)
		textBuilder.Reset()
	}

	flushThinking := func() {
		if thinkingBuilder.Len() == 0 && thinkingSignature == "" {
			return
		}
		ensureContentArray()
		block := `{"type":"thinking","thinking":""}`
		block, _ = sjson.Set(block, "thinking", thinkingBuilder.String())
		if thinkingSignature != "" {
			block, _ = sjson.Set(block, "signature", thinkingSignature)
		}
		responseJSON, _ = sjson.SetRaw(responseJSON, "content.-1", block)
		thinkingBuilder.Reset()
		thinkingSignature = ""
	}

	if parts.IsArray() {
		for _, part := range parts.Array() {
			isThought := part.Get("thought").Bool()
			if isThought {
				sig := part.Get("thoughtSignature")
				if !sig.Exists() {
					sig = part.Get("thought_signature")
				}
				if sig.Exists() && sig.String() != "" {
					thinkingSignature = sig.String()
				}
			}

			if text := part.Get("text"); text.Exists() && text.String() != "" {
				if isThought {
					flushText()
					thinkingBuilder.WriteString(text.String())
					continue
				}
				flushThinking()
				textBuilder.WriteString(text.String())
				continue
			}

			if functionCall := part.Get("functionCall"); functionCall.Exists() {
				flushThinking()
				flushText()
				hasToolCall = true

				name := functionCall.Get("name").String()
				toolIDCounter++
				toolBlock := `{"type":"tool_use","id":"","name":"","input":{}}`
				toolBlock, _ = sjson.Set(toolBlock, "id", fmt.Sprintf("tool_%d", toolIDCounter))
				toolBlock, _ = sjson.Set(toolBlock, "name", name)

				if args := functionCall.Get("args"); args.Exists() && args.Raw != "" && gjson.Valid(args.Raw) && args.IsObject() {
					toolBlock, _ = sjson.SetRaw(toolBlock, "input", args.Raw)
				}

				ensureContentArray()
				responseJSON, _ = sjson.SetRaw(responseJSON, "content.-1", toolBlock)
				continue
			}
		}
	}

	flushThinking()

	// Check for grounding data (web search mode)
	webSearchMode := hasGroundingData(rawJSON)
	if webSearchMode {
		// In web search mode, the text is buffered and output after web search blocks
		bufferedText := textBuilder.String()
		textBuilder.Reset()

		// Parse grounding data
		groundingMetadata := root.Get("response.candidates.0.groundingMetadata")
		chunks := groundingMetadata.Get("groundingChunks").Array()
		supports := groundingMetadata.Get("groundingSupports").Array()
		query := groundingMetadata.Get("searchEntryPoint.renderedContent").String()
		if query == "" {
			query = groundingMetadata.Get("webSearchQueries.0").String()
		}

		// Generate tool use ID
		toolUseID := makeServerToolUseID()
		results := toWebSearchResults(chunks)

		// 1. server_tool_use block
		serverToolBlock := `{"type":"server_tool_use","id":"","name":"web_search","input":{}}`
		serverToolBlock, _ = sjson.Set(serverToolBlock, "id", toolUseID)
		serverToolBlock, _ = sjson.Set(serverToolBlock, "input.query", query)
		ensureContentArray()
		responseJSON, _ = sjson.SetRaw(responseJSON, "content.-1", serverToolBlock)

		// 2. web_search_tool_result block
		resultsBytes, _ := json.Marshal(results)
		toolResultBlock := `{"type":"web_search_tool_result","tool_use_id":"","content":[]}`
		toolResultBlock, _ = sjson.Set(toolResultBlock, "tool_use_id", toolUseID)
		toolResultBlock, _ = sjson.SetRaw(toolResultBlock, "content", string(resultsBytes))
		responseJSON, _ = sjson.SetRaw(responseJSON, "content.-1", toolResultBlock)

		// 3. Citation blocks
		for _, support := range supports {
			citation, ok := buildCitationFromSupport(results, support)
			if !ok {
				continue
			}
			citedText := support.Get("segment.text").String()
			textBlock := `{"type":"text","text":"","citations":[]}`
			textBlock, _ = sjson.Set(textBlock, "text", citedText)
			textBlock, _ = sjson.SetRaw(textBlock, "citations.-1", citation)
			responseJSON, _ = sjson.SetRaw(responseJSON, "content.-1", textBlock)
		}

		// 4. Final text block with main content
		if bufferedText != "" {
			block := `{"type":"text","text":""}`
			block, _ = sjson.Set(block, "text", bufferedText)
			responseJSON, _ = sjson.SetRaw(responseJSON, "content.-1", block)
		}
	} else {
		flushText()
	}

	stopReason := "end_turn"
	if hasToolCall {
		stopReason = "tool_use"
	} else {
		if finish := root.Get("response.candidates.0.finishReason"); finish.Exists() {
			switch finish.String() {
			case "MAX_TOKENS":
				stopReason = "max_tokens"
			case "STOP", "FINISH_REASON_UNSPECIFIED", "UNKNOWN":
				stopReason = "end_turn"
			default:
				stopReason = "end_turn"
			}
		}
	}
	responseJSON, _ = sjson.Set(responseJSON, "stop_reason", stopReason)

	if promptTokens == 0 && outputTokens == 0 {
		if usageMeta := root.Get("response.usageMetadata"); !usageMeta.Exists() {
			responseJSON, _ = sjson.Delete(responseJSON, "usage")
		}
	}

	return responseJSON
}

func ClaudeTokenCount(ctx context.Context, count int64) string {
	return fmt.Sprintf(`{"input_tokens":%d}`, count)
}

// makeServerToolUseID generates a unique ID for server_tool_use blocks.
func makeServerToolUseID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "srvtoolu_" + hex.EncodeToString(b)
}

// stableEncryptedContent creates a stable base64-encoded JSON for encrypted_content field.
func stableEncryptedContent(url, title, citedText string) string {
	payload := map[string]string{"url": url, "title": title}
	if citedText != "" {
		payload["cited_text"] = citedText
	}
	data, _ := json.Marshal(payload)
	return base64.StdEncoding.EncodeToString(data)
}

// toWebSearchResults converts grounding chunks to WebSearchResult array.
func toWebSearchResults(chunks []gjson.Result) []WebSearchResult {
	var results []WebSearchResult
	for _, chunk := range chunks {
		web := chunk.Get("web")
		if !web.Exists() {
			continue
		}
		uri := web.Get("uri").String()
		title := web.Get("title").String()
		if title == "" {
			title = web.Get("domain").String()
		}
		if uri == "" && title == "" {
			continue
		}
		results = append(results, WebSearchResult{
			Type:             "web_search_result",
			Title:            title,
			URL:              uri,
			EncryptedContent: stableEncryptedContent(uri, title, ""),
			PageAge:          nil,
		})
	}
	return results
}

// hasGroundingData checks if the candidate contains any grounding metadata.
func hasGroundingData(rawJSON []byte) bool {
	candidate := gjson.GetBytes(rawJSON, "response.candidates.0")
	if !candidate.Exists() {
		return false
	}
	if candidate.Get("groundingMetadata").Exists() {
		return true
	}
	if candidate.Get("groundingChunks").Exists() {
		return true
	}
	if candidate.Get("groundingSupports").Exists() {
		return true
	}
	return false
}

// updateWebSearchStateFromResponse extracts grounding data from the response and updates state.
func updateWebSearchStateFromResponse(params *Params, rawJSON []byte) {
	candidate := gjson.GetBytes(rawJSON, "response.candidates.0")
	if !candidate.Exists() {
		return
	}

	// Extract search query
	queries := candidate.Get("groundingMetadata.webSearchQueries")
	if queries.IsArray() && len(queries.Array()) > 0 {
		if q := queries.Array()[0].String(); q != "" {
			params.WebSearch.Query = q
		}
	}

	// Extract grounding chunks (search results)
	chunks := candidate.Get("groundingChunks")
	if !chunks.Exists() || !chunks.IsArray() {
		chunks = candidate.Get("groundingMetadata.groundingChunks")
	}
	if chunks.Exists() && chunks.IsArray() {
		params.WebSearch.Results = toWebSearchResults(chunks.Array())
	}

	// Extract grounding supports (for citations)
	supports := candidate.Get("groundingSupports")
	if !supports.Exists() || !supports.IsArray() {
		supports = candidate.Get("groundingMetadata.groundingSupports")
	}
	if supports.Exists() && supports.IsArray() {
		params.WebSearch.Supports = supports.Array()
	}
}

// buildCitationFromSupport creates a citation block from a grounding support entry.
func buildCitationFromSupport(results []WebSearchResult, support gjson.Result) (string, bool) {
	citedText := support.Get("segment.text").String()
	if citedText == "" {
		return "", false
	}

	indices := support.Get("groundingChunkIndices")
	if !indices.Exists() || !indices.IsArray() || len(indices.Array()) == 0 {
		return "", false
	}

	idx := int(indices.Array()[0].Int())
	if idx < 0 || idx >= len(results) {
		return "", false
	}

	result := results[idx]
	citation := `{"type":"web_search_result_location","cited_text":"","url":"","title":"","encrypted_index":""}`
	citation, _ = sjson.Set(citation, "cited_text", citedText)
	citation, _ = sjson.Set(citation, "url", result.URL)
	citation, _ = sjson.Set(citation, "title", result.Title)
	citation, _ = sjson.Set(citation, "encrypted_index", stableEncryptedContent(result.URL, result.Title, citedText))

	return citation, true
}

// emitWebSearchBlocks outputs the web search SSE blocks at stream end.
// This includes: server_tool_use, web_search_tool_result, citations, and buffered text.
func emitWebSearchBlocks(params *Params, output *string) {
	// Ensure current block is closed
	if params.ResponseType != 0 {
		*output = *output + "event: content_block_stop\n"
		*output = *output + fmt.Sprintf(`data: {"type":"content_block_stop","index":%d}`, params.ResponseIndex)
		*output = *output + "\n\n\n"
		params.ResponseIndex++
		params.ResponseType = 0
	}

	// Ensure we have a tool use ID
	if params.WebSearch.ToolUseID == "" {
		params.WebSearch.ToolUseID = makeServerToolUseID()
	}

	// 1. server_tool_use block
	*output = *output + "event: content_block_start\n"
	serverToolBlock := fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":"server_tool_use","id":"%s","name":"web_search","input":{}}}`,
		params.ResponseIndex, params.WebSearch.ToolUseID)
	*output = *output + fmt.Sprintf("data: %s\n\n\n", serverToolBlock)

	// Send the query as input
	queryJSON := `{"query":""}`
	queryJSON, _ = sjson.Set(queryJSON, "query", params.WebSearch.Query)
	*output = *output + "event: content_block_delta\n"
	inputDelta := fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"input_json_delta","partial_json":""}}`, params.ResponseIndex)
	inputDelta, _ = sjson.Set(inputDelta, "delta.partial_json", queryJSON)
	*output = *output + fmt.Sprintf("data: %s\n\n\n", inputDelta)

	*output = *output + "event: content_block_stop\n"
	*output = *output + fmt.Sprintf(`data: {"type":"content_block_stop","index":%d}`, params.ResponseIndex)
	*output = *output + "\n\n\n"
	params.ResponseIndex++

	// 2. web_search_tool_result block
	resultsJSON, _ := json.Marshal(params.WebSearch.Results)
	*output = *output + "event: content_block_start\n"
	toolResultBlock := fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":"web_search_tool_result","tool_use_id":"%s","content":%s}}`,
		params.ResponseIndex, params.WebSearch.ToolUseID, string(resultsJSON))
	*output = *output + fmt.Sprintf("data: %s\n\n\n", toolResultBlock)

	*output = *output + "event: content_block_stop\n"
	*output = *output + fmt.Sprintf(`data: {"type":"content_block_stop","index":%d}`, params.ResponseIndex)
	*output = *output + "\n\n\n"
	params.ResponseIndex++

	// 3. Citation blocks
	for _, support := range params.WebSearch.Supports {
		citation, ok := buildCitationFromSupport(params.WebSearch.Results, support)
		if !ok {
			continue
		}

		*output = *output + "event: content_block_start\n"
		citationBlock := fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":"text","text":"","citations":[]}}`, params.ResponseIndex)
		*output = *output + fmt.Sprintf("data: %s\n\n\n", citationBlock)

		*output = *output + "event: content_block_delta\n"
		citationDelta := fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"citations_delta","citation":null}}`, params.ResponseIndex)
		citationDelta, _ = sjson.SetRaw(citationDelta, "delta.citation", citation)
		*output = *output + fmt.Sprintf("data: %s\n\n\n", citationDelta)

		*output = *output + "event: content_block_stop\n"
		*output = *output + fmt.Sprintf(`data: {"type":"content_block_stop","index":%d}`, params.ResponseIndex)
		*output = *output + "\n\n\n"
		params.ResponseIndex++
	}

	// 4. Final text block with buffered content
	if len(params.WebSearch.BufferedTextParts) > 0 {
		*output = *output + "event: content_block_start\n"
		*output = *output + fmt.Sprintf(`data: {"type":"content_block_start","index":%d,"content_block":{"type":"text","text":""}}`, params.ResponseIndex)
		*output = *output + "\n\n\n"

		for _, text := range params.WebSearch.BufferedTextParts {
			if text == "" {
				continue
			}
			*output = *output + "event: content_block_delta\n"
			textDelta := fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"text_delta","text":""}}`, params.ResponseIndex)
			textDelta, _ = sjson.Set(textDelta, "delta.text", text)
			*output = *output + fmt.Sprintf("data: %s\n\n\n", textDelta)
		}

		*output = *output + "event: content_block_stop\n"
		*output = *output + fmt.Sprintf(`data: {"type":"content_block_stop","index":%d}`, params.ResponseIndex)
		*output = *output + "\n\n\n"
		params.ResponseIndex++
	}

	params.ResponseType = 0
}
