// Package toolcalling provides simulated tool calling support for clients
// (Claude Code, Codex, etc.) by sending the entire request JSON to M365 Copilot
// and parsing the simulated response (OpenAI chat.completion or Anthropic
// Messages shape) from a ```json code block.
//
// M365 Copilot backend does not natively support client-defined tools.
// This package bridges the gap by:
//   - Building a simulated prompt that embeds the full request JSON
//   - Parsing the simulated JSON response (tool_calls / tool_use blocks)
//   - Converting tool role messages (OpenAI) and tool_result blocks (Anthropic)
//     back into text for the M365 backend
package toolcalling

import (
	"encoding/json"
	"fmt"
)

// ToolDef represents a tool definition from the client request.
type ToolDef struct {
	Type     string      `json:"type"`
	Function ToolDefFunc `json:"function"`
	// Anthropic-style fields (flat, no "function" wrapper)
	Name        string         `json:"name,omitempty"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"input_schema,omitempty"`
}

// ToolDefFunc is the OpenAI-style function definition inside a tool.
type ToolDefFunc struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

// ToolCall represents a parsed tool call from the M365 response.
type ToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// toolCallIDCounter generates sequential tool call IDs.
var toolCallIDCounter int

// nextToolCallID returns a unique tool call ID.
func nextToolCallID() string {
	toolCallIDCounter++
	return fmt.Sprintf("call_%d", toolCallIDCounter)
}

// ToolName extracts the name from either OpenAI or Anthropic tool definition.
func ToolName(t *ToolDef) string {
	if t.Function.Name != "" {
		return t.Function.Name
	}
	return t.Name
}

// FormatToolResult converts a tool result (from the client) into text
// that the M365 backend can understand in the next message.
func FormatToolResult(toolCallID, toolName, result string) string {
	return fmt.Sprintf("[Tool Result for %s (call_id: %s)]\n%s", toolName, toolCallID, result)
}

// FormatAssistantToolCall converts a previous assistant tool call (from conversation
// history) into text that the M365 backend can understand.
func FormatAssistantToolCall(toolName string, arguments json.RawMessage) string {
	return fmt.Sprintf("[Previous Tool Call: %s]\nArguments: %s", toolName, string(arguments))
}
