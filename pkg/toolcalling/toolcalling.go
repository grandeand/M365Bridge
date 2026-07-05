// Package toolcalling provides simulated tool calling support for clients
// (Claude Code, Codex, etc.) by injecting tool definitions into the message
// text sent to M365 Copilot and parsing tool call patterns from the response.
//
// M365 Copilot backend does not natively support client-defined tools.
// This package bridges the gap by:
//   - Injecting tool definitions as a system prompt prefix into the last user message
//   - Parsing fenced code block tool calls from M365 response text
//   - Converting tool role messages (OpenAI) and tool_result blocks (Anthropic)
//     back into text for the M365 backend
//
// Tool call format: Markdown fenced code blocks (```toolname). This exploits
// the model's training bias toward writing ```bash blocks — the "shell-routing"
// technique from cramt/m365-copilot-proxy. JSON format scored 0/5 on real
// agentic tasks; fenced blocks achieved working loops.
package toolcalling

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// ToolDef represents a tool definition from the client request.
type ToolDef struct {
	Type     string      `json:"type"`
	Function ToolDefFunc `json:"function"`
	// Anthropic-style fields (flat, no "function" wrapper)
	Name        string                 `json:"name,omitempty"`
	Description string                 `json:"description,omitempty"`
	InputSchema map[string]interface{} `json:"input_schema,omitempty"`
}

// ToolDefFunc is the OpenAI-style function definition inside a tool.
type ToolDefFunc struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	Parameters  map[string]interface{} `json:"parameters"`
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

// --- Fenced tool spec derivation ---

// bodyParamNames lists parameter names that are treated as the fence body
// (free-form text) rather than scalar header lines.
var bodyParamNames = map[string]bool{
	"command": true, "content": true, "code": true, "body": true,
	"script": true, "text": true, "query": true, "input": true,
	"patch": true, "cmd": true, "data": true, "contents": true,
}

// searchKeys and replaceKeys identify edit-pair parameters rendered as
// SEARCH/REPLACE diffs inside the fence body.
var searchKeys = map[string]bool{
	"old": true, "search": true, "find": true, "old_str": true, "old_string": true, "target": true,
}
var replaceKeys = map[string]bool{
	"new": true, "replace": true, "replacement": true, "new_str": true, "new_string": true,
}

// shellLangs are fence info-strings that mean "a shell script". The model
// emits ```bash reflexively; we route it to whatever shell tool the harness gave.
var shellLangs = map[string]bool{
	"bash": true, "sh": true, "shell": true, "zsh": true,
	"console": true, "shell-session": true, "shellsession": true, "shsession": true,
}

// shellToolNameRegex matches tool names that look like a run-a-command tool.
var shellToolNameRegex = regexp.MustCompile(`^(?i)(bash|sh|shell|zsh|run|exec|execute|command|cmd|terminal|run_command|run_terminal_cmd|execute_command|execute_bash|shell_exec|system)$`)

// FencedToolSpec describes how a tool maps onto the fenced code block format.
type FencedToolSpec struct {
	Name         string
	Description  string
	HeaderParams []string
	BodyParam    string
	EditPair     *editPair
}

type editPair struct {
	Search  string
	Replace string
}

// findShellTool returns the harness tool that runs a shell command, if any.
func findShellTool(tools []ToolDef) *ToolDef {
	for i := range tools {
		name := toolName(&tools[i])
		if shellToolNameRegex.MatchString(name) {
			return &tools[i]
		}
	}
	// Fallback: a single-string-param tool whose param is command-ish
	for i := range tools {
		props := toolParamProps(&tools[i])
		if len(props) == 1 {
			p := props[0]
			if p == "command" || p == "cmd" || p == "script" || p == "input" {
				return &tools[i]
			}
		}
	}
	return nil
}

// toolName extracts the name from either OpenAI or Anthropic tool definition.
func toolName(t *ToolDef) string {
	if t.Function.Name != "" {
		return t.Function.Name
	}
	return t.Name
}

// toolDescription extracts the description from either format.
func toolDescription(t *ToolDef) string {
	if t.Function.Description != "" {
		return t.Function.Description
	}
	return t.Description
}

// toolParams extracts the parameters map from either format.
func toolParams(t *ToolDef) map[string]interface{} {
	if t.Function.Parameters != nil {
		return t.Function.Parameters
	}
	return t.InputSchema
}

// toolParamProps returns the property names from a tool's parameters schema.
func toolParamProps(t *ToolDef) []string {
	params := toolParams(t)
	if params == nil {
		return nil
	}
	props, ok := params["properties"].(map[string]interface{})
	if !ok {
		return nil
	}
	result := make([]string, 0, len(props))
	for k := range props {
		result = append(result, k)
	}
	return result
}

// deriveFencedSpec maps an OpenAI tool definition onto the fenced shape.
func deriveFencedSpec(tool *ToolDef) FencedToolSpec {
	name := toolName(tool)
	desc := toolDescription(tool)
	props := toolParamProps(tool)

	var search, replace string
	for _, p := range props {
		if searchKeys[p] && search == "" {
			search = p
		}
		if replaceKeys[p] && replace == "" {
			replace = p
		}
	}

	if search != "" && replace != "" {
		headerParams := make([]string, 0, len(props))
		for _, p := range props {
			if p != search && p != replace {
				headerParams = append(headerParams, p)
			}
		}
		return FencedToolSpec{
			Name:         name,
			Description:  desc,
			HeaderParams: headerParams,
			EditPair:     &editPair{Search: search, Replace: replace},
		}
	}

	var bodyParam string
	for _, p := range props {
		if bodyParamNames[p] {
			bodyParam = p
			break
		}
	}
	if bodyParam == "" && len(props) == 1 {
		bodyParam = props[0]
	}

	headerParams := make([]string, 0, len(props))
	for _, p := range props {
		if p != bodyParam {
			headerParams = append(headerParams, p)
		}
	}

	return FencedToolSpec{
		Name:         name,
		Description:  desc,
		HeaderParams: headerParams,
		BodyParam:    bodyParam,
	}
}

// buildSpecMap creates a map from fence info-string to tool spec.
// Shell language aliases (bash, sh, etc.) are mapped to the shell tool.
func buildSpecMap(tools []ToolDef) map[string]FencedToolSpec {
	m := make(map[string]FencedToolSpec)
	for i := range tools {
		spec := deriveFencedSpec(&tools[i])
		m[spec.Name] = spec
	}

	// Shell aliasing: route ```bash / ```sh / ```shell to the harness's shell tool
	// even when it's named run/run_command/etc.
	if shell := findShellTool(tools); shell != nil {
		shellSpec := m[toolName(shell)]
		for lang := range shellLangs {
			if _, exists := m[lang]; !exists {
				m[lang] = shellSpec
			}
		}
	}

	return m
}

// --- Regex patterns ---

// fenceRegex matches Markdown fenced code blocks: ```info-string\nbody\n```
var fenceRegex = regexp.MustCompile("```([A-Za-z0-9_]+)[ \t]*\r?\n([\\s\\S]*?)\r?\n?```")

// searchReplaceRegex matches SEARCH/REPLACE diff blocks inside fence bodies.
var searchReplaceRegex = regexp.MustCompile(`<{5,}\s*SEARCH\s*\r?\n([\s\S]*?)\r?\n={5,}\s*\r?\n([\s\S]*?)\r?\n>{5,}\s*REPLACE`)

// jsonToolCallRegex matches stray JSON tool calls as a tolerance fallback.
var jsonToolCallRegex = regexp.MustCompile(`\{\s*"tool"\s*:\s*"[^"]+"\s*,\s*"arguments"\s*:\s*\{[\s\S]*?\}\s*\}`)

// legacyToolCallPattern matches <tool>{...}</tool> blocks as a fallback.
var legacyToolCallPattern = regexp.MustCompile(`(?s)<tool>\s*(\{.*?\})\s*</tool>`)

// headerLineRegex matches "key: value" header lines inside a fence.
var headerLineRegex = regexp.MustCompile(`^([A-Za-z0-9_]+):[ \t]?(.*)$`)

// --- Parsing ---

// parseFencedInner parses the inner text of one fenced block into arguments.
func parseFencedInner(spec FencedToolSpec, inner string) (map[string]interface{}, error) {
	lines := strings.Split(inner, "\n")
	args := make(map[string]interface{})

	// Header: contiguous "key: value" lines whose key is a known header param
	i := 0
	if len(spec.HeaderParams) > 0 {
		headerSet := make(map[string]bool)
		for _, h := range spec.HeaderParams {
			headerSet[h] = true
		}
		for ; i < len(lines); i++ {
			line := lines[i]
			if strings.TrimSpace(line) == "" {
				i++
				break
			}
			m := headerLineRegex.FindStringSubmatch(line)
			if m != nil && headerSet[m[1]] {
				args[m[1]] = m[2]
			} else {
				break
			}
		}
	}

	rest := strings.Join(lines[i:], "\n")

	if spec.EditPair != nil {
		sr := searchReplaceRegex.FindStringSubmatch(rest)
		if sr == nil {
			return nil, fmt.Errorf("edit tool %q missing SEARCH/REPLACE markers", spec.Name)
		}
		args[spec.EditPair.Search] = sr[1]
		args[spec.EditPair.Replace] = sr[2]
	} else if spec.BodyParam != "" {
		args[spec.BodyParam] = rest
	}

	return args, nil
}

// ParseToolCalls scans response text for fenced tool calls and extracts them.
// Returns the text with tool call blocks removed and the parsed tool calls.
// Falls back to JSON and legacy <tool> tag parsing for tolerance.
// If no tool call blocks are found, returns the original text and nil.
func ParseToolCalls(text string, tools []ToolDef) (string, []ToolCall) {
	if len(tools) == 0 {
		return text, nil
	}

	specs := buildSpecMap(tools)
	var calls []ToolCall
	leftover := text

	// Primary: parse fenced code blocks
	matches := fenceRegex.FindAllStringSubmatch(text, -1)
	for _, m := range matches {
		infoStr := m[1]
		body := m[2]
		spec, ok := specs[infoStr]
		if !ok {
			continue // ```python illustration etc. — not a tool, leave in prose
		}
		args, err := parseFencedInner(spec, body)
		if err != nil {
			continue
		}
		argsBytes, err := json.Marshal(args)
		if err != nil {
			continue
		}
		calls = append(calls, ToolCall{
			ID:        nextToolCallID(),
			Name:      spec.Name,
			Arguments: argsBytes,
		})
		leftover = strings.Replace(leftover, m[0], "", 1)
	}

	if len(calls) > 0 {
		return strings.TrimSpace(leftover), calls
	}

	// Fallback 1: stray JSON tool calls {"tool":"...","arguments":{...}}
	jsonMatches := jsonToolCallRegex.FindAllString(text, -1)
	for _, jm := range jsonMatches {
		var parsed struct {
			Tool      string                 `json:"tool"`
			Arguments map[string]interface{} `json:"arguments"`
		}
		if err := json.Unmarshal([]byte(jm), &parsed); err != nil {
			continue
		}
		if parsed.Tool == "" {
			continue
		}
		argsBytes, _ := json.Marshal(parsed.Arguments)
		calls = append(calls, ToolCall{
			ID:        nextToolCallID(),
			Name:      parsed.Tool,
			Arguments: argsBytes,
		})
	}
	if len(calls) > 0 {
		cleaned := jsonToolCallRegex.ReplaceAllString(text, "")
		return strings.TrimSpace(cleaned), calls
	}

	// Fallback 2: legacy <tool>{...}</tool> tags
	legacyMatches := legacyToolCallPattern.FindAllStringSubmatch(text, -1)
	for _, m := range legacyMatches {
		if len(m) < 2 {
			continue
		}
		var parsed struct {
			Name      string                 `json:"name"`
			Arguments map[string]interface{} `json:"arguments"`
		}
		if err := json.Unmarshal([]byte(m[1]), &parsed); err != nil {
			continue
		}
		if parsed.Name == "" {
			continue
		}
		argsBytes, _ := json.Marshal(parsed.Arguments)
		calls = append(calls, ToolCall{
			ID:        nextToolCallID(),
			Name:      parsed.Name,
			Arguments: argsBytes,
		})
	}
	if len(calls) > 0 {
		cleaned := legacyToolCallPattern.ReplaceAllString(text, "")
		return strings.TrimSpace(cleaned), calls
	}

	return text, nil
}

// --- Anti-confabulation guards ---

// confabulationPatterns detects responses where the model claims it can't
// access files or asks the user to paste content — instead of calling a tool.
var confabulationPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)return(?:ing|s|ed)?\s+no\s+(?:output|results?|content)`),
	regexp.MustCompile(`(?i)no\s+(?:output|results?|content|data)\s+(?:was\s+|were\s+)?(?:return|provid|present)`),
	regexp.MustCompile(`(?i)(?:unable|not able|can.?t|cannot)\s+to?\s*(?:access|inspect|list|read|run|locate|see|open)`),
	regexp.MustCompile(`(?i)don.?t\s+have\s+access`),
	regexp.MustCompile(`(?i)no\s+access\s+to`),
	regexp.MustCompile(`(?i)paste\s+(?:the\s+)?(?:contents?|files?|code|them)`),
	regexp.MustCompile(`(?i)provide\s+(?:the\s+)?(?:contents?|files?)`),
	regexp.MustCompile(`(?i)(?:environment|shell|tool)\s+(?:isn.?t|is not|aren.?t|are not|appears? to be)\s+(?:return|provid|respond|work|access)`),
	regexp.MustCompile(`(?i)no\s+files?\s+(?:in|found|present|visible)`),
	regexp.MustCompile(`(?i)(?:file|directory|folder|it)\s+(?:appears?|seems?|looks?)\s+(?:to\s+be\s+)?empty`),
	regexp.MustCompile(`(?i)nothing\s+to\s+(?:simplify|fix|do|change|show|read)`),
	regexp.MustCompile(`(?i)(?:tool|command|it)\s+returned\s+(?:no|empty|nothing)`),
}

// hallucinatedCompletionPatterns detects responses where the model claims to
// have performed a file mutation without actually calling a tool.
var hallucinatedCompletionPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bI(?:'ve|\s+have|\s+just|\s+now)?\s+(?:created|wrote|written|replaced|updated|saved|applied|added|overwrote|modified|generated|implemented|rewrote)\b`),
	regexp.MustCompile(`(?i)\b(?:the\s+)?(?:file|readme|script|config|change|version|content)\s+(?:has|have|is|was|were)\s+(?:been\s+)?(?:created|replaced|updated|saved|written|applied|added|modified|overwritten)\b`),
	regexp.MustCompile(`(?i)\bhere'?s\s+(?:the\s+)?(?:updated|new|simplified|replaced|final)\s+(?:file|readme|version|content)\b`),
	regexp.MustCompile(`(?i)\b(?:created|wrote|written|generated|saved|added|produced|implemented|overwrote)\b[^.\n]{0,60}\b[\w-]{2,}\.[a-z]{1,4}\b`),
	regexp.MustCompile(`(?i)\b(?:executed|ran|invoked|launched|compiled)\b[^.\n]{0,40}\b(?:it|them|this|the\s+(?:script|program|file|code|command|tests?)|python3?|node|\S{2,}\.[a-z]{1,4})\b`),
}

// LooksLikeConfabulation returns true if the text looks like the model is
// refusing to act (claiming it can't access files, asking to paste content)
// instead of calling a tool. Used to trigger a retry.
func LooksLikeConfabulation(text string) bool {
	if len(text) < 12 {
		return false
	}
	t := strings.TrimSpace(text)
	if len(t) < 12 {
		return false
	}
	for _, re := range confabulationPatterns {
		if re.MatchString(t) {
			return true
		}
	}
	return false
}

// LooksLikeHallucinatedCompletion returns true if the text claims a file
// mutation was performed without any tool call evidence. Used to trigger a
// retry only when no tool call ran in the whole conversation.
func LooksLikeHallucinatedCompletion(text string) bool {
	if len(text) < 8 {
		return false
	}
	t := strings.TrimSpace(text)
	if len(t) < 8 {
		return false
	}
	for _, re := range hallucinatedCompletionPatterns {
		if re.MatchString(t) {
			return true
		}
	}
	return false
}

// markdownHeaderRegex detects markdown headers (# Title) in prose.
var markdownHeaderRegex = regexp.MustCompile(`(?m)^#{1,6}\s`)

// IsProseDocument returns true if the parsed result looks like a markdown
// document (prose with embedded code fences) rather than a real tool call.
// This prevents the parser from executing the model's answer as tool calls.
// Heuristics: ≥4 tool calls OR markdown headers OR ≥300 chars of prose.
func IsProseDocument(toolCallCount int, textContent string) bool {
	if toolCallCount < 2 {
		return false
	}
	prose := strings.TrimSpace(textContent)
	if toolCallCount >= 4 {
		return true
	}
	if markdownHeaderRegex.MatchString(prose) {
		return true
	}
	return len(prose) >= 300
}

// --- Tool injection ---

// InjectTools prepends tool definitions and instructions to the last user message.
// Returns the modified message text. If no tools are provided, returns the original text.
func InjectTools(messages []string, tools []ToolDef) []string {
	if len(tools) == 0 || len(messages) == 0 {
		return messages
	}

	instruction := buildToolInstruction(tools)
	result := make([]string, len(messages))
	copy(result, messages)

	// Find the last user message and prepend the tool instruction
	for i := len(result) - 1; i >= 0; i-- {
		result[i] = instruction + "\n\n" + result[i]
		break
	}

	return result
}

// buildToolInstruction creates the system prompt that tells the model about client-side
// tools and the fenced code block format to use when requesting the client to execute them.
//
// Uses "shell-routing" framing: the model is told it's an automated agent whose
// output is parsed by a program. Tools are emitted as Markdown fenced code blocks
// (```toolname), exploiting the model's training bias toward writing ```bash blocks.
// This is the proven approach from cramt/m365-copilot-proxy.
func buildToolInstruction(tools []ToolDef) string {
	var sb strings.Builder

	sb.WriteString("You are the execution core of an automated agent, not a chat assistant. ")
	sb.WriteString("Your output is parsed by a program — a real runtime that executes your tool calls against a live system and returns the actual results to you.\n\n")

	// Shell-first framing if a shell tool is present
	if shell := findShellTool(tools); shell != nil {
		shellName := toolName(shell)
		sb.WriteString(fmt.Sprintf("THE WAY YOU DO ANYTHING IS BY WRITING A SHELL SCRIPT. You have a real shell (the `%s` tool). ", shellName))
		sb.WriteString("To perform a step, emit ONE ```bash block that does the whole thing end-to-end against the real files in the working directory: ")
		sb.WriteString("create/overwrite files with `cat > name <<'EOF' ... EOF` heredocs, edit files in place with `sed -i`, inspect with `cat`/`ls`/`grep`, run code with the available interpreters. ")
		sb.WriteString("The block is executed for real and you get its output back. Writing the commands IS doing the task; describing what you \"would\" run accomplishes nothing.\n\n")

		sb.WriteString("You have NOT run any command yet and have NO results. ")
		sb.WriteString("NEVER claim a command \"returned no output\", that files are \"missing\", or that you \"cannot access\" / \"cannot list\" the environment before you have actually emitted a ```bash block and seen its output. ")
		sb.WriteString("The files named in the task are present on a real filesystem right now. ")
		sb.WriteString("Your FIRST output must be a ```bash block (e.g. `ls -la` then `cat` the relevant files) — never open with prose, a question, or a request for the user to paste files. ")
		sb.WriteString("Do not assume a file's contents or a command's result; run a command and read the real output. One self-contained ```bash block per turn.\n\n")
	}

	sb.WriteString("Performing the task with tools is your PRIMARY JOB. Answering the user in prose is SECONDARY — you write prose only when the task is fully done or no tool can make progress. Default to acting, not talking.\n\n")

	sb.WriteString("TOOL USE IS REQUIRED when the user asks you to read files, run commands, inspect the repository, fetch data, or perform any action a tool can accomplish. ")
	sb.WriteString("The tools are real: they read real files, run real commands, and change real state. Never answer from memory or simulate a result when a tool can provide it.\n\n")

	sb.WriteString("To call a tool, output ONLY a single fenced code block whose info-string is the tool name. ")
	sb.WriteString("A fenced block is an ACTION the runtime executes — it is NOT an illustration, an example, or \"here's how you would do it\". No text before or after it:\n\n")

	sb.WriteString("```<tool_name>\n")
	sb.WriteString("<header lines: one \"key: value\" per scalar argument>\n\n")
	sb.WriteString("<body argument, if the tool has one>\n")
	sb.WriteString("```\n\n")

	sb.WriteString("STRICT RULES:\n")
	sb.WriteString("- Output ONLY the fenced block when calling a tool. No prose, no second fence, no commentary before or after.\n")
	sb.WriteString("- Never describe your intent (\"I'll read the file...\", \"Let me check...\") and never emit filler or acknowledgements. Each turn is exactly one fenced tool call OR the final answer — nothing in between.\n")
	sb.WriteString("- One tool call per response, then stop and wait for its result. Never emit two fenced blocks in one response.\n")
	sb.WriteString("- The fence info-string and the header keys must match a tool defined below exactly.\n")
	sb.WriteString("- A result block is the real result from the live system — treat it as ground truth, never invent or assume results.\n")
	sb.WriteString("- NEVER claim you have done something — read a file, run a command, written code, built, or succeeded — unless a result proving it already appears above.\n")
	sb.WriteString("- If a tool call fails or returns partial data, immediately call another tool to resolve it. Do not give up.\n")
	sb.WriteString("- Do not defer work or promise future results (\"I'll do this next...\").\n")
	sb.WriteString("- Do not ask the user questions unless tool execution is impossible.\n")
	sb.WriteString("- Produce natural-language text only when the task is complete and no further tool call applies; that text is the answer returned to the caller. When you do, output only the answer itself — no preamble, no sign-off.\n\n")

	// Tool definitions block
	sb.WriteString("<tools>\n")
	for i := range tools {
		spec := deriveFencedSpec(&tools[i])
		sb.WriteString(renderFencedTemplate(spec))
		if i < len(tools)-1 {
			sb.WriteString("\n\n")
		}
	}
	sb.WriteString("\n</tools>\n")

	return sb.String()
}

// renderFencedTemplate produces a self-documenting template for one tool.
func renderFencedTemplate(spec FencedToolSpec) string {
	var lines []string
	for _, h := range spec.HeaderParams {
		lines = append(lines, fmt.Sprintf("%s: <%s>", h, h))
	}
	if spec.EditPair != nil {
		lines = append(lines, "<<<<<<< SEARCH")
		lines = append(lines, fmt.Sprintf("<%s>", spec.EditPair.Search))
		lines = append(lines, "=======")
		lines = append(lines, fmt.Sprintf("<%s>", spec.EditPair.Replace))
		lines = append(lines, ">>>>>>> REPLACE")
	} else if spec.BodyParam != "" {
		if len(lines) > 0 {
			lines = append(lines, "")
		}
		lines = append(lines, fmt.Sprintf("<%s>", spec.BodyParam))
	}

	header := spec.Name
	if spec.Description != "" {
		header = fmt.Sprintf("%s — %s", spec.Name, spec.Description)
	}

	return fmt.Sprintf("%s\n```%s\n%s\n```", header, spec.Name, strings.Join(lines, "\n"))
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
