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

// imageUploadOptions contains option flags needed for image upload support.
var imageUploadOptions = map[string]bool{
	"cwc_flux_image":                                    true,
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
	Role        string             `json:"role"`
	Content     string             `json:"content"`
	Name        string             `json:"name,omitempty"`
	Images      []ImageData        `json:"-"`
	Annotations []MessageAnnotation `json:"-"`
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
func (m *Message) UnmarshalJSON(data []byte) error {
	var raw struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
		Name    string          `json:"name,omitempty"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	m.Role = raw.Role
	m.Name = raw.Name

	if len(raw.Content) == 0 {
		return nil
	}

	// Try string content first
	var s string
	if err := json.Unmarshal(raw.Content, &s); err == nil {
		m.Content = s
		return nil
	}

	// Try array of content blocks
	var blocks []map[string]interface{}
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
			if imgURL, ok := block["image_url"].(map[string]interface{}); ok {
				if url, ok := imgURL["url"].(string); ok {
					if img := parseDataURL(url); img != nil {
						m.Images = append(m.Images, *img)
					}
				}
			}
		case "image":
			// Anthropic format: {"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "..."}}
			if src, ok := block["source"].(map[string]interface{}); ok {
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
	commaIdx := strings.Index(rest, ",")
	if commaIdx < 0 {
		return nil
	}
	encoding := rest[:commaIdx]
	base64Data := rest[commaIdx+1:]
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
func BuildPayload(hexSID, uuidSID, text, tone, gptOverride string, enableFileUpload bool, extraOptions []string) (string, error) {
	invocationID := uuid.New().String()
	options := getOptions(enableFileUpload, false, extraOptions)

	payload := map[string]interface{}{
		"type":         4,
		"invocationId": invocationID,
		"target":       "chat",
		"arguments": []map[string]interface{}{
			{
				"source":                "officeweb",
				"clientCorrelationId":   hexSID,
				"sessionId":             uuidSID,
				"message":              buildFullMessage(hexSID, text, nil),
				"optionsSets":           options,
				"streamingMode":         "ConciseWithPadding",
				"spokenTextMode":        "None",
				"options":               map[string]interface{}{},
				"extraExtensionParameters": map[string]interface{}{},
				"allowedMessageTypes":   allowedMessageTypes,
				"sliceIds":              []string{},
				"tone":                  tone,
				"plugins": []map[string]string{
					{"Id": "BingWebSearch", "Source": "BuiltIn"},
				},
				"isStartOfSession":      false,
				"isSbsSupported":        true,
				"renderReferencesBehindEOS": true,
				"disconnectBehavior":    "continue",
			},
		},
	}

	if gptOverride != "" {
		args := payload["arguments"].([]map[string]interface{})
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
func BuildConversationPayload(hexSID, uuidSID string, messages []Message, tone, gptOverride string, enableFileUpload bool, extraOptions []string) (string, error) {
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

	options := getOptions(enableFileUpload, hasImages, extraOptions)

	payload := map[string]interface{}{
		"type":         4,
		"invocationId": invocationID,
		"target":       "chat",
		"arguments": []map[string]interface{}{
			{
				"source":                "officeweb",
				"clientCorrelationId":   hexSID,
				"sessionId":             uuidSID,
				"message":              buildMinimalMessage(hexSID, lastText, annotations),
				"optionsSets":           options,
				"streamingMode":         "ConciseWithPadding",
				"spokenTextMode":        "None",
				"options":               map[string]interface{}{},
				"extraExtensionParameters": map[string]interface{}{},
				"allowedMessageTypes":   allowedMessageTypes,
				"sliceIds":              []string{},
				"tone":                  tone,
				"plugins": []map[string]string{
					{"Id": "BingWebSearch", "Source": "BuiltIn"},
				},
				"isStartOfSession":      false,
				"isSbsSupported":        true,
				"renderReferencesBehindEOS": true,
				"disconnectBehavior":    "continue",
			},
		},
	}

	if gptOverride != "" {
		args := payload["arguments"].([]map[string]interface{})
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
func buildFullMessage(hexSID, text string, annotations []MessageAnnotation) map[string]interface{} {
	now := time.Now()
	_, offset := now.Zone()
	tzName := getTZName()

	msg := map[string]interface{}{
		"author":             "user",
		"inputMethod":        "Keyboard",
		"text":               text,
		"entityAnnotationTypes": []string{"People", "File", "Event", "Email", "TeamsMessage"},
		"connectedFederatedConnections": []string{"dummyId"},
		"requestId":          hexSID + "_0",
		"locationInfo": map[string]interface{}{
			"timeZoneOffset": offset / 3600,
			"timeZone":       tzName,
		},
		"locale":             getLocale(),
		"messageType":        "Chat",
		"experienceType":     "Default",
		"adaptiveCards":      []interface{}{},
		"clientPreferences":  map[string]interface{}{},
	}

	if len(annotations) > 0 {
		msg["messageAnnotations"] = annotations
	}

	return msg
}

// buildMinimalMessage constructs a minimal message object for conversation requests.
// Uses empty entityAnnotationTypes and no connectedFederatedConnections.
func buildMinimalMessage(hexSID, text string, annotations []MessageAnnotation) map[string]interface{} {
	now := time.Now()
	_, offset := now.Zone()
	tzName := getTZName()

	msg := map[string]interface{}{
		"author":             "user",
		"inputMethod":        "Keyboard",
		"text":               text,
		"entityAnnotationTypes": []string{},
		"requestId":          hexSID + "_0",
		"locationInfo": map[string]interface{}{
			"timeZoneOffset": offset / 3600,
			"timeZone":       tzName,
		},
		"locale":             getLocale(),
		"messageType":        "Chat",
		"experienceType":     "Default",
		"adaptiveCards":      []interface{}{},
		"clientPreferences":  map[string]interface{}{},
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

// buildM365History converts OpenAI-style messages to M365 format.
func buildM365History(messages []Message) []map[string]interface{} {
	if len(messages) <= 1 {
		return nil
	}

	history := make([]map[string]interface{}, 0, len(messages)-1)
	lastText := ""

	for _, msg := range messages[:len(messages)-1] {
		switch msg.Role {
		case "user":
			content := msg.Content
			if content == "" {
				content = lastText
			}
			history = append(history, map[string]interface{}{
				"author":         "user",
				"inputMethod":    "Keyboard",
				"text":           content,
				"messageType":    "Chat",
				"experienceType": "Default",
				"adaptiveCards":  []interface{}{},
				"clientPreferences": map[string]interface{}{},
			})
			lastText = content
		case "assistant":
			if msg.Content != "" {
				history = append(history, map[string]interface{}{
					"author":      "bot",
					"text":        msg.Content,
					"messageType": "Chat",
				})
				lastText = msg.Content
			}
		case "tool":
			history = append(history, map[string]interface{}{
				"author":         "user",
				"inputMethod":    "Keyboard",
				"text":           fmt.Sprintf("[Tool result: %s]", msg.Content),
				"messageType":    "Chat",
				"adaptiveCards":  []interface{}{},
				"clientPreferences": map[string]interface{}{},
			})
		}
	}

	return history
}

// getOptions returns the appropriate option set based on feature flags.
func getOptions(enableFileUpload, enableImageUpload bool, extraOptions []string) []string {
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
