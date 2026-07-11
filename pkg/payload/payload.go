// Package payload provides request payload builders for M365 Copilot WebSocket communication.
// It constructs JSON payloads for chat requests, conversation history, and various options.
package payload

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	// variants is the feature flags string sent with requests.
	variants = "EnableMcpServerWidgets,feature.EnableLuForChatCIQ,feature.enableChatCIQPlugin," +
		"EnableRequestPlugins,feature.IsCustomEngineCopilotEnabled,feature.bizchatfluxv3," +
		"feature.enablechatpages,feature.IsStreamingModeInChatEnabled," +
		"IncludeSourceAttributionsConcise,SkipPublishEmptyMessage," +
		"feature.EnableDeduplicatingSourceAttributions,feature.enableDeltaStreamingForReferences," +
		"feature.enableIncludeReferencesInDeltaResponse,feature.enablereferencesforagents," +
		"feature.EnableReferencesListCompleteSignal,SingletonEnvOn,cdxenablefccinmainline," +
		"feature.disabledisallowedmsgs,cdxenablerenderforisocomp," +
		"feature.EnablePersonalization,feature.EnableSkipEmittingMessageOnFlush," +
		"feature.EnableRemoveEmptySourceAttributions,feature.EnableRemoveStreamingMode," +
		"feature.OfficeWebToHelix,feature.OfficeDesktopToHelix,feature.M365TeamsHubToHelix," +
		"feature.OwaHubToHelix,feature.MonarchHubToHelix,feature.Win32OutlookHubToHelix," +
		"feature.MacOutlookHubToHelix,Agt_bizchat_enableGpt5ForHelix"
)

// optionsSetsFull contains the full set of option flags for complete functionality.
var optionsSetsFull = []string{
	"search_result_progress_messages_with_search_queries",
	"update_textdoc_response_after_streaming",
	"deepleo_networking_timeout_10minutes_canmore",
	"cwc_code_interpreter",
	"cwc_code_interpreter_amsfix", "cwcfluxgptv",
	"gptvnorm2048", "cwc_code_interpreter_citation_fix",
	"code_interpreter_interactive_charts",
	"cwc_code_interpreter_interactive_charts_inline_image",
	"code_interpreter_matplotlib_patching",
	"cwc_fileupload_odb", "update_memory_plugin",
	"add_custom_instructions",
	"enable_batch_token_processing",
	"enable_gg_gpt",
	"rich_responses",
	"pages_citations", "pages_citations_multiturn",
}

// fileUploadOptions contains option flags specific to file upload.
var fileUploadOptions = map[string]bool{
	"cwc_fileupload_odb": true,
}

// codeInterpreterOptions contains option flags that activate M365's built-in
// code_interpreter sandbox. When client-defined tools are present, these MUST be
// stripped to prevent M365 from intercepting file/code operations and routing
// them to its own sandbox instead of emitting tool calls for the client.
// This is the primary infrastructure lever (cramt/m365-copilot-proxy approach).
var codeInterpreterOptions = map[string]bool{
	"cwc_code_interpreter":                                 true,
	"cwc_code_interpreter_amsfix":                          true,
	"cwcfluxgptv":                                          true,
	"gptvnorm2048":                                         true,
	"cwc_code_interpreter_citation_fix":                    true,
	"code_interpreter_interactive_charts":                  true,
	"cwc_code_interpreter_interactive_charts_inline_image": true,
	"code_interpreter_matplotlib_patching":                 true,
}

// imageUploadOptions contains option flags needed for image upload support.
var imageUploadOptions = map[string]bool{
	"cwc_flux_image": true,
	"flux_v3_gptv_enable_upload_multi_image_in_turn_wo_ch": true,
}

// allowedMessageTypes lists the message types allowed in requests.
var allowedMessageTypes = []string{
	"Chat", "Suggestion", "InternalSearchQuery", "Disengaged",
	"InternalLoaderMessage", "Progress", "GeneratedCode",
	"RenderCardRequest", "AdsQuery", "SemanticSerp",
	"GenerateContentQuery", "SearchQuery",
	"ConfirmationCard", "AuthError", "DeveloperLogs",
	"TriggerPlugin", "HintInvocation", "MemoryUpdate",
	"EndOfRequest", "TriggerConfirmation",
	"ResumeInvokeAction", "ResumeUserInputRequest",
}

// Message represents a chat message in the conversation history.
type Message struct {
	Role        string              `json:"role"`
	Content     string              `json:"content"`
	Name        string              `json:"name,omitempty"`
	Images      []ImageData         `json:"-"`
	Annotations []MessageAnnotation `json:"-"`
	ToolCallID  string              `json:"tool_call_id,omitempty"` // OpenAI tool role messages
}

// ImageData represents an image extracted from multimodal content.
type ImageData struct {
	Base64    string // raw base64 data without data: prefix
	MediaType string // e.g. "image/png"
	FileName  string // e.g. "upload.png"
}

// MessageAnnotation represents an image annotation attached to a WebSocket message.
type MessageAnnotation struct {
	ID                        string            `json:"id"`
	MessageAnnotationType     string            `json:"messageAnnotationType"`
	MessageAnnotationMetadata map[string]string `json:"messageAnnotationMetadata"`
}

// UnmarshalJSON implements custom JSON unmarshaling for Message to handle
// both string content and multimodal content arrays (OpenAI/Anthropic format).
// It also converts tool-related messages (tool role, tool_calls, tool_result,
// tool_use blocks) into plain text so the M365 backend can process them.
func (m *Message) UnmarshalJSON(data []byte) error {
	var raw struct {
		Role       string          `json:"role"`
		Content    json.RawMessage `json:"content"`
		Name       string          `json:"name,omitempty"`
		ToolCallID string          `json:"tool_call_id,omitempty"`
		ToolCalls  []struct {
			ID       string `json:"id"`
			Type     string `json:"type"`
			Function struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"function"`
		} `json:"tool_calls,omitempty"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	m.Role = raw.Role
	m.Name = raw.Name
	m.ToolCallID = raw.ToolCallID

	// Handle OpenAI assistant messages with tool_calls field
	if len(raw.ToolCalls) > 0 {
		var sb strings.Builder
		for _, tc := range raw.ToolCalls {
			fmt.Fprintf(&sb, "[Previous Tool Call: %s]\nArguments: %s\n\n", tc.Function.Name, tc.Function.Arguments)
		}
		m.Content = strings.TrimSpace(sb.String())
	}

	if len(raw.Content) == 0 {
		return nil
	}

	// Handle null content (e.g. assistant message with tool_calls and content=null)
	if string(raw.Content) == "null" {
		return nil
	}

	// Try string content first
	var s string
	if err := json.Unmarshal(raw.Content, &s); err == nil {
		m.Content = s
		// Convert tool role messages to formatted text
		if m.Role == "tool" && m.ToolCallID != "" {
			m.Content = fmt.Sprintf("[Tool Result (call_id: %s)]\n%s", m.ToolCallID, s)
		}
		return nil
	}

	// Try array of content blocks
	var blocks []map[string]any
	if err := json.Unmarshal(raw.Content, &blocks); err != nil {
		return fmt.Errorf("content must be string or array of content blocks")
	}

	for _, block := range blocks {
		blockType, _ := block["type"].(string)
		switch blockType {
		case "text":
			if txt, ok := block["text"].(string); ok {
				m.Content += txt
			}
		case "image_url":
			// OpenAI format: {"type": "image_url", "image_url": {"url": "data:image/png;base64,..."}}
			if imgURL, ok := block["image_url"].(map[string]any); ok {
				if url, ok := imgURL["url"].(string); ok {
					if img := parseDataURL(url); img != nil {
						m.Images = append(m.Images, *img)
					}
				}
			}
		case "image":
			// Anthropic format: {"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "..."}}
			if src, ok := block["source"].(map[string]any); ok {
				if srcType, ok := src["type"].(string); ok && srcType == "base64" {
					mediaType, _ := src["media_type"].(string)
					base64Data, _ := src["data"].(string)
					if base64Data != "" {
						m.Images = append(m.Images, ImageData{
							Base64:    base64Data,
							MediaType: mediaType,
							FileName:  "upload." + extFromMediaType(mediaType),
						})
					}
				}
			}
		case "tool_use":
			// Anthropic assistant message: previous tool call
			name, _ := block["name"].(string)
			input := block["input"]
			if inputBytes, err := json.Marshal(input); err == nil {
				m.Content += fmt.Sprintf("\n[Previous Tool Call: %s]\nArguments: %s\n", name, string(inputBytes))
			}
		case "tool_result":
			// Anthropic user message: tool result
			toolUseID, _ := block["tool_use_id"].(string)
			resultContent := ""
			if c, ok := block["content"].(string); ok {
				resultContent = c
			} else if cArr, ok := block["content"].([]any); ok {
				for _, cItem := range cArr {
					if cMap, ok := cItem.(map[string]any); ok {
						if txt, ok := cMap["text"].(string); ok {
							resultContent += txt
						}
					}
				}
			}
			m.Content += fmt.Sprintf("\n[Tool Result (call_id: %s)]\n%s\n", toolUseID, resultContent)
		}
	}

	return nil
}

// parseDataURL parses a data URL (data:image/png;base64,...) and returns ImageData.
func parseDataURL(url string) *ImageData {
	const prefix = "data:"
	if !strings.HasPrefix(url, prefix) {
		return nil
	}
	rest := url[len(prefix):]
	semiIdx := strings.Index(rest, ";")
	if semiIdx < 0 {
		return nil
	}
	mediaType := rest[:semiIdx]
	rest = rest[semiIdx+1:]
	encoding, base64Data, found := strings.Cut(rest, ",")
	if !found {
		return nil
	}
	if encoding != "base64" {
		return nil
	}
	return &ImageData{
		Base64:    base64Data,
		MediaType: mediaType,
		FileName:  "upload." + extFromMediaType(mediaType),
	}
}

// extFromMediaType returns the file extension for a media type.
func extFromMediaType(mediaType string) string {
	switch mediaType {
	case "image/png":
		return "png"
	case "image/jpeg":
		return "jpg"
	case "image/gif":
		return "gif"
	case "image/webp":
		return "webp"
	default:
		return "bin"
	}
}

// BuildURL constructs the WebSocket URL for M365 Copilot connection.
// Returns the complete URL, hex session ID, and UUID session ID.
func BuildURL(token, hexSID, conversationID, userOID, tenantID string) (string, string, string, error) {
	if userOID == "" || tenantID == "" {
		return "", "", "", fmt.Errorf("M365_USER_OID and M365_TENANT_ID are required")
	}

	if hexSID == "" {
		hexSID = uuid.New().String()
	}

	uuidSID := formatUUID(hexSID)

	baseURL := fmt.Sprintf("wss://substrate.office.com/m365Copilot/Chathub/%s@%s", userOID, tenantID)
	url := fmt.Sprintf("%s?chatsessionid=%s&XRoutingParameterSessionKey=%s&clientrequestid=%s&X-SessionId=%s",
		baseURL, hexSID, hexSID, hexSID, uuidSID)

	if conversationID != "" {
		url += fmt.Sprintf("&ConversationId=%s", conversationID)
	}

	url += fmt.Sprintf("&access_token=%s", token)
	url += fmt.Sprintf("&variants=%s", variants)
	url += "&source=%22officeweb%22&product=Office&agentHost=Bizchat.FullScreen"
	url += "&licenseType=Starter&isEdu=false&agent=web&scenario=OfficeWebIncludedCopilot"

	return url, hexSID, uuidSID, nil
}

// formatUUID converts a hex string (with or without dashes) to UUID format (8-4-4-4-12).
func formatUUID(hex string) string {
	hex = strings.ReplaceAll(hex, "-", "")
	if len(hex) < 32 {
		return hex
	}
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		hex[0:8], hex[8:12], hex[12:16], hex[16:20], hex[20:32])
}

// BuildPayload constructs a chat request payload for a single message.
// When hasTools is true, code_interpreter option flags are stripped to prevent
// M365 from intercepting file/code operations.
func BuildPayload(hexSID, uuidSID, text, tone, gptOverride string, enableFileUpload, hasTools bool, extraOptions []string) (string, error) {
	invocationID := uuid.New().String()
	options := getOptions(enableFileUpload, false, hasTools, extraOptions)

	payload := map[string]any{
		"type":         4,
		"invocationId": invocationID,
		"target":       "chat",
		"arguments": []map[string]any{
			{
				"source":                   "officeweb",
				"clientCorrelationId":      hexSID,
				"sessionId":                uuidSID,
				"message":                  buildFullMessage(hexSID, text, nil),
				"optionsSets":              options,
				"streamingMode":            "ConciseWithPadding",
				"spokenTextMode":           "None",
				"options":                  map[string]any{},
				"extraExtensionParameters": map[string]any{},
				"allowedMessageTypes":      allowedMessageTypes,
				"sliceIds":                 []string{},
				"tone":                     tone,
				"plugins": []map[string]string{
					{"Id": "BingWebSearch", "Source": "BuiltIn"},
				},
				"isStartOfSession":          false,
				"isSbsSupported":            true,
				"renderReferencesBehindEOS": true,
				"disconnectBehavior":        "continue",
			},
		},
	}

	if gptOverride != "" {
		args := payload["arguments"].([]map[string]any)
		args[0]["gptIdOverride"] = map[string]string{
			"id":     gptOverride,
			"source": "MOS3",
		}
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// BuildConversationPayload constructs a chat request payload with conversation history.
// When hasTools is true, code_interpreter option flags are stripped to prevent
// M365 from intercepting file/code operations.
func BuildConversationPayload(hexSID, uuidSID string, messages []Message, tone, gptOverride string, enableFileUpload, hasTools bool, extraOptions []string) (string, error) {
	invocationID := uuid.New().String()

	// Extract annotations from the last message (images are attached to the last user message)
	var annotations []MessageAnnotation
	hasImages := false
	lastText := ""
	if len(messages) > 0 {
		lastText = messages[len(messages)-1].Content
		annotations = messages[len(messages)-1].Annotations
		hasImages = len(annotations) > 0
	}

	// Merge system messages into the last message as a prefix.
	// M365 backend tracks conversation history via ConversationId, so only the
	// last message is sent. System prompts in earlier messages would be lost.
	// Prepending them to the last message ensures they reach the model.
	var systemParts []string
	for _, msg := range messages {
		if msg.Role == "system" && strings.TrimSpace(msg.Content) != "" {
			systemParts = append(systemParts, msg.Content)
		}
	}
	if len(systemParts) > 0 {
		systemPrefix := strings.Join(systemParts, "\n\n")
		lastText = systemPrefix + "\n\n" + lastText
	}

	options := getOptions(enableFileUpload, hasImages, hasTools, extraOptions)

	payload := map[string]any{
		"type":         4,
		"invocationId": invocationID,
		"target":       "chat",
		"arguments": []map[string]any{
			{
				"source":                   "officeweb",
				"clientCorrelationId":      hexSID,
				"sessionId":                uuidSID,
				"message":                  buildMinimalMessage(hexSID, lastText, annotations),
				"optionsSets":              options,
				"streamingMode":            "ConciseWithPadding",
				"spokenTextMode":           "None",
				"options":                  map[string]any{},
				"extraExtensionParameters": map[string]any{},
				"allowedMessageTypes":      allowedMessageTypes,
				"sliceIds":                 []string{},
				"tone":                     tone,
				"plugins": []map[string]string{
					{"Id": "BingWebSearch", "Source": "BuiltIn"},
				},
				"isStartOfSession":          false,
				"isSbsSupported":            true,
				"renderReferencesBehindEOS": true,
				"disconnectBehavior":        "continue",
			},
		},
	}

	if gptOverride != "" {
		args := payload["arguments"].([]map[string]any)
		args[0]["gptIdOverride"] = map[string]string{
			"id":     gptOverride,
			"source": "MOS3",
		}
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// buildFullMessage constructs a full message object for single-message requests.
// Uses the full entityAnnotationTypes and connectedFederatedConnections.
func buildFullMessage(hexSID, text string, annotations []MessageAnnotation) map[string]any {
	now := time.Now()
	_, offset := now.Zone()
	tzName := getTZName()

	msg := map[string]any{
		"author":                        "user",
		"inputMethod":                   "Keyboard",
		"text":                          text,
		"entityAnnotationTypes":         []string{"People", "File", "Event", "Email", "TeamsMessage"},
		"connectedFederatedConnections": []string{"dummyId"},
		"requestId":                     hexSID + "_0",
		"locationInfo": map[string]any{
			"timeZoneOffset": offset / 3600,
			"timeZone":       tzName,
		},
		"locale":            getLocale(),
		"messageType":       "Chat",
		"experienceType":    "Default",
		"adaptiveCards":     []any{},
		"clientPreferences": map[string]any{},
	}

	if len(annotations) > 0 {
		msg["messageAnnotations"] = annotations
	}

	return msg
}

// buildMinimalMessage constructs a minimal message object for conversation requests.
// Uses empty entityAnnotationTypes and no connectedFederatedConnections.
func buildMinimalMessage(hexSID, text string, annotations []MessageAnnotation) map[string]any {
	now := time.Now()
	_, offset := now.Zone()
	tzName := getTZName()

	msg := map[string]any{
		"author":                "user",
		"inputMethod":           "Keyboard",
		"text":                  text,
		"entityAnnotationTypes": []string{},
		"requestId":             hexSID + "_0",
		"locationInfo": map[string]any{
			"timeZoneOffset": offset / 3600,
			"timeZone":       tzName,
		},
		"locale":            getLocale(),
		"messageType":       "Chat",
		"experienceType":    "Default",
		"adaptiveCards":     []any{},
		"clientPreferences": map[string]any{},
	}

	if len(annotations) > 0 {
		msg["messageAnnotations"] = annotations
	}

	return msg
}

// getTZName returns the system timezone name.
// Tries TZ env var, then /etc/localtime symlink, then falls back to UTC.
func getTZName() string {
	if tz := os.Getenv("TZ"); tz != "" {
		return tz
	}
	// On Unix/macOS, /etc/localtime is a symlink to the timezone file
	if link, err := os.Readlink("/etc/localtime"); err == nil {
		// Path looks like /var/db/timezone/zoneinfo/Europe/Istanbul
		// or ../zoneinfo/Europe/Istanbul
		parts := strings.Split(link, "/")
		for i, p := range parts {
			if p == "zoneinfo" && i+1 < len(parts) {
				return strings.Join(parts[i+1:], "/")
			}
		}
		// Try reading the link target directly
		if resolved, err := filepath.EvalSymlinks("/etc/localtime"); err == nil {
			parts := strings.Split(resolved, "/")
			for i, p := range parts {
				if p == "zoneinfo" && i+1 < len(parts) {
					return strings.Join(parts[i+1:], "/")
				}
			}
		}
	}
	return "UTC"
}

// getLocale returns the system locale, defaulting to "en-us".
func getLocale() string {
	for _, env := range []string{"LANG", "LC_ALL", "LC_MESSAGES"} {
		lang := os.Getenv(env)
		if lang == "" || lang == "C" || lang == "POSIX" || lang == "c" {
			continue
		}
		lang = strings.SplitN(lang, ".", 2)[0]
		lang = strings.ReplaceAll(lang, "_", "-")
		return strings.ToLower(lang)
	}
	return "en-us"
}

// getOptions returns the appropriate option set based on feature flags.
// When hasTools is true, code_interpreter flags are stripped to prevent M365
// from routing file/code operations to its own sandbox.
func getOptions(enableFileUpload, enableImageUpload, hasTools bool, extraOptions []string) []string {
	options := make([]string, 0, len(optionsSetsFull))
	options = append(options, optionsSetsFull...)

	if !enableFileUpload {
		filtered := make([]string, 0, len(options))
		for _, opt := range options {
			if !fileUploadOptions[opt] {
				filtered = append(filtered, opt)
			}
		}
		options = filtered
	}

	// Strip code_interpreter flags when client-defined tools are present.
	// This is the primary infrastructure lever: without it, M365 intercepts
	// file operations before the LLM processes text instructions.
	if hasTools {
		filtered := make([]string, 0, len(options))
		for _, opt := range options {
			if !codeInterpreterOptions[opt] {
				filtered = append(filtered, opt)
			}
		}
		options = filtered
	}

	if enableImageUpload {
		for opt := range imageUploadOptions {
			options = append(options, opt)
		}
	}

	if len(extraOptions) > 0 {
		options = append(options, extraOptions...)
	}

	return options
}
