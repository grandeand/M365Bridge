// Package servers provides HTTP API server for M365 Copilot.
// This file implements OpenAI-compatible and Anthropic-compatible API endpoints.
package servers

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/KilimcininKorOglu/M365Bridge/pkg/auth"
	"github.com/KilimcininKorOglu/M365Bridge/pkg/client"
	"github.com/KilimcininKorOglu/M365Bridge/pkg/models"
	"github.com/KilimcininKorOglu/M365Bridge/pkg/payload"
	"github.com/KilimcininKorOglu/M365Bridge/pkg/toolcalling"
	"github.com/pkoukk/tiktoken-go"
)

const (
	// contextCacheDir is the directory for context cache files.
	contextCacheDir = "data/cache"
	// contextCacheMaxSize is the maximum number of in-memory cache entries.
	contextCacheMaxSize = 256
)

// ContextCache provides session-based conversation persistence across requests.
type ContextCache struct {
	cacheDir string
	mu       sync.RWMutex
	mem      map[string]string
	order    []string
}

// NewContextCache creates a new context cache instance.
func NewContextCache(cacheDir string) *ContextCache {
	os.MkdirAll(cacheDir, 0700)
	return &ContextCache{
		cacheDir: cacheDir,
		mem:      make(map[string]string),
	}
}

// path returns the file path for a cache key.
func (cc *ContextCache) path(key string) string {
	hash := md5.Sum([]byte(key))
	safe := hex.EncodeToString(hash[:])
	return filepath.Join(cc.cacheDir, safe+".json")
}

// Get retrieves a conversation ID by session key.
func (cc *ContextCache) Get(key string) string {
	cc.mu.RLock()
	if val, ok := cc.mem[key]; ok {
		cc.mu.RUnlock()
		return val
	}
	cc.mu.RUnlock()

	data, err := os.ReadFile(cc.path(key))
	if err != nil {
		return ""
	}
	var convID string
	if err := json.Unmarshal(data, &convID); err != nil {
		return ""
	}

	cc.mu.Lock()
	cc.mem[key] = convID
	cc.order = append(cc.order, key)
	cc.evict()
	cc.mu.Unlock()

	return convID
}

// Set stores a conversation ID by session key.
func (cc *ContextCache) Set(key, convID string) {
	cc.mu.Lock()
	cc.mem[key] = convID
	if idx := indexOf(cc.order, key); idx >= 0 {
		cc.order = append(cc.order[:idx], cc.order[idx+1:]...)
	}
	cc.order = append(cc.order, key)
	cc.evict()
	cc.mu.Unlock()

	data, _ := json.Marshal(convID)
	os.WriteFile(cc.path(key), data, 0600)
}

// evict removes oldest entries when cache exceeds max size.
func (cc *ContextCache) evict() {
	for len(cc.order) > contextCacheMaxSize {
		old := cc.order[0]
		cc.order = cc.order[1:]
		delete(cc.mem, old)
	}
}

// indexOf returns the index of a string in a slice, or -1.
func indexOf(slice []string, s string) int {
	for i, v := range slice {
		if v == s {
			return i
		}
	}
	return -1
}

// APIServer handles HTTP API requests.
type APIServer struct {
	config       *models.Config
	tokenManager *auth.TokenManager
	m365Client   *client.M365Client
	ctxCache     *ContextCache
	server       *http.Server
	stopCh       chan struct{}
	mu           sync.RWMutex
}

// NewAPIServer creates a new API server instance.
func NewAPIServer(config *models.Config, tokenManager *auth.TokenManager) *APIServer {
	return &APIServer{
		config:       config,
		tokenManager: tokenManager,
		ctxCache:     NewContextCache(contextCacheDir),
	}
}

// tokenRefreshInterval is the interval for periodic access token refresh.
const tokenRefreshInterval = 30 * time.Minute

// Start starts the HTTP server on the specified port.
func (api *APIServer) Start(port int) error {
	api.mu.Lock()
	defer api.mu.Unlock()

	// Initialize client
	api.m365Client = client.NewM365Client(api.tokenManager)
	api.stopCh = make(chan struct{})

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", api.withAuth(api.handleChatCompletions))
	mux.HandleFunc("/v1/completions", api.withAuth(api.handleCompletions))
	mux.HandleFunc("/v1/messages", api.withAuth(api.handleAnthropicMessages))
	mux.HandleFunc("/v1/complete", api.withAuth(api.handleAnthropicComplete))
	mux.HandleFunc("/v1/models", api.withAuth(api.handleModels))
	mux.HandleFunc("/health", api.handleHealth)

	api.server = &http.Server{
		Addr:    fmt.Sprintf(":%d", port),
		Handler: mux,
	}

	// Start background token refresher
	go api.runTokenRefresher()

	if len(api.config.APIKeys) > 0 {
		log.Printf("Starting API server on port %d (API key required, %d key(s) configured)", port, len(api.config.APIKeys))
	} else {
		log.Printf("Starting API server on port %d (no API key required)", port)
	}
	return api.server.ListenAndServe()
}

// runTokenRefresher periodically refreshes the access token in the background.
// This prevents the first request after token expiry from blocking 1-2 seconds.
func (api *APIServer) runTokenRefresher() {
	ticker := time.NewTicker(tokenRefreshInterval)
	defer ticker.Stop()

	for {
		select {
		case <-api.stopCh:
			return
		case <-ticker.C:
			if _, err := api.tokenManager.Refresh(); err != nil {
				log.Printf("Background token refresh failed: %v", err)
			} else {
				log.Println("Background token refresh succeeded")
			}
		}
	}
}

// withAuth wraps a handler with API key authentication.
// If no API keys are configured, all requests are allowed (backward compatible).
func (api *APIServer) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions {
			next(w, r)
			return
		}
		if len(api.config.APIKeys) > 0 {
			provided := r.Header.Get("Authorization")
			if provided == "" {
				api.sendError(w, http.StatusUnauthorized, "Missing Authorization header")
				return
			}
			token := strings.TrimSpace(strings.TrimPrefix(provided, "Bearer "))
			if !api.isValidAPIKey(token) {
				api.sendError(w, http.StatusUnauthorized, "Invalid API key")
				return
			}
		}
		next(w, r)
	}
}

// isValidAPIKey checks if the given token matches any configured API key.
func (api *APIServer) isValidAPIKey(token string) bool {
	for _, k := range api.config.APIKeys {
		if token == k {
			return true
		}
	}
	return false
}

// extractAPIKey gets the bearer token from the Authorization header.
// Used as a fallback session ID when no explicit session ID is provided.
func (api *APIServer) extractAPIKey(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if auth == "" {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
}

// Stop stops the HTTP server and background token refresher.
func (api *APIServer) Stop() error {
	api.mu.Lock()
	defer api.mu.Unlock()

	// Signal background token refresher to stop
	if api.stopCh != nil {
		close(api.stopCh)
		api.stopCh = nil
	}

	if api.server != nil {
		return api.server.Close()
	}
	return nil
}

// handleHealth handles health check requests.
func (api *APIServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}

// handleModels handles model list requests.
func (api *APIServer) handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		api.handleCORS(w, r)
		return
	}
	if r.Method != http.MethodGet {
		api.sendError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	modelList := []map[string]interface{}{}
	for _, cfg := range models.ModelRegistry {
		modelList = append(modelList, map[string]interface{}{
			"id":       cfg.OpenAIID,
			"object":   "model",
			"created":  1700000000,
			"owned_by": "microsoft",
		})
	}

	response := map[string]interface{}{
		"object": "list",
		"data":   modelList,
	}

	api.sendJSON(w, http.StatusOK, response)
}

// handleCORS handles CORS preflight requests.
func (api *APIServer) handleCORS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Session-Id")
	w.WriteHeader(http.StatusOK)
}

// getSessionID extracts session ID from headers or request body.
// Priority: X-Session-Id header > session_id body field > user body field > hash(api_key + first_user_message)
func (api *APIServer) getSessionID(r *http.Request, reqBody map[string]interface{}) string {
	sid := r.Header.Get("X-Session-Id")
	if sid == "" {
		if v, ok := reqBody["session_id"].(string); ok {
			sid = v
		}
	}
	if sid == "" {
		if v, ok := reqBody["user"].(string); ok {
			sid = v
		}
	}
	if sid == "" {
		sid = api.hashSessionID(r, reqBody)
	}
	return sid
}

// hashSessionID derives a session ID from the API key and the first user message.
// When auth is enabled, the hash includes the API key so that different keys
// produce different sessions even with the same first message.
// When auth is disabled, only the first user message is hashed.
func (api *APIServer) hashSessionID(r *http.Request, reqBody map[string]interface{}) string {
	firstMsg := extractFirstUserMessage(reqBody)
	if firstMsg == "" {
		return ""
	}
	apiKey := api.extractAPIKey(r)
	h := md5.Sum([]byte(apiKey + "\x00" + firstMsg))
	return "h:" + hex.EncodeToString(h[:])
}

// extractFirstUserMessage scans the messages array and returns the first user message content.
func extractFirstUserMessage(reqBody map[string]interface{}) string {
	msgs, ok := reqBody["messages"].([]interface{})
	if !ok || len(msgs) == 0 {
		return ""
	}
	for _, m := range msgs {
		msg, ok := m.(map[string]interface{})
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		if role != "user" {
			continue
		}
		// Content can be a string or an array of content blocks
		switch c := msg["content"].(type) {
		case string:
			if c != "" {
				return c
			}
		case []interface{}:
			for _, block := range c {
				bm, ok := block.(map[string]interface{})
				if !ok {
					continue
				}
				if t, _ := bm["type"].(string); t == "text" {
					if txt, _ := bm["text"].(string); txt != "" {
						return txt
					}
				}
			}
		}
	}
	return ""
}

// hashSessionIDFromMessages derives a session ID from the API key and the first user message
// in a typed Message slice. Used by handleChatCompletions which decodes into a struct.
func (api *APIServer) hashSessionIDFromMessages(r *http.Request, messages []payload.Message) string {
	firstMsg := ""
	for _, m := range messages {
		if m.Role == "user" && m.Content != "" {
			firstMsg = m.Content
			break
		}
	}
	if firstMsg == "" {
		return ""
	}
	apiKey := api.extractAPIKey(r)
	h := md5.Sum([]byte(apiKey + "\x00" + firstMsg))
	return "h:" + hex.EncodeToString(h[:])
}

// handleChatCompletions handles OpenAI chat completion requests.
func (api *APIServer) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		api.handleCORS(w, r)
		return
	}
	if r.Method != http.MethodPost {
		api.sendError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	var req struct {
		Model          string                   `json:"model"`
		Messages       []payload.Message        `json:"messages"`
		Stream         bool                     `json:"stream"`
		MaxTokens      int                      `json:"max_tokens"`
		ResponseFormat map[string]interface{}   `json:"response_format"`
		SessionID      string                   `json:"session_id"`
		User           string                   `json:"user"`
		Tools          []toolcalling.ToolDef    `json:"tools"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		api.sendError(w, http.StatusBadRequest, fmt.Sprintf("Invalid JSON: %v", err))
		return
	}

	cfg := models.LookupModel(req.Model)
	if cfg.OpenAIID == "" {
		api.sendError(w, http.StatusBadRequest, fmt.Sprintf("Unknown model: %s", req.Model))
		return
	}

	// Handle JSON mode
	if req.ResponseFormat != nil {
		if format, ok := req.ResponseFormat["type"].(string); ok && format == "json_object" {
			api.injectJSONMode(&req.Messages)
		}
	}

	// Inject tool definitions into last user message if tool calling is enabled
	if api.config.ToolCalling && len(req.Tools) > 0 {
		injectToolDefs(&req.Messages, req.Tools)
	}

	// Resolve session ID and conversation ID
	// Priority: request body session_id > request body user > X-Session-Id header > hash(api_key + first_user_message)
	sid := req.SessionID
	if sid == "" {
		sid = req.User
	}
	if sid == "" {
		sid = r.Header.Get("X-Session-Id")
	}
	if sid == "" {
		sid = api.hashSessionIDFromMessages(r, req.Messages)
	}

	var convID string
	if sid != "" {
		convID = api.ctxCache.Get("session:" + sid)
	}

	// Upload any images found in multimodal content and attach annotations
	api.uploadImagesAndAnnotate(&req.Messages, convID)

	// Determine if client-defined tools are present (for optionsSets stripping)
	hasTools := api.config.ToolCalling && len(req.Tools) > 0

	if req.Stream {
		api.streamChatCompletions(w, req.Messages, cfg, sid, convID, req.MaxTokens, hasTools, req.Tools)
	} else {
		api.nonStreamChatCompletions(w, req.Messages, cfg, sid, convID, req.MaxTokens, hasTools, req.Tools)
	}
}

// handleCompletions handles OpenAI text completion requests.
func (api *APIServer) handleCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		api.handleCORS(w, r)
		return
	}
	if r.Method != http.MethodPost {
		api.sendError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	var req struct {
		Model     string `json:"model"`
		Prompt    string `json:"prompt"`
		Suffix    string `json:"suffix"`
		Stream    bool   `json:"stream"`
		MaxTokens int    `json:"max_tokens"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		api.sendError(w, http.StatusBadRequest, fmt.Sprintf("Invalid JSON: %v", err))
		return
	}

	cfg := models.LookupModel(req.Model)
	if cfg.OpenAIID == "" {
		api.sendError(w, http.StatusBadRequest, fmt.Sprintf("Unknown model: %s", req.Model))
		return
	}

	// Convert FIM to chat format
	messages := api.fimToChat(req.Prompt, req.Suffix)

	// Resolve session ID and conversation ID
	sid := api.getSessionID(r, nil)
	var convID string
	if sid != "" {
		convID = api.ctxCache.Get("session:" + sid)
	}

	if req.Stream {
		api.streamCompletions(w, messages, cfg, req.MaxTokens, sid, convID)
	} else {
		api.nonStreamCompletions(w, messages, cfg, req.MaxTokens, sid, convID)
	}
}

// handleAnthropicMessages handles Anthropic messages API requests.
func (api *APIServer) handleAnthropicMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		api.handleCORS(w, r)
		return
	}
	if r.Method != http.MethodPost {
		api.sendError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	var req struct {
		Model       string            `json:"model"`
		Messages    []payload.Message `json:"messages"`
		System      string            `json:"system"`
		MaxTokens   int               `json:"max_tokens"`
		Stream      bool              `json:"stream"`
		Temperature float64           `json:"temperature"`
		Tools       []toolcalling.ToolDef `json:"tools"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		api.sendError(w, http.StatusBadRequest, fmt.Sprintf("Invalid JSON: %v", err))
		return
	}

	// Map Anthropic model to internal model
	cfg := models.LookupModel(req.Model)

	// Build chat messages with system prompt prepended
	chatMessages := []payload.Message{}
	if req.System != "" {
		chatMessages = append(chatMessages, payload.Message{Role: "system", Content: req.System})
	}
	chatMessages = append(chatMessages, req.Messages...)

	// Inject tool definitions into last user message if tool calling is enabled
	if api.config.ToolCalling && len(req.Tools) > 0 {
		injectToolDefs(&chatMessages, req.Tools)
	}

	// Resolve session ID and conversation ID
	sid := api.getSessionID(r, nil)
	var convID string
	if sid != "" {
		convID = api.ctxCache.Get("session:" + sid)
	}

	// Upload any images found in multimodal content and attach annotations
	api.uploadImagesAndAnnotate(&chatMessages, convID)

	// Determine if client-defined tools are present (for optionsSets stripping)
	hasTools := api.config.ToolCalling && len(req.Tools) > 0

	if req.Stream {
		api.streamAnthropicMessages(w, chatMessages, cfg, req.Model, req.MaxTokens, sid, convID, hasTools, req.Tools)
	} else {
		api.nonStreamAnthropicMessages(w, chatMessages, cfg, req.Model, req.MaxTokens, sid, convID, hasTools, req.Tools)
	}
}

// handleAnthropicComplete handles Anthropic complete (FIM) requests.
func (api *APIServer) handleAnthropicComplete(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		api.handleCORS(w, r)
		return
	}
	if r.Method != http.MethodPost {
		api.sendError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	var req struct {
		Model             string   `json:"model"`
		Prompt            string   `json:"prompt"`
		MaxTokensToSample int      `json:"max_tokens_to_sample"`
		Stream            bool     `json:"stream"`
		StopSequences     []string `json:"stop_sequences"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		api.sendError(w, http.StatusBadRequest, fmt.Sprintf("Invalid JSON: %v", err))
		return
	}

	cfg := models.LookupModel(req.Model)

	messages := api.fimToChat(req.Prompt, "")

	// Resolve session ID and conversation ID
	sid := api.getSessionID(r, nil)
	var convID string
	if sid != "" {
		convID = api.ctxCache.Get("session:" + sid)
	}

	respText, _, _, _, err := api.m365Client.ChatConversation(messages, cfg.Tone, cfg.Override, convID, api.config.UserOID, api.config.TenantID, false)
	if err != nil {
		api.sendError(w, http.StatusInternalServerError, fmt.Sprintf("Completion failed: %v", err))
		return
	}

	stopReason := "end_turn"
	for _, s := range req.StopSequences {
		if strings.Contains(respText, s) {
			stopReason = "stop_sequence"
			break
		}
	}

	// Enforce max_tokens_to_sample on response text
	if req.MaxTokensToSample > 0 {
		if truncated, ok := truncateToTokens(respText, req.MaxTokensToSample); ok {
			respText = truncated
			stopReason = "max_tokens"
		}
	}

	response := map[string]interface{}{
		"completion":  respText,
		"stop_reason": stopReason,
		"model":       req.Model,
		"stop":        nil,
		"log_id":      fmt.Sprintf("cmpl_%s", uuid.New().String()),
	}

	api.sendJSON(w, http.StatusOK, response)

	// Cache conversation ID for session continuity
	if sid != "" {
		if convID := api.m365Client.LastConversationID(); convID != "" {
			api.ctxCache.Set("session:"+sid, convID)
		}
	}
}

// streamChatCompletions streams chat completion responses in OpenAI format.
func (api *APIServer) streamChatCompletions(w http.ResponseWriter, messages []payload.Message, cfg models.ModelConfig, sid, convID string, maxTokens int, hasTools bool, tools []toolcalling.ToolDef) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "close")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	flusher, ok := w.(http.Flusher)
	if !ok {
		api.sendError(w, http.StatusInternalServerError, "Streaming not supported")
		return
	}

	chunkID := fmt.Sprintf("chatcmpl-%s", uuid.New().String())
	openaiModel := cfg.OpenAIID

	ch := api.m365Client.ChatConversationStreamGen(messages, cfg.Tone, cfg.Override, convID, api.config.UserOID, api.config.TenantID, hasTools)

	hasContent := false
	fullText := ""
	thinkingText := ""
	truncated := false

	// When tool calling is enabled, buffer all text and parse for tool calls at the end.
	// Tool call blocks may span multiple chunks, so we can't parse incrementally.
	toolCallingEnabled := api.config.ToolCalling

	for chunk := range ch {
		if chunk.Error != nil {
			api.sendSSEError(w, chunkID, openaiModel, chunk.Error)
			return
		}

		if chunk.IsFinal {
			break
		}

		// Send thinking as reasoning_content (OpenAI extended thinking format)
		if chunk.Thinking != "" {
			thinkingText += chunk.Thinking
			if !hasContent {
				api.sendSSEChunk(w, chunkID, openaiModel, map[string]interface{}{
					"role":             "assistant",
					"reasoning_content": chunk.Thinking,
				})
				hasContent = true
			} else {
				api.sendSSEChunk(w, chunkID, openaiModel, map[string]interface{}{
					"reasoning_content": chunk.Thinking,
				})
			}
			flusher.Flush()
			continue
		}

		// Check max_tokens limit before sending more content
		if maxTokens > 0 && countTokens(fullText) >= maxTokens {
			truncated = true
			// Drain remaining chunks
			for range ch {
			}
			break
		}

		fullText += chunk.Text

		// If tool calling is not enabled, stream text directly
		if !toolCallingEnabled {
			if !hasContent {
				api.sendSSEChunk(w, chunkID, openaiModel, map[string]interface{}{
					"role":    "assistant",
					"content": chunk.Text,
				})
				hasContent = true
			} else {
				api.sendSSEChunk(w, chunkID, openaiModel, map[string]interface{}{
					"content": chunk.Text,
				})
			}
			flusher.Flush()
		}
	}

	// Parse simulated tool calls from full text if tool calling is enabled
	var simToolCalls []toolcalling.ToolCall
	if toolCallingEnabled {
		cleanedText, parsedCalls := toolcalling.ParseToolCalls(fullText, tools)
		if len(parsedCalls) > 0 {
			fullText = cleanedText
			simToolCalls = parsedCalls
		}
	}

	// If tool calling buffered text, send it now as a single chunk
	if toolCallingEnabled && fullText != "" && len(simToolCalls) == 0 {
		if !hasContent {
			api.sendSSEChunk(w, chunkID, openaiModel, map[string]interface{}{
				"role":    "assistant",
				"content": fullText,
			})
			hasContent = true
		} else {
			api.sendSSEChunk(w, chunkID, openaiModel, map[string]interface{}{
				"content": fullText,
			})
		}
		flusher.Flush()
	}

	// Send tool calls in stream if any (from M365 backend or simulated)
	api.mu.RLock()
	toolCalls := api.m365Client.LastToolCalls()
	api.mu.RUnlock()

	// Append simulated tool calls
	for _, stc := range simToolCalls {
		toolCalls = append(toolCalls, client.ToolCall{
			ID:       stc.ID,
			Type:     "function",
			Function: client.ToolCallFunction{Name: stc.Name, Arguments: string(stc.Arguments)},
		})
	}

	if len(toolCalls) > 0 {
		if !hasContent {
			api.sendSSEChunk(w, chunkID, openaiModel, map[string]interface{}{
				"role":    "assistant",
				"content": nil,
			})
		}
		for i, tc := range toolCalls {
			api.sendSSEChunk(w, chunkID, openaiModel, map[string]interface{}{
				"tool_calls": []map[string]interface{}{
					{
						"index": i,
						"id":    tc.ID,
						"type":  "function",
						"function": map[string]string{
							"name":      tc.Function.Name,
							"arguments": tc.Function.Arguments,
						},
					},
				},
			})
		}
		flusher.Flush()
	}

	// Send final chunk with usage
	promptStr := fmt.Sprint(messages)
	finishReason := "stop"
	if truncated {
		finishReason = "length"
	}
	if len(toolCalls) > 0 {
		finishReason = "tool_calls"
	}
	promptTok := countTokens(promptStr)
	completionTok := countTokens(fullText)
	reasoningTok := countTokens(thinkingText)
	usage := map[string]interface{}{
		"prompt_tokens":     promptTok,
		"completion_tokens": completionTok,
		"reasoning_tokens":  reasoningTok,
		"total_tokens":      promptTok + completionTok + reasoningTok,
	}

	api.sendSSEDone(w, chunkID, openaiModel, finishReason, usage)
	flusher.Flush()

	// Cache conversation ID for session continuity
	if sid != "" {
		if convID := api.m365Client.LastConversationID(); convID != "" {
			api.ctxCache.Set("session:"+sid, convID)
		}
	}
}

// nonStreamChatCompletions handles non-streaming chat completion in OpenAI format.
func (api *APIServer) nonStreamChatCompletions(w http.ResponseWriter, messages []payload.Message, cfg models.ModelConfig, sid, convID string, maxTokens int, hasTools bool, tools []toolcalling.ToolDef) {
	respText, thinking, toolCalls, finishReason, err := api.m365Client.ChatConversation(messages, cfg.Tone, cfg.Override, convID, api.config.UserOID, api.config.TenantID, hasTools)
	if err != nil {
		api.sendError(w, http.StatusInternalServerError, fmt.Sprintf("Chat failed: %v", err))
		return
	}

	// Parse simulated tool calls from response text if tool calling is enabled
	if api.config.ToolCalling {
		cleanedText, parsedCalls := toolcalling.ParseToolCalls(respText, tools)
		if len(parsedCalls) > 0 {
			respText = cleanedText
			finishReason = "tool_calls"
			for _, pc := range parsedCalls {
				toolCalls = append(toolCalls, client.ToolCall{
					ID:       pc.ID,
					Type:     "function",
					Function: client.ToolCallFunction{Name: pc.Name, Arguments: string(pc.Arguments)},
				})
			}
		} else if hasTools && toolcalling.LooksLikeConfabulation(respText) {
			// Anti-confabulation retry: model claimed it can't access files without
			// calling a tool. Force a retry in the same conversation.
			retryMsg := []payload.Message{
				{Role: "user", Content: "You have not used any tool. Do not claim you cannot access files or that files are empty. Emit a single ```bash block now to inspect the files and run commands. Act, do not explain."},
			}
			retryText, _, retryToolCalls, retryFinish, retryErr := api.m365Client.ChatConversation(retryMsg, cfg.Tone, cfg.Override, convID, api.config.UserOID, api.config.TenantID, hasTools)
			if retryErr == nil {
				retryCleaned, retryParsed := toolcalling.ParseToolCalls(retryText, tools)
				if len(retryParsed) > 0 {
					respText = retryCleaned
					finishReason = "tool_calls"
					for _, pc := range retryParsed {
						toolCalls = append(toolCalls, client.ToolCall{
							ID:       pc.ID,
							Type:     "function",
							Function: client.ToolCallFunction{Name: pc.Name, Arguments: string(pc.Arguments)},
						})
					}
				} else {
					respText = retryText
					finishReason = retryFinish
					toolCalls = retryToolCalls
				}
			}
		}
	}

	// Enforce max_tokens on response text
	if maxTokens > 0 {
		if truncated, ok := truncateToTokens(respText, maxTokens); ok {
			respText = truncated
			finishReason = "length"
		}
	}

	msg := map[string]interface{}{
		"role":    "assistant",
		"content": respText,
	}

	if thinking != "" {
		msg["reasoning_content"] = thinking
	}

	if len(toolCalls) > 0 {
		openaiToolCalls := make([]map[string]interface{}, len(toolCalls))
		for i, tc := range toolCalls {
			openaiToolCalls[i] = map[string]interface{}{
				"index": i,
				"id":    tc.ID,
				"type":  "function",
				"function": map[string]string{
					"name":      tc.Function.Name,
					"arguments": tc.Function.Arguments,
				},
			}
		}
		msg["tool_calls"] = openaiToolCalls
		if respText == "" {
			msg["content"] = nil
		}
	}

	promptStr := fmt.Sprint(messages)
	promptTok := countTokens(promptStr)
	completionTok := countTokens(respText)
	reasoningTok := countTokens(thinking)
	response := map[string]interface{}{
		"id":      fmt.Sprintf("chatcmpl-%s", uuid.New().String()),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   cfg.OpenAIID,
		"choices": []map[string]interface{}{
			{
				"index":         0,
				"message":       msg,
				"finish_reason": finishReason,
			},
		},
		"usage": map[string]interface{}{
			"prompt_tokens":     promptTok,
			"completion_tokens": completionTok,
			"reasoning_tokens":  reasoningTok,
			"total_tokens":      promptTok + completionTok + reasoningTok,
		},
	}

	api.sendJSON(w, http.StatusOK, response)

	// Cache conversation ID for session continuity
	if sid != "" {
		if convID := api.m365Client.LastConversationID(); convID != "" {
			api.ctxCache.Set("session:"+sid, convID)
		}
	}
}

// streamAnthropicMessages streams messages in Anthropic SSE format.
func (api *APIServer) streamAnthropicMessages(w http.ResponseWriter, messages []payload.Message, cfg models.ModelConfig, anthropicModel string, maxTokens int, sid, convID string, hasTools bool, tools []toolcalling.ToolDef) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "close")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	flusher, ok := w.(http.Flusher)
	if !ok {
		api.sendError(w, http.StatusInternalServerError, "Streaming not supported")
		return
	}

	msgID := fmt.Sprintf("msg_%s", uuid.New().String())

	// Send message_start event
	header := map[string]interface{}{
		"type": "message_start",
		"message": map[string]interface{}{
			"id":            msgID,
			"type":          "message",
			"role":          "assistant",
			"content":       []interface{}{},
			"model":         anthropicModel,
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage": map[string]interface{}{
				"input_tokens":  countTokens(fmt.Sprint(messages)),
				"output_tokens": 0,
			},
		},
	}
	api.sendAnthropicSSE(w, "message_start", header)
	flusher.Flush()

	// Stream content with optional thinking block
	fullText := ""
	thinkingText := ""
	truncated := false
	thinkingBlockOpen := false
	textBlockOpen := false
	blockIndex := 0
	toolCallingEnabled := api.config.ToolCalling
	ch := api.m365Client.ChatConversationStreamGen(messages, cfg.Tone, cfg.Override, convID, api.config.UserOID, api.config.TenantID, hasTools)

	for chunk := range ch {
		if chunk.Error != nil {
			errEvent := map[string]interface{}{
				"type": "error",
				"error": map[string]interface{}{
					"type":    "server_error",
					"message": chunk.Error.Error(),
				},
			}
			api.sendAnthropicSSE(w, "error", errEvent)
			flusher.Flush()
			return
		}

		if chunk.IsFinal {
			break
		}

		// Handle thinking content
		if chunk.Thinking != "" {
			thinkingText += chunk.Thinking
			if !thinkingBlockOpen {
				cbStart := map[string]interface{}{
					"type":          "content_block_start",
					"index":         blockIndex,
					"content_block": map[string]interface{}{"type": "thinking", "thinking": ""},
				}
				api.sendAnthropicSSE(w, "content_block_start", cbStart)
				thinkingBlockOpen = true
			}
			delta := map[string]interface{}{
				"type":  "content_block_delta",
				"index": blockIndex,
				"delta": map[string]interface{}{"type": "thinking_delta", "thinking": chunk.Thinking},
			}
			api.sendAnthropicSSE(w, "content_block_delta", delta)
			flusher.Flush()
			continue
		}

		// Transition from thinking to text
		if thinkingBlockOpen && !textBlockOpen {
			api.sendAnthropicSSE(w, "content_block_stop", map[string]interface{}{"type": "content_block_stop", "index": blockIndex})
			blockIndex++
			thinkingBlockOpen = false
		}

		// Open text block on first text chunk (only if not buffering for tool calling)
		if !textBlockOpen && !toolCallingEnabled {
			cbStart := map[string]interface{}{
				"type":          "content_block_start",
				"index":         blockIndex,
				"content_block": map[string]interface{}{"type": "text", "text": ""},
			}
			api.sendAnthropicSSE(w, "content_block_start", cbStart)
			textBlockOpen = true
		}

		// Check max_tokens limit before sending more content
		if maxTokens > 0 && countTokens(fullText) >= maxTokens {
			truncated = true
			for range ch {
			}
			break
		}

		fullText += chunk.Text

		// If tool calling is not enabled, stream text deltas directly
		if !toolCallingEnabled {
			delta := map[string]interface{}{
				"type":  "content_block_delta",
				"index": blockIndex,
				"delta": map[string]interface{}{"type": "text_delta", "text": chunk.Text},
			}
			api.sendAnthropicSSE(w, "content_block_delta", delta)
			flusher.Flush()
		}
	}

	// Parse simulated tool calls from full text if tool calling is enabled
	var simToolCalls []toolcalling.ToolCall
	if toolCallingEnabled {
		cleanedText, parsedCalls := toolcalling.ParseToolCalls(fullText, tools)
		if len(parsedCalls) > 0 {
			fullText = cleanedText
			simToolCalls = parsedCalls
		}
	}

	// If tool calling buffered text, send it now as a text block
	if toolCallingEnabled && fullText != "" {
		cbStart := map[string]interface{}{
			"type":          "content_block_start",
			"index":         blockIndex,
			"content_block": map[string]interface{}{"type": "text", "text": ""},
		}
		api.sendAnthropicSSE(w, "content_block_start", cbStart)
		textBlockOpen = true
		delta := map[string]interface{}{
			"type":  "content_block_delta",
			"index": blockIndex,
			"delta": map[string]interface{}{"type": "text_delta", "text": fullText},
		}
		api.sendAnthropicSSE(w, "content_block_delta", delta)
		flusher.Flush()
	}

	// Close any open blocks
	if thinkingBlockOpen {
		api.sendAnthropicSSE(w, "content_block_stop", map[string]interface{}{"type": "content_block_stop", "index": blockIndex})
		blockIndex++
	}
	if textBlockOpen {
		api.sendAnthropicSSE(w, "content_block_stop", map[string]interface{}{"type": "content_block_stop", "index": blockIndex})
		blockIndex++
	}

	// Send tool_use content blocks if any (server-side tools from M365 backend or simulated)
	api.mu.RLock()
	toolCalls := api.m365Client.LastToolCalls()
	api.mu.RUnlock()

	// Append simulated tool calls
	for _, stc := range simToolCalls {
		toolCalls = append(toolCalls, client.ToolCall{
			ID:       stc.ID,
			Type:     "function",
			Function: client.ToolCallFunction{Name: stc.Name, Arguments: string(stc.Arguments)},
		})
	}

	for _, tc := range toolCalls {
		var input interface{}
		json.Unmarshal([]byte(tc.Function.Arguments), &input)
		if input == nil {
			input = map[string]interface{}{}
		}
		api.sendAnthropicSSE(w, "content_block_start", map[string]interface{}{
			"type":  "content_block_start",
			"index": blockIndex,
			"content_block": map[string]interface{}{
				"type":  "tool_use",
				"id":    tc.ID,
				"name":  tc.Function.Name,
				"input": input,
			},
		})
		api.sendAnthropicSSE(w, "content_block_stop", map[string]interface{}{
			"type":  "content_block_stop",
			"index": blockIndex,
		})
		blockIndex++
	}
	flusher.Flush()

	// Send message_delta event
	stopReason := "end_turn"
	if truncated {
		stopReason = "max_tokens"
	}
	if len(toolCalls) > 0 {
		stopReason = "tool_use"
	}
	msgDelta := map[string]interface{}{
		"type": "message_delta",
		"delta": map[string]interface{}{
			"stop_reason":   stopReason,
			"stop_sequence": nil,
		},
		"usage": map[string]interface{}{
			"output_tokens":    countTokens(fullText),
			"reasoning_tokens": countTokens(thinkingText),
		},
	}
	api.sendAnthropicSSE(w, "message_delta", msgDelta)
	flusher.Flush()

	// Send message_stop event
	msgStop := map[string]interface{}{"type": "message_stop"}
	api.sendAnthropicSSE(w, "message_stop", msgStop)
	flusher.Flush()

	// Cache conversation ID for session continuity
	if sid != "" {
		if convID := api.m365Client.LastConversationID(); convID != "" {
			api.ctxCache.Set("session:"+sid, convID)
		}
	}
}

// nonStreamAnthropicMessages handles non-streaming Anthropic messages response.
func (api *APIServer) nonStreamAnthropicMessages(w http.ResponseWriter, messages []payload.Message, cfg models.ModelConfig, anthropicModel string, maxTokens int, sid, convID string, hasTools bool, tools []toolcalling.ToolDef) {
	respText, thinking, toolCalls, finishReason, err := api.m365Client.ChatConversation(messages, cfg.Tone, cfg.Override, convID, api.config.UserOID, api.config.TenantID, hasTools)
	if err != nil {
		api.sendError(w, http.StatusInternalServerError, fmt.Sprintf("Chat failed: %v", err))
		return
	}

	// Parse simulated tool calls from response text if tool calling is enabled
	if api.config.ToolCalling {
		cleanedText, parsedCalls := toolcalling.ParseToolCalls(respText, tools)
		if len(parsedCalls) > 0 {
			respText = cleanedText
			finishReason = "tool_calls"
			for _, pc := range parsedCalls {
				toolCalls = append(toolCalls, client.ToolCall{
					ID:       pc.ID,
					Type:     "function",
					Function: client.ToolCallFunction{Name: pc.Name, Arguments: string(pc.Arguments)},
				})
			}
		} else if hasTools && toolcalling.LooksLikeConfabulation(respText) {
			// Anti-confabulation retry: model claimed it can't access files without
			// calling a tool. Force a retry in the same conversation.
			retryMsg := []payload.Message{
				{Role: "user", Content: "You have not used any tool. Do not claim you cannot access files or that files are empty. Emit a single ```bash block now to inspect the files and run commands. Act, do not explain."},
			}
			retryText, _, retryToolCalls, retryFinish, retryErr := api.m365Client.ChatConversation(retryMsg, cfg.Tone, cfg.Override, convID, api.config.UserOID, api.config.TenantID, hasTools)
			if retryErr == nil {
				retryCleaned, retryParsed := toolcalling.ParseToolCalls(retryText, tools)
				if len(retryParsed) > 0 {
					respText = retryCleaned
					finishReason = "tool_calls"
					for _, pc := range retryParsed {
						toolCalls = append(toolCalls, client.ToolCall{
							ID:       pc.ID,
							Type:     "function",
							Function: client.ToolCallFunction{Name: pc.Name, Arguments: string(pc.Arguments)},
						})
					}
				} else {
					respText = retryText
					finishReason = retryFinish
					toolCalls = retryToolCalls
				}
			}
		}
	}

	stopReason := "end_turn"
	if finishReason == "tool_calls" {
		stopReason = "tool_use"
	}

	// Enforce max_tokens on response text
	if maxTokens > 0 {
		if truncated, ok := truncateToTokens(respText, maxTokens); ok {
			respText = truncated
			stopReason = "max_tokens"
		}
	}

	content := []map[string]interface{}{}
	if thinking != "" {
		content = append(content, map[string]interface{}{"type": "thinking", "thinking": thinking})
	}
	if respText != "" {
		content = append(content, map[string]interface{}{"type": "text", "text": respText})
	}

	if len(toolCalls) > 0 {
		for _, tc := range toolCalls {
			var input interface{}
			json.Unmarshal([]byte(tc.Function.Arguments), &input)
			if input == nil {
				input = map[string]interface{}{}
			}
			content = append(content, map[string]interface{}{
				"type":  "tool_use",
				"id":    tc.ID,
				"name":  tc.Function.Name,
				"input": input,
			})
		}
	}

	response := map[string]interface{}{
		"id":            fmt.Sprintf("msg_%s", uuid.New().String()),
		"type":          "message",
		"role":          "assistant",
		"content":       content,
		"model":         anthropicModel,
		"stop_reason":   stopReason,
		"stop_sequence": nil,
		"usage": map[string]interface{}{
			"input_tokens":     countTokens(fmt.Sprint(messages)),
			"output_tokens":    countTokens(respText),
			"reasoning_tokens": countTokens(thinking),
		},
	}

	api.sendJSON(w, http.StatusOK, response)

	// Cache conversation ID for session continuity
	if sid != "" {
		if convID := api.m365Client.LastConversationID(); convID != "" {
			api.ctxCache.Set("session:"+sid, convID)
		}
	}
}

// streamCompletions streams text completion responses in OpenAI text_completion format.
func (api *APIServer) streamCompletions(w http.ResponseWriter, messages []payload.Message, cfg models.ModelConfig, maxTokens int, sid, convID string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "close")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	flusher, ok := w.(http.Flusher)
	if !ok {
		api.sendError(w, http.StatusInternalServerError, "Streaming not supported")
		return
	}

	compID := fmt.Sprintf("cmpl-%s", uuid.New().String())
	openaiModel := cfg.OpenAIID

	ch := api.m365Client.ChatConversationStreamGen(messages, cfg.Tone, cfg.Override, convID, api.config.UserOID, api.config.TenantID, false)

	fullText := ""
	thinkingText := ""
	truncated := false

	for chunk := range ch {
		if chunk.Error != nil {
			errChunk := map[string]interface{}{
				"id":      compID,
				"object":  "text_completion",
				"created": time.Now().Unix(),
				"model":   openaiModel,
				"choices": []map[string]interface{}{
					{
						"index":         0,
						"text":          fmt.Sprintf("Error: %v", chunk.Error),
						"finish_reason": "stop",
						"logprobs":      nil,
					},
				},
			}
			jsonData, _ := json.Marshal(errChunk)
			fmt.Fprintf(w, "data: %s\n\n", jsonData)
			fmt.Fprintf(w, "data: [DONE]\n\n")
			flusher.Flush()
			return
		}

		if chunk.IsFinal {
			break
		}

		// Accumulate thinking text (not sent as content for text_completion)
		if chunk.Thinking != "" {
			thinkingText += chunk.Thinking
			continue
		}

		// Check max_tokens limit before sending more content
		if maxTokens > 0 && countTokens(fullText) >= maxTokens {
			truncated = true
			for range ch {
			}
			break
		}

		fullText += chunk.Text

		chunkData := map[string]interface{}{
			"id":      compID,
			"object":  "text_completion",
			"created": time.Now().Unix(),
			"model":   openaiModel,
			"choices": []map[string]interface{}{
				{
					"index":         0,
					"text":          chunk.Text,
					"finish_reason": nil,
					"logprobs":      nil,
				},
			},
		}

		jsonData, _ := json.Marshal(chunkData)
		fmt.Fprintf(w, "data: %s\n\n", jsonData)
		flusher.Flush()
	}

	// Send final done chunk
	finishReason := "stop"
	if truncated {
		finishReason = "length"
	}
	doneChunk := map[string]interface{}{
		"id":      compID,
		"object":  "text_completion",
		"created": time.Now().Unix(),
		"model":   openaiModel,
		"choices": []map[string]interface{}{
			{
				"index":         0,
				"text":          "",
				"finish_reason": finishReason,
				"logprobs":      nil,
			},
		},
	}
	jsonData, _ := json.Marshal(doneChunk)
	fmt.Fprintf(w, "data: %s\n\n", jsonData)
	fmt.Fprintf(w, "data: [DONE]\n\n")
	flusher.Flush()

	// Cache conversation ID for session continuity
	if sid != "" {
		if convID := api.m365Client.LastConversationID(); convID != "" {
			api.ctxCache.Set("session:"+sid, convID)
		}
	}
}

// nonStreamCompletions handles non-streaming text completion.
func (api *APIServer) nonStreamCompletions(w http.ResponseWriter, messages []payload.Message, cfg models.ModelConfig, maxTokens int, sid, convID string) {
	respText, thinking, _, _, err := api.m365Client.ChatConversation(messages, cfg.Tone, cfg.Override, convID, api.config.UserOID, api.config.TenantID, false)
	if err != nil {
		api.sendError(w, http.StatusInternalServerError, fmt.Sprintf("Completion failed: %v", err))
		return
	}

	// Enforce max_tokens on response text
	finishReason := "stop"
	if maxTokens > 0 {
		if truncated, ok := truncateToTokens(respText, maxTokens); ok {
			respText = truncated
			finishReason = "length"
		}
	}

	promptStr := fmt.Sprint(messages)
	promptTok := countTokens(promptStr)
	completionTok := countTokens(respText)
	reasoningTok := countTokens(thinking)
	response := map[string]interface{}{
		"id":      fmt.Sprintf("cmpl-%s", uuid.New().String()),
		"object":  "text_completion",
		"created": time.Now().Unix(),
		"model":   cfg.OpenAIID,
		"choices": []map[string]interface{}{
			{
				"index":         0,
				"text":          respText,
				"finish_reason": finishReason,
				"logprobs":      nil,
			},
		},
		"usage": map[string]interface{}{
			"prompt_tokens":     promptTok,
			"completion_tokens": completionTok,
			"reasoning_tokens":  reasoningTok,
			"total_tokens":      promptTok + completionTok + reasoningTok,
		},
	}

	api.sendJSON(w, http.StatusOK, response)

	// Cache conversation ID for session continuity
	if sid != "" {
		if convID := api.m365Client.LastConversationID(); convID != "" {
			api.ctxCache.Set("session:"+sid, convID)
		}
	}
}

// sendJSON sends a JSON response.
func (api *APIServer) sendJSON(w http.ResponseWriter, statusCode int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(statusCode)

	json.NewEncoder(w).Encode(data)
}

// sendError sends an error response.
func (api *APIServer) sendError(w http.ResponseWriter, statusCode int, message string) {
	api.sendJSON(w, statusCode, map[string]interface{}{
		"error": map[string]interface{}{
			"message": message,
			"type":    "error",
			"code":    statusCode,
		},
	})
}

// sendSSEChunk sends a Server-Sent Events chunk in OpenAI chat.completion.chunk format.
func (api *APIServer) sendSSEChunk(w http.ResponseWriter, chunkID, model string, data map[string]interface{}) {
	chunk := map[string]interface{}{
		"id":      chunkID,
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]interface{}{
			{
				"index":         0,
				"delta":         data,
				"finish_reason": nil,
			},
		},
	}

	jsonData, _ := json.Marshal(chunk)
	fmt.Fprintf(w, "data: %s\n\n", jsonData)
}

// sendSSEDone sends the final SSE chunk.
func (api *APIServer) sendSSEDone(w http.ResponseWriter, chunkID, model, finishReason string, usage map[string]interface{}) {
	if finishReason == "" {
		finishReason = "stop"
	}
	chunk := map[string]interface{}{
		"id":      chunkID,
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]interface{}{
			{
				"index":         0,
				"delta":         map[string]interface{}{},
				"finish_reason": finishReason,
			},
		},
	}

	if usage != nil {
		chunk["usage"] = usage
	}

	jsonData, _ := json.Marshal(chunk)
	fmt.Fprintf(w, "data: %s\n\n", jsonData)
	fmt.Fprintf(w, "data: [DONE]\n\n")
}

// sendSSEError sends an error via SSE.
func (api *APIServer) sendSSEError(w http.ResponseWriter, chunkID, model string, err error) {
	chunk := map[string]interface{}{
		"id":      chunkID,
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]interface{}{
			{
				"index":         0,
				"delta":         map[string]interface{}{"content": fmt.Sprintf("Error: %v", err)},
				"finish_reason": "stop",
			},
		},
	}

	jsonData, _ := json.Marshal(chunk)
	fmt.Fprintf(w, "data: %s\n\n", jsonData)
	fmt.Fprintf(w, "data: [DONE]\n\n")
}

// sendAnthropicSSE sends an Anthropic-format SSE event.
func (api *APIServer) sendAnthropicSSE(w http.ResponseWriter, eventType string, data map[string]interface{}) {
	jsonData, _ := json.Marshal(data)
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, jsonData)
}

// uploadImagesAndAnnotate uploads any images found in message Images fields
// to the M365 backend and attaches the resulting docId annotations to the
// last message with images. This enables multimodal image input support.
func (api *APIServer) uploadImagesAndAnnotate(messages *[]payload.Message, convID string) {
	// Find the last message with images
	lastImgIdx := -1
	for i := len(*messages) - 1; i >= 0; i-- {
		if len((*messages)[i].Images) > 0 {
			lastImgIdx = i
			break
		}
	}
	if lastImgIdx < 0 {
		return
	}

	// Use existing convID or generate a temporary UUID for upload
	uploadConvID := convID
	if uploadConvID == "" {
		uploadConvID = uuid.New().String()
	}

	msg := &(*messages)[lastImgIdx]
	for _, img := range msg.Images {
		result, err := api.m365Client.UploadFile(img.Base64, img.MediaType, img.FileName, uploadConvID, api.config.UserOID, api.config.TenantID)
		if err != nil {
			log.Printf("Image upload failed: %v", err)
			continue
		}
		if !result.IsSuccess {
			log.Printf("Image upload returned non-success: %+v", result)
			continue
		}

		fileType := strings.TrimPrefix(result.FileType, ".")
		msg.Annotations = append(msg.Annotations, payload.MessageAnnotation{
			ID:                    result.DocID,
			MessageAnnotationType: "ImageFile",
			MessageAnnotationMetadata: map[string]string{
				"@type":          "File",
				"annotationType": "File",
				"fileType":       fileType,
				"fileName":       img.FileName,
			},
		})
	}
}

// injectJSONMode injects JSON mode instructions into messages.
func (api *APIServer) injectJSONMode(messages *[]payload.Message) {
	instruction := "You MUST respond with valid JSON only. Do not include markdown code blocks, explanation, or any text outside the JSON object."

	for i, msg := range *messages {
		if msg.Role == "system" {
			(*messages)[i].Content = msg.Content + "\n" + instruction
			return
		}
	}

	*messages = append([]payload.Message{{Role: "system", Content: instruction}}, *messages...)
}

// injectToolDefs prepends tool definitions and instructions to the last user message.
func injectToolDefs(messages *[]payload.Message, tools []toolcalling.ToolDef) {
	if len(tools) == 0 || len(*messages) == 0 {
		return
	}

	// Build tool instruction text
	msgTexts := make([]string, len(*messages))
	for i, msg := range *messages {
		msgTexts[i] = msg.Content
	}
	injected := toolcalling.InjectTools(msgTexts, tools)

	for i := len(*messages) - 1; i >= 0; i-- {
		if (*messages)[i].Role == "user" {
			(*messages)[i].Content = injected[i]
			break
		}
	}
}

// fimToChat converts FIM (fill-in-the-middle) prompts to chat format.
func (api *APIServer) fimToChat(prompt, suffix string) []payload.Message {
	if suffix != "" {
		return []payload.Message{
			{
				Role: "user",
				Content: fmt.Sprintf("Complete the middle of the following text naturally.\n\n--- BEGIN TEXT ---\n%s\n--- MIDDLE ---\n%s\n--- END ---\n\nWrite only the middle part that connects the two sections.", prompt, suffix),
			},
		}
	}

	return []payload.Message{
		{
			Role:    "user",
			Content: fmt.Sprintf("Continue writing from this point:\n\n%s", prompt),
		},
	}
}

// tokenEncoder is the tiktoken encoder for cl100k_base (GPT-4/5 family).
var tokenEncoder *tiktoken.Tiktoken

func init() {
	enc, err := tiktoken.EncodingForModel("gpt-4")
	if err != nil {
		enc, err = tiktoken.GetEncoding("cl100k_base")
		if err != nil {
			log.Printf("Failed to init tiktoken encoder, falling back to space split: %v", err)
		}
	}
	tokenEncoder = enc
}

// countTokens returns the real BPE token count using tiktoken.
func countTokens(text string) int {
	if strings.TrimSpace(text) == "" {
		return 0
	}
	if tokenEncoder != nil {
		return len(tokenEncoder.Encode(text, nil, nil))
	}
	return len(strings.Split(text, " "))
}

// truncateToTokens truncates text to at most maxTokens tokens using tiktoken.
// Returns the truncated text and true if truncation occurred.
func truncateToTokens(text string, maxTokens int) (string, bool) {
	if maxTokens <= 0 {
		return text, false
	}
	if tokenEncoder != nil {
		tokens := tokenEncoder.Encode(text, nil, nil)
		if len(tokens) <= maxTokens {
			return text, false
		}
		return tokenEncoder.Decode(tokens[:maxTokens]), true
	}
	words := strings.Split(text, " ")
	if len(words) <= maxTokens {
		return text, false
	}
	return strings.Join(words[:maxTokens], " "), true
}
