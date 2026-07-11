// Package toolcalling — simulated mode (edlaver-style).
//
// Instead of injecting tool definitions and parsing fenced code blocks, the
// simulated mode sends the ENTIRE OpenAI chat completion request (as JSON) to
// M365 Copilot and asks it to produce a valid OpenAI chat completion response
// inside a single ```json code block. The proxy then extracts that JSON,
// scores candidate objects to pick the best chat-completion-shaped one, and
// parses choices[0].message.tool_calls out of it.
//
// This is a port of edlaver/m365-copilot-bun-proxy's simulated transform mode.
package toolcalling

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/KilimcininKorOglu/M365Bridge/pkg/logging"
)

// BuildSimulatedPrompt constructs the prompt sent to M365 Copilot in simulated
// mode. It embeds the full OpenAI request JSON and instructs the model to
// return a single ```json block containing a valid chat.completion response.
//
// requestJSON is the serialized OpenAI /v1/chat/completions request body.
// hasTools indicates whether the request carries client-defined tools.
// toolChoice is the raw tool_choice value ("auto", "required", "none", or "").
func BuildSimulatedPrompt(requestJSON string, hasTools bool, toolChoice string) string {
	lines := []string{
		"The JSON payload below is an entire request for the OpenAI chat.completions format.",
		"The JSON payload below is an entire request for POST /v1/chat/completions.",
		"Interpret it exactly in OpenAI chat.completions format and produce the corresponding response in the same format.",
		"Focus on producing a valid response object that matches the expected OpenAI format for this request.",
		"Return exactly one markdown JSON code block containing a single valid JSON object and no surrounding prose.",
		`If the payload has "stream": true, still return the final completed JSON object (not SSE events).`,
	}

	if hasTools {
		lines = append(lines,
			"Tool calls are supported here: emit assistant tool calls when appropriate.",
			`If returning tool calls, use choices[0].message.tool_calls and set choices[0].finish_reason to "tool_calls".`,
			"Do not refuse by saying tool invocation is unsupported.",
			"For each tool call, function.arguments must be a JSON string value (not an object).",
			"CRITICAL: Only use tool names that appear in the tools array of the request payload. Never invent tool names.",
			"NEVER emit a tool_calls entry with name \"code_interpreter\" or any name not present in the request's tools array.",
			"Do not use code_interpreter, web_search, or any built-in/baked-in tool. Only the client-supplied tools are valid.",
		)
		normalizedChoice := strings.TrimSpace(toolChoice)
		switch strings.ToLower(normalizedChoice) {
		case "required":
			lines = append(lines, "This request requires at least one tool call. Do not return a plain-text-only assistant response.")
		case "", "auto", "none":
		default:
			lines = append(lines, fmt.Sprintf("This request requires a call to the tool named %q. Do not return a plain-text-only assistant response.", normalizedChoice))
		}
	}

	lines = append(lines, "```json", requestJSON, "```")
	return strings.Join(lines, "\n")
}

// BuildSimulatedPromptAnthropic constructs the prompt sent to M365 Copilot in
// simulated mode for Anthropic Messages API clients. It embeds the full
// Anthropic /v1/messages request JSON and instructs the model to return a
// valid Anthropic message response inside a single ```json code block.
//
// requestJSON is the serialized Anthropic /v1/messages request body.
// hasTools indicates whether the request carries client-defined tools.
// toolChoice is the Anthropic tool_choice value ("any", "auto", "tool", or "").
func BuildSimulatedPromptAnthropic(requestJSON string, hasTools bool, toolChoice string) string {
	lines := []string{
		"The JSON payload below is an entire request for the Anthropic Messages API format.",
		"The JSON payload below is an entire request for POST /v1/messages.",
		"Interpret it exactly in Anthropic Messages format and produce the corresponding response in the same format.",
		"Focus on producing a valid response object that matches the expected Anthropic format for this request.",
		"Return exactly one markdown JSON code block containing a single valid JSON object and no surrounding prose.",
		`If the payload has "stream": true, still return the final completed JSON object (not SSE events).`,
	}

	if hasTools {
		lines = append(lines,
			"Tool calls are supported here: emit assistant tool calls when appropriate.",
			`If returning tool calls, use content blocks with "type": "tool_use" and set stop_reason to "tool_use".`,
			"Do not refuse by saying tool invocation is unsupported.",
			`For each tool_use block, "input" must be a JSON object (not a string).`,
			"CRITICAL: Only use tool names that appear in the tools array of the request payload. Never invent tool names.",
			"NEVER emit a tool_use block with name \"code_interpreter\" or any name not present in the request's tools array.",
			"Do not use code_interpreter, web_search, or any built-in/baked-in tool. Only the client-supplied tools are valid.",
		)
		switch strings.ToLower(strings.TrimSpace(toolChoice)) {
		case "any":
			lines = append(lines, "This request requires at least one tool call. Do not return a plain-text-only assistant response.")
		case "tool":
			lines = append(lines, "This request requires a specific tool call. Do not return a plain-text-only assistant response.")
		}
	}

	lines = append(lines, "```json", requestJSON, "```")
	return strings.Join(lines, "\n")
}

// SimulatedResult holds the parsed simulated response.
type SimulatedResult struct {
	Content      string     // assistant message content (empty if tool calls present)
	ToolCalls    []ToolCall // parsed tool calls
	FinishReason string     // "tool_calls" or "stop"
	HasPayload   bool       // true if a usable chat-completion payload was found
}

// ParseSimulatedResponse extracts a simulated OpenAI chat completion response
// from M365 Copilot's raw text output. It enumerates JSON candidates (fenced
// blocks + balanced segments), scores them by chat-completion shape, and parses
// tool calls / content from the best candidate.
//
// allowedToolNames is the set of tool names the client actually declared. Any
// tool call whose name is NOT in this set (e.g. M365-invented "code_interpreter")
// is silently dropped. If all tool calls are dropped, the response is treated as
// a plain content response. Pass nil to disable filtering (not recommended).
func ParseSimulatedResponse(text string, allowedToolNames []string) SimulatedResult {
	allowed := make(map[string]bool, len(allowedToolNames))
	for _, n := range allowedToolNames {
		allowed[strings.TrimSpace(n)] = true
	}

	result := SimulatedResult{FinishReason: "stop"}
	candidates := enumerateJSONCandidates(text)
	if len(candidates) == 0 {
		logging.Debug("ParseSimulatedResponse: no JSON candidates found")
		return result
	}

	logging.Debugf("ParseSimulatedResponse: found %d JSON candidates, allowedTools=%v", len(candidates), allowedToolNames)
	var best map[string]any
	bestScore := -1 << 30
	for _, raw := range candidates {
		var parsed map[string]any
		if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
			continue
		}
		score := scoreSimulatedCandidate(parsed)
		if score > bestScore {
			bestScore = score
			best = parsed
		}
	}
	if best == nil || bestScore <= 0 {
		logging.Debugf("ParseSimulatedResponse: no valid candidate (bestScore=%d)", bestScore)
		return result
	}

	result.HasPayload = true
	parseChatCompletionPayload(best, &result, allowed)
	if len(result.ToolCalls) > 0 {
		logging.Infof("ParseSimulatedResponse: parsed %d tool calls", len(result.ToolCalls))
	} else if result.Content != "" {
		logging.Debug("ParseSimulatedResponse: parsed plain content response")
	}
	return result
}

// ParseSimulatedResponseAnthropic extracts a simulated Anthropic Messages
// response from M365 Copilot's raw text output. It enumerates JSON candidates,
// scores them by Anthropic message shape, and parses tool_use blocks / text
// content from the best candidate.
//
// allowedToolNames is the set of tool names the client actually declared. Any
// tool_use block whose name is NOT in this set is silently dropped.
func ParseSimulatedResponseAnthropic(text string, allowedToolNames []string) SimulatedResult {
	allowed := make(map[string]bool, len(allowedToolNames))
	for _, n := range allowedToolNames {
		allowed[strings.TrimSpace(n)] = true
	}

	result := SimulatedResult{FinishReason: "stop"}
	candidates := enumerateJSONCandidates(text)
	if len(candidates) == 0 {
		return result
	}

	var best map[string]any
	bestScore := -1 << 30
	for _, raw := range candidates {
		var parsed map[string]any
		if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
			continue
		}
		score := scoreAnthropicCandidate(parsed)
		if score > bestScore {
			bestScore = score
			best = parsed
		}
	}
	if best == nil || bestScore <= 0 {
		return result
	}

	result.HasPayload = true
	parseAnthropicPayload(best, &result, allowed)
	return result
}

// parseAnthropicPayload extracts tool_use blocks and text content from an
// Anthropic message-shaped JSON object. Tool calls whose name is not in
// `allowed` (when non-empty) are dropped.
func parseAnthropicPayload(payload map[string]any, result *SimulatedResult, allowed map[string]bool) {
	// stop_reason
	if sr, ok := payload["stop_reason"].(string); ok && sr != "" {
		if sr == "tool_use" {
			result.FinishReason = "tool_calls"
		} else {
			result.FinishReason = "stop"
		}
	}

	content, ok := payload["content"].([]any)
	if !ok || len(content) == 0 {
		// Some models put text directly in a "text" field
		if t, ok := payload["text"].(string); ok && t != "" {
			result.Content = t
		}
		return
	}

	var textParts []string
	for _, block := range content {
		bm, ok := block.(map[string]any)
		if !ok {
			continue
		}
		blockType, _ := bm["type"].(string)
		switch blockType {
		case "text":
			if t, ok := bm["text"].(string); ok && t != "" {
				textParts = append(textParts, t)
			}
		case "tool_use":
			name, _ := bm["name"].(string)
			if name == "" {
				continue
			}
			if len(allowed) > 0 && !allowed[name] {
				continue
			}
			id, _ := bm["id"].(string)
			if id == "" {
				id = nextToolCallID()
			}
			// input is a JSON object in Anthropic format
			var argsBytes []byte
			if input, ok := bm["input"]; ok && input != nil {
				argsBytes, _ = json.Marshal(input)
			} else {
				argsBytes = []byte("{}")
			}
			result.ToolCalls = append(result.ToolCalls, ToolCall{
				ID:        id,
				Name:      name,
				Arguments: json.RawMessage(argsBytes),
			})
		}
	}

	if len(result.ToolCalls) > 0 {
		result.Content = ""
		result.FinishReason = "tool_calls"
		return
	}
	result.Content = strings.Join(textParts, "\n")
}

// scoreAnthropicCandidate scores a parsed JSON object by how much it resembles
// an Anthropic Messages response. Higher is better; <=0 means unusable.
func scoreAnthropicCandidate(candidate map[string]any) int {
	score := 0
	if isRequestLikeSimulatedPayload(candidate) {
		score -= 180
	}

	// Anthropic response has "content" array, "role", "stop_reason", "type":"message"
	content, hasContent := candidate["content"].([]any)
	if hasContent && len(content) > 0 {
		score += 220
		// Check for tool_use / text blocks
		for _, block := range content {
			if bm, ok := block.(map[string]any); ok {
				if bt, ok := bm["type"].(string); ok {
					switch bt {
					case "tool_use":
						score += 90
					case "text":
						if t, ok := bm["text"].(string); ok && strings.TrimSpace(t) != "" {
							score += 35
						}
					}
				}
			}
		}
	}

	if role, ok := candidate["role"].(string); ok && strings.ToLower(role) == "assistant" {
		score += 30
	}
	if t, ok := candidate["type"].(string); ok && strings.ToLower(t) == "message" {
		score += 70
	}
	if sr, ok := candidate["stop_reason"].(string); ok && sr != "" {
		score += 25
	}
	if id, ok := candidate["id"].(string); ok && strings.HasPrefix(strings.ToLower(id), "msg_") {
		score += 50
	}

	// Penalize OpenAI-shaped objects (choices array)
	if _, ok := candidate["choices"].([]any); ok {
		score -= 100
	}

	return score
}

// parseChatCompletionPayload extracts tool calls and content from a
// chat.completion-shaped JSON object into the result. Tool calls whose name is
// not in `allowed` (when non-empty) are dropped — this strips M365-invented
// tools like "code_interpreter" that the client never declared.
func parseChatCompletionPayload(payload map[string]any, result *SimulatedResult, allowed map[string]bool) {
	choices, ok := payload["choices"].([]any)
	if !ok || len(choices) == 0 {
		return
	}
	first, ok := choices[0].(map[string]any)
	if !ok {
		return
	}

	if fr, ok := first["finish_reason"].(string); ok && fr != "" {
		result.FinishReason = fr
	}

	message, ok := first["message"].(map[string]any)
	if !ok {
		return
	}

	if toolCallsNode, ok := message["tool_calls"].([]any); ok && len(toolCallsNode) > 0 {
		for _, tcNode := range toolCallsNode {
			tc, ok := tcNode.(map[string]any)
			if !ok {
				continue
			}
			name, id, args := extractToolCallFields(tc)
			if name == "" {
				continue
			}
			// Filter out tool calls whose name the client never declared
			// (e.g. M365-injected "code_interpreter"). When allowed is empty,
			// filtering is skipped (back-compat / non-tool requests).
			if len(allowed) > 0 && !allowed[name] {
				continue
			}
			result.ToolCalls = append(result.ToolCalls, ToolCall{
				ID:        id,
				Name:      name,
				Arguments: json.RawMessage(args),
			})
		}
		if len(result.ToolCalls) > 0 {
			result.Content = ""
			result.FinishReason = "tool_calls"
			return
		}
	}

	result.Content = normalizeMessageContent(message["content"])
}

// extractToolCallFields pulls id/name/arguments from a tool_calls entry,
// tolerating both the OpenAI wrapper ({id,type,function:{name,arguments}})
// and a flat shape ({name,arguments}).
func extractToolCallFields(tc map[string]any) (name, id, args string) {
	if fn, ok := tc["function"].(map[string]any); ok {
		if n, ok := fn["name"].(string); ok && n != "" {
			name = n
		}
		args = normalizeArgumentsJSON(fn["arguments"])
		if i, ok := tc["id"].(string); ok && i != "" {
			id = i
		}
	}
	if name == "" {
		if n, ok := tc["name"].(string); ok && n != "" {
			name = n
		}
	}
	if args == "" {
		args = normalizeArgumentsJSON(tc["arguments"])
	}
	if id == "" {
		if i, ok := tc["id"].(string); ok && i != "" {
			id = i
		}
	}
	if id == "" {
		id = nextToolCallID()
	}
	return
}

// normalizeArgumentsJSON ensures arguments is a JSON string value. If the node
// is already a string, it is returned as-is (after light validation); if it is
// an object/array, it is re-serialized; if missing, "{}" is returned.
func normalizeArgumentsJSON(node any) string {
	if node == nil {
		return "{}"
	}
	if s, ok := node.(string); ok {
		trimmed := strings.TrimSpace(s)
		if trimmed == "" {
			return "{}"
		}
		// Validate it parses; if not, return as-is (tolerance).
		var probe any
		if json.Unmarshal([]byte(trimmed), &probe) == nil {
			return trimmed
		}
		return trimmed
	}
	b, err := json.Marshal(node)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// normalizeMessageContent flattens message.content (string or array of parts)
// into a single string.
func normalizeMessageContent(node any) string {
	if node == nil {
		return ""
	}
	if s, ok := node.(string); ok {
		return s
	}
	if arr, ok := node.([]any); ok {
		var parts []string
		for _, part := range arr {
			if s, ok := part.(string); ok {
				parts = append(parts, s)
				continue
			}
			if m, ok := part.(map[string]any); ok {
				if t, ok := m["text"].(string); ok {
					parts = append(parts, t)
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// scoreSimulatedCandidate scores a parsed JSON object by how much it resembles
// an OpenAI chat.completion response. Higher is better; <=0 means unusable.
func scoreSimulatedCandidate(candidate map[string]any) int {
	score := 0
	if isRequestLikeSimulatedPayload(candidate) {
		score -= 180
	}

	choices, ok := candidate["choices"].([]any)
	if ok && len(choices) > 0 {
		score += 220
		if first, ok := choices[0].(map[string]any); ok {
			if message, ok := first["message"].(map[string]any); ok {
				score += 80
				if role, ok := message["role"].(string); ok && strings.ToLower(role) == "assistant" {
					score += 20
				}
				if tc, ok := message["tool_calls"].([]any); ok && len(tc) > 0 {
					score += 90
				}
				if content, ok := message["content"].(string); ok && strings.TrimSpace(content) != "" {
					score += 35
				} else if arr, ok := message["content"].([]any); ok && len(arr) > 0 {
					score += 20
				}
			}
			if fr, ok := first["finish_reason"].(string); ok && fr != "" {
				score += 15
			}
		}
	}

	if looksLikeChatChoiceObject(candidate) {
		score += 75
	}
	if obj, ok := candidate["object"].(string); ok && strings.ToLower(obj) == "chat.completion" {
		score += 70
	}
	if id, ok := candidate["id"].(string); ok && strings.HasPrefix(strings.ToLower(id), "chatcmpl") {
		score += 50
	}

	return score
}

// isRequestLikeSimulatedPayload detects when a candidate is actually the echoed
// request (has messages/input + tools/tool_choice) rather than a response, so
// we can penalize it.
func isRequestLikeSimulatedPayload(candidate map[string]any) bool {
	_, hasMessages := candidate["messages"].([]any)
	_, hasInput := candidate["input"]
	_, hasTools := candidate["tools"].([]any)
	_, hasToolChoiceStr := candidate["tool_choice"].(string)
	_, hasToolChoiceObj := candidate["tool_choice"].(map[string]any)
	_, hasParallel := candidate["parallel_tool_calls"]
	_, hasChoices := candidate["choices"].([]any)
	lacksResponseShape := !hasChoices

	hasToolSignals := hasTools || hasToolChoiceStr || hasToolChoiceObj || hasParallel || lacksResponseShape
	return (hasMessages || hasInput) && hasToolSignals
}

// looksLikeChatChoiceObject returns true if the candidate itself looks like a
// single choice object (has message/delta/finish_reason).
func looksLikeChatChoiceObject(candidate map[string]any) bool {
	_, hasMessage := candidate["message"].(map[string]any)
	_, hasDelta := candidate["delta"].(map[string]any)
	_, hasFinishReason := candidate["finish_reason"]
	return hasMessage || hasDelta || hasFinishReason
}

// enumerateJSONCandidates yields candidate JSON substrings from rawText:
//  1. The trimmed whole text
//  2. Bodies of ``` fenced code blocks
//  3. Balanced {…} / […] segments scanned from the text
//
// Duplicates are removed. Order is preserved (best-effort).
func enumerateJSONCandidates(rawText string) []string {
	trimmed := strings.TrimSpace(rawText)
	if trimmed == "" {
		return nil
	}

	seen := make(map[string]bool)
	var out []string
	add := func(s string) {
		n := strings.TrimSpace(s)
		if n == "" || seen[n] {
			return
		}
		seen[n] = true
		out = append(out, n)
	}

	add(trimmed)

	// Fenced code blocks
	cursor := 0
	for cursor < len(rawText) {
		fenceStart := indexOf(rawText, "```", cursor)
		if fenceStart < 0 {
			break
		}
		bodyStart := indexOfByte(rawText, '\n', fenceStart+3)
		if bodyStart < 0 {
			break
		}
		fenceEnd := indexOf(rawText, "```", bodyStart+1)
		if fenceEnd < 0 {
			break
		}
		body := strings.TrimSpace(rawText[bodyStart+1 : fenceEnd])
		if body != "" {
			add(body)
		}
		cursor = fenceEnd + 3
	}

	// Balanced JSON segments
	for _, seg := range extractBalancedJSONSegments(rawText) {
		add(seg)
	}

	return out
}

// extractBalancedJSONSegments scans rawText for balanced {…} and […] segments.
func extractBalancedJSONSegments(rawText string) []string {
	const maxCandidates = 128
	var segments []string
	emitted := 0
	for start := 0; start < len(rawText) && emitted < maxCandidates; start++ {
		ch := rawText[start]
		if ch != '{' && ch != '[' {
			continue
		}
		seg := extractBalancedJSONSegment(rawText, start, ch)
		if seg != "" {
			segments = append(segments, seg)
			emitted++
		}
	}
	return segments
}

// extractBalancedJSONSegment returns the balanced segment starting at `start`
// with opening char `opening` ('{' or '['), or "" if unbalanced.
func extractBalancedJSONSegment(rawText string, start int, opening byte) string {
	var closing byte
	if opening == '{' {
		closing = '}'
	} else {
		closing = ']'
	}
	depth := 0
	inString := false
	escaped := false

	for i := start; i < len(rawText); i++ {
		ch := rawText[i]
		if inString {
			if escaped {
				escaped = false
				continue
			}
			if ch == '\\' {
				escaped = true
				continue
			}
			if ch == '"' {
				inString = false
			}
			continue
		}
		if ch == '"' {
			inString = true
			continue
		}
		if ch == opening {
			depth++
			continue
		}
		if ch == closing {
			depth--
			if depth == 0 {
				return strings.TrimSpace(rawText[start : i+1])
			}
		}
	}
	return ""
}

func indexOf(s, sub string, from int) int {
	idx := strings.Index(s[from:], sub)
	if idx < 0 {
		return -1
	}
	return from + idx
}

func indexOfByte(s string, b byte, from int) int {
	for i := from; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

// FormatSimulatedToolResult converts a tool result into text for the next M365
// turn in simulated mode. Mirrors the fenced FormatToolResult but is kept
// separate so simulated conversations can be formatted differently if needed.
func FormatSimulatedToolResult(toolCallID, toolName, result string) string {
	return fmt.Sprintf("[Tool Result for %s (call_id: %s)]\n%s", toolName, toolCallID, result)
}
