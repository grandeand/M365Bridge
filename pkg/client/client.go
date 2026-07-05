// Package client provides WebSocket client for M365 Copilot communication.
// It handles SignalR protocol, message parsing, streaming responses, and tool call extraction.
package client

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/KilimcininKorOglu/M365Bridge/pkg/auth"
	"github.com/KilimcininKorOglu/M365Bridge/pkg/models"
	"github.com/KilimcininKorOglu/M365Bridge/pkg/payload"
)

const (
	// signalRDelimiter is the delimiter used in SignalR protocol.
	signalRDelimiter = "\x1e"
	// handshakeMessage is the SignalR handshake message.
	handshakeMessage = `{"protocol":"json","version":1}` + signalRDelimiter
	// defaultHandshakeTimeout is the timeout for WebSocket handshake.
	defaultHandshakeTimeout = 15 * time.Second
	// defaultRecvTimeout is the timeout for receiving messages.
	defaultRecvTimeout = 45 * time.Second
	// defaultRecvFinalTimeout is the timeout for final message in streaming.
	defaultRecvFinalTimeout = 60 * time.Second
)

var (
	// ErrConnectionClosed is returned when the WebSocket connection is closed.
	ErrConnectionClosed = errors.New("connection closed")
	// ErrHandshakeFailed is returned when SignalR handshake fails.
	ErrHandshakeFailed = errors.New("handshake failed")
)

// ToolCall represents a detected tool call from the response.
type ToolCall struct {
	ID       string                 `json:"id"`
	Type     string                 `json:"type"`
	Function ToolCallFunction       `json:"function"`
}

// ToolCallFunction represents the function part of a tool call.
type ToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// M365Client handles WebSocket communication with M365 Copilot.
type M365Client struct {
	tokenManager      *auth.TokenManager
	handshakeTimeout  time.Duration
	recvTimeout       time.Duration
	recvFinalTimeout  time.Duration

	// requestMutex serializes requests to protect shared result state.
	// Each request opens its own WebSocket connection (per-request model),
	// but the result fields below are shared on the client instance.
	requestMutex       sync.Mutex

	lastToolCalls      []ToolCall
	lastFinishReason   string
	lastFullText       string
	lastThinking       string
	lastConversationID string
	connMutex          sync.RWMutex
}

// NewM365Client creates a new M365 client instance.
func NewM365Client(tokenManager *auth.TokenManager) *M365Client {
	return &M365Client{
		tokenManager:     tokenManager,
		handshakeTimeout: defaultHandshakeTimeout,
		recvTimeout:      defaultRecvTimeout,
		recvFinalTimeout: defaultRecvFinalTimeout,
	}
}

// Close is a no-op now; per-request connections are closed by dialConnection callers.
func (c *M365Client) Close() error {
	return nil
}

// UploadResult represents the response from the M365 UploadFile endpoint.
type UploadResult struct {
	DocID      string `json:"docId"`
	FileName   string `json:"fileName"`
	FileType   string `json:"fileType"`
	IsSuccess  bool
}

// UploadFile uploads an image to M365 Copilot via the UploadFile HTTP endpoint.
// The base64Data should be raw base64 (without data: prefix).
// conversationID is the M365 conversation ID (use a random UUID for new conversations).
// userOID and tenantID are used for the x-anchormailbox header.
func (c *M365Client) UploadFile(base64Data, mediaType, fileName, conversationID, userOID, tenantID string) (*UploadResult, error) {
	token, err := c.tokenManager.Get()
	if err != nil {
		return nil, fmt.Errorf("failed to get token: %w", err)
	}

	dataURL := fmt.Sprintf("data:%s;base64,%s", mediaType, base64Data)

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	writer.WriteField("scenario", "UploadImage")
	writer.WriteField("conversationId", conversationID)
	writer.WriteField("FileBase64", dataURL)
	writer.WriteField("optionsSets", "gptvnorm2048")
	writer.Close()

	req, err := http.NewRequest("POST", "https://substrate.office.com/m365Copilot/UploadFile", &body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Origin", "https://m365.cloud.microsoft")
	req.Header.Set("x-scenario", "OfficeWebIncludedCopilot")
	req.Header.Set("x-variants", "feature.EnableImageSupportInUploadFile")
	if userOID != "" && tenantID != "" {
		req.Header.Set("x-anchormailbox", fmt.Sprintf("Oid:%s@%s", userOID, tenantID))
	}

	httpClient := &http.Client{Timeout: 30 * time.Second}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("upload request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read upload response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("upload failed with status %d: %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		FileName string `json:"fileName"`
		FileType string `json:"fileType"`
		DocID    string `json:"docId"`
		Result   struct {
			Value string `json:"value"`
		} `json:"result"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("failed to parse upload response: %w", err)
	}

	return &UploadResult{
		DocID:     result.DocID,
		FileName:  result.FileName,
		FileType:  result.FileType,
		IsSuccess: result.Result.Value == "Success",
	}, nil
}

// dialConnection opens a new WebSocket connection for a single request.
// The caller is responsible for closing the connection when done.
func (c *M365Client) dialConnection(conversationID, userOID, tenantID string) (*websocket.Conn, string, string, error) {
	token, err := c.tokenManager.Get()
	if err != nil {
		return nil, "", "", fmt.Errorf("failed to get token: %w", err)
	}

	hexSID := strings.ReplaceAll(uuid.New().String(), "-", "")
	uuidSID := formatUUID(hexSID)

	url, _, _, err := payload.BuildURL(token, hexSID, conversationID, userOID, tenantID)
	if err != nil {
		return nil, "", "", err
	}

	dialer := websocket.Dialer{
		HandshakeTimeout: c.handshakeTimeout,
	}

	conn, _, err := dialer.Dial(url, nil)
	if err != nil {
		return nil, "", "", fmt.Errorf("failed to dial: %w", err)
	}

	if err := conn.WriteMessage(websocket.TextMessage, []byte(handshakeMessage)); err != nil {
		conn.Close()
		return nil, "", "", fmt.Errorf("%w: %v", ErrHandshakeFailed, err)
	}

	conn.SetReadDeadline(time.Now().Add(c.handshakeTimeout))
	_, _, err = conn.ReadMessage()
	if err != nil {
		conn.Close()
		return nil, "", "", fmt.Errorf("%w: %v", ErrHandshakeFailed, err)
	}
	conn.SetReadDeadline(time.Time{})

	return conn, hexSID, uuidSID, nil
}

// Chat sends a single message and returns the complete response.
// When hasTools is true, code_interpreter option flags are stripped from the payload.
func (c *M365Client) Chat(text, tone, gptOverride, conversationID, userOID, tenantID string, hasTools bool) (string, error) {
	conn, hexSID, uuidSID, err := c.dialConnection(conversationID, userOID, tenantID)
	if err != nil {
		return "", err
	}
	defer conn.Close()

	payloadStr, err := payload.BuildPayload(hexSID, uuidSID, text, tone, gptOverride, false, hasTools, nil)
	if err != nil {
		return "", err
	}

	return c.sendRecv(conn, payloadStr)
}

// ChatStream sends a message and streams the response to stdout.
// Returns the complete text.
func (c *M365Client) ChatStream(text, tone, gptOverride, conversationID, userOID, tenantID string, hasTools bool) (string, error) {
	fullText := ""

	ch := c.ChatStreamGen(text, tone, gptOverride, conversationID, userOID, tenantID, hasTools)
	for chunk := range ch {
		if chunk.Error != nil {
			return "", chunk.Error
		}
		if !chunk.IsFinal {
			fullText += chunk.Text
		}
	}

	return fullText, nil
}

// StreamChunk represents a chunk of streamed response.
type StreamChunk struct {
	Text     string
	Thinking string
	IsFinal  bool
	Error    error
}

// ChatStreamGen generates a stream of response chunks.
// When hasTools is true, code_interpreter option flags are stripped from the payload.
func (c *M365Client) ChatStreamGen(text, tone, gptOverride, conversationID, userOID, tenantID string, hasTools bool) <-chan StreamChunk {
	ch := make(chan StreamChunk)

	go func() {
		defer close(ch)

		conn, hexSID, uuidSID, err := c.dialConnection(conversationID, userOID, tenantID)
		if err != nil {
			ch <- StreamChunk{Error: err}
			return
		}
		defer conn.Close()

		payloadStr, err := payload.BuildPayload(hexSID, uuidSID, text, tone, gptOverride, false, hasTools, nil)
		if err != nil {
			ch <- StreamChunk{Error: err}
			return
		}

		if err := conn.WriteMessage(websocket.TextMessage, []byte(payloadStr+signalRDelimiter)); err != nil {
			ch <- StreamChunk{Error: err}
			return
		}

		sentText := ""

		for {
			conn.SetReadDeadline(time.Now().Add(c.recvTimeout))
			msgType, message, err := conn.ReadMessage()
			if err != nil {
				if websocket.IsCloseError(err) || websocket.IsUnexpectedCloseError(err) {
					ch <- StreamChunk{Error: ErrConnectionClosed}
				} else {
					ch <- StreamChunk{Error: err}
				}
				return
			}
			conn.SetReadDeadline(time.Time{})

			if msgType != websocket.TextMessage {
				continue
			}

			text := string(message)
			parts := strings.Split(text, signalRDelimiter)

			for _, part := range parts {
				part = strings.TrimSpace(part)
				if part == "" {
					continue
				}

				var data map[string]interface{}
				if err := json.Unmarshal([]byte(part), &data); err != nil {
					continue
				}

				if msgType, ok := data["type"].(float64); ok && int(msgType) == 1 {
					if target, ok := data["target"].(string); ok && target == "update" {
						if args, ok := data["arguments"].([]interface{}); ok {
							for _, arg := range args {
								if argMap, ok := arg.(map[string]interface{}); ok {
									// Extract conversationId from type:1 update if present (rare)
									if convID, ok := argMap["conversationId"].(string); ok && convID != "" {
										c.connMutex.Lock()
										c.lastConversationID = convID
										c.connMutex.Unlock()
									}
									// Handle writeAtCursor (delta text)
									if writeAtCursor, ok := argMap["writeAtCursor"].(string); ok {
										sentText += writeAtCursor
										ch <- StreamChunk{Text: writeAtCursor, IsFinal: false}
									}
									// Handle messages[] - extract thinking from Progress messages and text from last message
									if msgs, ok := argMap["messages"].([]interface{}); ok && len(msgs) > 0 {
										// Scan all messages for thinking (Progress + ChainOfThoughtSummary)
										for _, msg := range msgs {
											if msgMap, ok := msg.(map[string]interface{}); ok {
												if mt, _ := msgMap["messageType"].(string); mt == "Progress" {
													if co, _ := msgMap["contentOrigin"].(string); co == "ChainOfThoughtSummary" {
														if t, _ := msgMap["text"].(string); t != "" {
															ch <- StreamChunk{Thinking: t, IsFinal: false}
														}
													}
												}
											}
										}
										// Handle last message text (accumulated full text, skip Progress)
										// Compute diff from sentText to avoid duplication
										if lastMsg, ok := msgs[len(msgs)-1].(map[string]interface{}); ok {
											if lastMsgType, _ := lastMsg["messageType"].(string); lastMsgType != "Progress" {
												if msgText, ok := lastMsg["text"].(string); ok && msgText != "" {
													if _, hasCursor := argMap["writeAtCursor"]; !hasCursor {
														if msgText != sentText {
															var chunk string
															if strings.HasPrefix(msgText, sentText) {
																chunk = msgText[len(sentText):]
															} else {
																chunk = msgText
															}
															sentText = msgText
															if chunk != "" {
																ch <- StreamChunk{Text: chunk, IsFinal: false}
															}
														}
													}
												}
											}
										}
									}
								}
							}
						}
					}
				} else if msgType, ok := data["type"].(float64); ok && int(msgType) == 2 {
					// type: 2 is invocation completion; contains item.conversationId
					if item, ok := data["item"].(map[string]interface{}); ok {
						if convID, ok := item["conversationId"].(string); ok && convID != "" {
							c.connMutex.Lock()
							c.lastConversationID = convID
							c.connMutex.Unlock()
						}
					}
				} else if msgType, ok := data["type"].(float64); ok && int(msgType) == 3 {
					ch <- StreamChunk{Text: "", IsFinal: true}
					return
				}
			}
		}
	}()

	return ch
}

// ChatConversation sends a conversation with history and returns the response.
// When hasTools is true, code_interpreter option flags are stripped from the payload.
func (c *M365Client) ChatConversation(messages []payload.Message, tone, gptOverride, conversationID, userOID, tenantID string, hasTools bool) (string, string, []ToolCall, string, error) {
	ch := c.ChatConversationStreamGen(messages, tone, gptOverride, conversationID, userOID, tenantID, hasTools)

	for chunk := range ch {
		if chunk.Error != nil {
			return "", "", nil, "", chunk.Error
		}
	}

	c.connMutex.RLock()
	fullText := c.lastFullText
	thinking := c.lastThinking
	toolCalls := c.lastToolCalls
	finishReason := c.lastFinishReason
	c.connMutex.RUnlock()

	return cleanText(fullText), thinking, toolCalls, finishReason, nil
}

// ConversationStreamChunk represents a chunk of conversation stream.
type ConversationStreamChunk struct {
	Text     string
	Thinking string
	IsFinal  bool
	Error    error
}

// ChatConversationStreamGen generates a stream of conversation response chunks.
// When hasTools is true, code_interpreter option flags are stripped from the payload.
func (c *M365Client) ChatConversationStreamGen(messages []payload.Message, tone, gptOverride, conversationID, userOID, tenantID string, hasTools bool) <-chan ConversationStreamChunk {
	ch := make(chan ConversationStreamChunk)

	go func() {
		defer close(ch)

		c.requestMutex.Lock()
		defer c.requestMutex.Unlock()

		conn, hexSID, uuidSID, err := c.dialConnection(conversationID, userOID, tenantID)
		if err != nil {
			ch <- ConversationStreamChunk{Error: err}
			return
		}
		defer conn.Close()

		payloadStr, err := payload.BuildConversationPayload(hexSID, uuidSID, messages, tone, gptOverride, false, hasTools, nil)
		if err != nil {
			ch <- ConversationStreamChunk{Error: err}
			return
		}

		if err := conn.WriteMessage(websocket.TextMessage, []byte(payloadStr+signalRDelimiter)); err != nil {
			ch <- ConversationStreamChunk{Error: err}
			return
		}

		toolCalls := []ToolCall{}

		c.connMutex.Lock()
		c.lastFullText = ""
		c.lastThinking = ""
		c.connMutex.Unlock()

		for {
			conn.SetReadDeadline(time.Now().Add(c.recvFinalTimeout))
			msgType, message, err := conn.ReadMessage()
			if err != nil {
				if websocket.IsCloseError(err) || websocket.IsUnexpectedCloseError(err) {
					ch <- ConversationStreamChunk{Error: ErrConnectionClosed}
				} else {
					ch <- ConversationStreamChunk{Error: err}
				}
				return
			}
			conn.SetReadDeadline(time.Time{})

			if msgType != websocket.TextMessage {
				continue
			}

			text := string(message)
			parts := strings.Split(text, signalRDelimiter)

			for _, part := range parts {
				part = strings.TrimSpace(part)
				if part == "" {
					continue
				}

				var data map[string]interface{}
				if err := json.Unmarshal([]byte(part), &data); err != nil {
					continue
				}

				if msgType, ok := data["type"].(float64); ok && int(msgType) == 1 {
					if target, ok := data["target"].(string); ok && target == "update" {
						if args, ok := data["arguments"].([]interface{}); ok {
							for _, arg := range args {
								if argMap, ok := arg.(map[string]interface{}); ok {
									// Extract conversationId from type:1 update if present (rare)
									if convID, ok := argMap["conversationId"].(string); ok && convID != "" {
										c.connMutex.Lock()
										c.lastConversationID = convID
										c.connMutex.Unlock()
									}
									if msgs, ok := argMap["messages"].([]interface{}); ok {
										// Check all messages for tool calls and thinking
										for _, msg := range msgs {
											if msgMap, ok := msg.(map[string]interface{}); ok {
												if messageType, ok := msgMap["messageType"].(string); ok {
													if funcName, exists := models.ToolMessageType[messageType]; exists {
														if tc := extractToolCall(msgMap, funcName); tc != nil {
															toolCalls = append(toolCalls, *tc)
														}
													}
													// Extract thinking from Progress + ChainOfThoughtSummary
													if messageType == "Progress" {
														if co, _ := msgMap["contentOrigin"].(string); co == "ChainOfThoughtSummary" {
															if t, _ := msgMap["text"].(string); t != "" {
																c.connMutex.Lock()
																c.lastThinking += t
																c.connMutex.Unlock()
																ch <- ConversationStreamChunk{Thinking: t, IsFinal: false}
															}
														}
														// Extract web search tool calls from searchQueries field
														if sq, ok := msgMap["searchQueries"].([]interface{}); ok && len(sq) > 0 {
															for _, q := range sq {
																if query, ok := q.(string); ok && query != "" {
																	tc := makeSearchToolCall(query, msgMap)
																	toolCalls = append(toolCalls, *tc)
																}
															}
														}
													}
												}
											}
										}
										// Only process text from the last message (skip Progress messages)
										if len(msgs) > 0 {
											if lastMsg, ok := msgs[len(msgs)-1].(map[string]interface{}); ok {
												if lastMsgType, _ := lastMsg["messageType"].(string); lastMsgType != "Progress" {
													if newText, ok := lastMsg["text"].(string); ok && newText != "" {
														c.connMutex.Lock()
														if newText != c.lastFullText {
															var chunk string
															if strings.HasPrefix(newText, c.lastFullText) {
																chunk = newText[len(c.lastFullText):]
															} else {
																chunk = newText
															}
															c.lastFullText = newText
															if chunk != "" {
																ch <- ConversationStreamChunk{Text: chunk, IsFinal: false}
															}
														}
														c.connMutex.Unlock()
													}
												}
											}
										}
									}
									if writeAtCursor, ok := argMap["writeAtCursor"].(string); ok {
										c.connMutex.Lock()
										c.lastFullText += writeAtCursor
										ch <- ConversationStreamChunk{Text: writeAtCursor, IsFinal: false}
										c.connMutex.Unlock()
									}
								}
							}
						}
					}
				} else if msgType, ok := data["type"].(float64); ok && int(msgType) == 2 {
					// type: 2 is invocation completion; contains item.conversationId
					if item, ok := data["item"].(map[string]interface{}); ok {
						if convID, ok := item["conversationId"].(string); ok && convID != "" {
							c.connMutex.Lock()
							c.lastConversationID = convID
							c.connMutex.Unlock()
						}
					}
				} else if msgType, ok := data["type"].(float64); ok && int(msgType) == 3 {
					c.connMutex.Lock()
					c.lastToolCalls = toolCalls
					if len(toolCalls) > 0 {
						c.lastFinishReason = "tool_calls"
					} else {
						c.lastFinishReason = "stop"
					}
					c.connMutex.Unlock()
					ch <- ConversationStreamChunk{Text: "", IsFinal: true}
					return
				} else if msgType, ok := data["type"].(float64); ok && int(msgType) == -1 {
					ch <- ConversationStreamChunk{Error: fmt.Errorf("server error: %v", data)}
					return
				}
			}
		}
	}()

	return ch
}

// sendRecv sends a payload and waits for the complete response.
func (c *M365Client) sendRecv(conn *websocket.Conn, payload string) (string, error) {
	if err := conn.WriteMessage(websocket.TextMessage, []byte(payload+signalRDelimiter)); err != nil {
		return "", err
	}

	fullText := ""

	for {
		conn.SetReadDeadline(time.Now().Add(c.recvTimeout))
		msgType, message, err := conn.ReadMessage()
		if err != nil {
			return "", err
		}
		conn.SetReadDeadline(time.Time{})

		if msgType != websocket.TextMessage {
			continue
		}

		text := string(message)
		parts := strings.Split(text, signalRDelimiter)

		for _, part := range parts {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}

			var data map[string]interface{}
			if err := json.Unmarshal([]byte(part), &data); err != nil {
				continue
			}

			if msgType, ok := data["type"].(float64); ok && int(msgType) == 1 {
				if target, ok := data["target"].(string); ok && target == "update" {
					if args, ok := data["arguments"].([]interface{}); ok {
						for _, arg := range args {
							if argMap, ok := arg.(map[string]interface{}); ok {
								if msgs, ok := argMap["messages"].([]interface{}); ok && len(msgs) > 0 {
									if lastMsg, ok := msgs[len(msgs)-1].(map[string]interface{}); ok {
										if text, ok := lastMsg["text"].(string); ok {
											fullText = text
										}
									}
								}
							}
						}
					}
				}
			} else if msgType, ok := data["type"].(float64); ok && int(msgType) == 3 {
				return fullText, nil
			}
		}
	}
}

// extractToolCall extracts a tool call from a message.
func extractToolCall(msg map[string]interface{}, funcName string) *ToolCall {
	messageType, _ := msg["messageType"].(string)
	text, _ := msg["text"].(string)

	if messageType == "" || text == "" {
		return nil
	}

	var args string
	switch messageType {
	case "InternalSearchQuery":
		query := strings.TrimPrefix(text, "search: ")
		argsMap := map[string]string{"query": query}
		argsBytes, _ := json.Marshal(argsMap)
		args = string(argsBytes)
	case "GeneratedCode":
		argsMap := map[string]string{"code": text}
		argsBytes, _ := json.Marshal(argsMap)
		args = string(argsBytes)
	default:
		argsMap := map[string]string{"input": text}
		argsBytes, _ := json.Marshal(argsMap)
		args = string(argsBytes)
	}

	messageID, _ := msg["messageId"].(string)
	if messageID == "" {
		messageID = generateUUID()
	}

	return &ToolCall{
		ID:   messageID,
		Type: "function",
		Function: ToolCallFunction{
			Name:      funcName,
			Arguments: args,
		},
	}
}

// generateUUID generates a random UUID string.
func generateUUID() string {
	return uuid.New().String()
}

// makeSearchToolCall creates a ToolCall for a web search query extracted from
// the searchQueries field of a Progress message.
func makeSearchToolCall(query string, msg map[string]interface{}) *ToolCall {
	argsMap := map[string]string{"query": query}
	argsBytes, _ := json.Marshal(argsMap)

	messageID, _ := msg["messageId"].(string)
	if messageID == "" {
		messageID = generateUUID()
	}

	return &ToolCall{
		ID:   messageID,
		Type: "function",
		Function: ToolCallFunction{
			Name:      "search",
			Arguments: string(argsBytes),
		},
	}
}

// formatUUID converts a UUID string to standard UUID format (8-4-4-4-12).
// Accepts both dashed (36-char) and undashed (32-char) UUID strings.
func formatUUID(s string) string {
	// Strip dashes if present
	hex := strings.ReplaceAll(s, "-", "")
	if len(hex) < 32 {
		return s
	}
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		hex[0:8], hex[8:12], hex[12:16], hex[16:20], hex[20:32])
}

// cleanText removes non-printable characters from text.
func cleanText(text string) string {
	if text == "" {
		return ""
	}

	// Remove non-printable characters except newline, tab, carriage return
	var result strings.Builder
	for _, r := range text {
		if r == '\n' || r == '\t' || r == '\r' || (r >= 32 && r <= 126) || r > 127 {
			result.WriteRune(r)
		}
	}

	cleaned := result.String()

	// Remove control characters at end
	re := regexp.MustCompile(`[\x00-\x1f\x7f]{1,3}$`)
	cleaned = re.ReplaceAllString(cleaned, "")

	return strings.TrimSpace(cleaned)
}

// LastToolCalls returns the tool calls from the last conversation.
func (c *M365Client) LastToolCalls() []ToolCall {
	c.connMutex.RLock()
	defer c.connMutex.RUnlock()
	return c.lastToolCalls
}

// LastConversationID returns the conversation ID from the last response.
func (c *M365Client) LastConversationID() string {
	c.connMutex.RLock()
	defer c.connMutex.RUnlock()
	return c.lastConversationID
}

// LastThinking returns the thinking content from the last response.
func (c *M365Client) LastThinking() string {
	c.connMutex.RLock()
	defer c.connMutex.RUnlock()
	return c.lastThinking
}
