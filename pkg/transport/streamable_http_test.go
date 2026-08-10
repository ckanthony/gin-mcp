package transport

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dolfly/gin-mcp/pkg/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Test Setup ---

func setupTestStreamableHTTPTransport(mountPath string, allowedOrigins []string) *StreamableHTTPTransport {
	setGinModeOnce.Do(func() {}) // Already called by sse_test.go if tests run together; safe to call again via Once.
	return NewStreamableHTTPTransport(mountPath, allowedOrigins)
}

// --- Constructor & RegisterHandler ---

func TestNewStreamableHTTPTransport(t *testing.T) {
	mountPath := "/mcp"
	s := NewStreamableHTTPTransport(mountPath, nil)

	assert.NotNil(t, s)
	assert.Equal(t, mountPath, s.mountPath)
	assert.NotNil(t, s.handlers)
	assert.Empty(t, s.handlers)
	assert.NotNil(t, s.requestAuths)
	assert.Empty(t, s.requestAuths)
	assert.NotNil(t, s.sessions)
	assert.Empty(t, s.sessions)
	assert.Empty(t, s.allowedOrigins)
}

func TestNewStreamableHTTPTransport_WithAllowedOrigins(t *testing.T) {
	origins := []string{"https://app.example.com", "https://other.example.com"}
	s := NewStreamableHTTPTransport("/mcp", origins)

	assert.NotNil(t, s)
	assert.Equal(t, origins, s.allowedOrigins)
}

func TestStreamableHTTPTransport_RegisterHandler(t *testing.T) {
	s := setupTestStreamableHTTPTransport("/mcp", nil)
	method := "test/method"
	handler := func(msg *types.MCPMessage) *types.MCPMessage {
		return &types.MCPMessage{Result: "ok"}
	}

	s.RegisterHandler(method, handler)

	s.hMu.RLock()
	registeredHandler, exists := s.handlers[method]
	s.hMu.RUnlock()

	assert.True(t, exists, "Handler should be registered")
	assert.NotNil(t, registeredHandler)

	// Test overwriting a handler
	newHandler := func(msg *types.MCPMessage) *types.MCPMessage {
		return &types.MCPMessage{Result: "new ok"}
	}
	s.RegisterHandler(method, newHandler)
	s.hMu.RLock()
	overwrittenHandler, _ := s.handlers[method]
	s.hMu.RUnlock()
	resp := overwrittenHandler(&types.MCPMessage{})
	assert.Equal(t, "new ok", resp.Result)
}

// --- Origin helpers ---

func TestStreamableHTTPTransport_IsOriginAllowed(t *testing.T) {
	tests := []struct {
		name           string
		allowedOrigins []string
		requestOrigin  string
		want           bool
	}{
		{"empty origin always allowed", []string{"https://allowed.example.com"}, "", true},
		{"no allowlist permits all origins", nil, "https://any.example.com", true},
		{"empty allowlist permits all origins", []string{}, "https://any.example.com", true},
		{"origin in allowlist", []string{"https://allowed.example.com"}, "https://allowed.example.com", true},
		{"origin not in allowlist", []string{"https://allowed.example.com"}, "https://evil.example.com", false},
		{"one of many origins matches", []string{"https://a.com", "https://b.com"}, "https://b.com", true},
		{"no match in multi-origin list", []string{"https://a.com", "https://b.com"}, "https://c.com", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := NewStreamableHTTPTransport("/mcp", tc.allowedOrigins)
			got := s.isOriginAllowed(tc.requestOrigin)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestStreamableHTTPTransport_CorsOriginHeader(t *testing.T) {
	tests := []struct {
		name           string
		allowedOrigins []string
		requestOrigin  string
		want           string
	}{
		{"no allowlist, no origin → wildcard", nil, "", "*"},
		{"no allowlist, origin present → wildcard", nil, "https://app.example.com", "*"},
		{"allowlist configured, origin present → echo origin", []string{"https://app.example.com"}, "https://app.example.com", "https://app.example.com"},
		{"allowlist configured, no origin → wildcard", []string{"https://app.example.com"}, "", "*"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := NewStreamableHTTPTransport("/mcp", tc.allowedOrigins)
			got := s.corsOriginHeader(tc.requestOrigin)
			assert.Equal(t, tc.want, got)
		})
	}
}

// --- isNotification helper ---

func TestIsNotification(t *testing.T) {
	assert.True(t, isNotification(&types.MCPMessage{ID: nil}), "nil ID should be a notification")
	assert.True(t, isNotification(&types.MCPMessage{ID: types.RawMessage(`null`)}), "null ID should be a notification")
	assert.False(t, isNotification(&types.MCPMessage{ID: types.RawMessage(`"1"`)}), "string ID should not be a notification")
	assert.False(t, isNotification(&types.MCPMessage{ID: types.RawMessage(`1`)}), "numeric ID should not be a notification")
}

// --- HandleMessage ---

func TestStreamableHTTPTransport_HandleMessage_Success(t *testing.T) {
	s := setupTestStreamableHTTPTransport("/mcp", nil)
	method := "test/success"
	handlerCalled := false
	s.RegisterHandler(method, func(msg *types.MCPMessage) *types.MCPMessage {
		handlerCalled = true
		assert.Equal(t, method, msg.Method)
		assert.Equal(t, types.RawMessage(`"req-id-1"`), msg.ID)
		return &types.MCPMessage{Jsonrpc: "2.0", ID: msg.ID, Result: "handler success"}
	})

	reqBody := `{"jsonrpc":"2.0","id":"req-id-1","method":"test/success","params":{}}`
	c, w, _ := setupTestGinContext("POST", "/mcp", bytes.NewBufferString(reqBody), nil)
	c.Request.Header.Set("Content-Type", "application/json")

	s.HandleMessage(c)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.True(t, handlerCalled, "Registered handler should have been called")
	assert.Contains(t, w.Body.String(), `"result":"handler success"`)
	assert.Contains(t, w.Body.String(), `"id":"req-id-1"`)
}

func TestStreamableHTTPTransport_HandleMessage_Notification(t *testing.T) {
	// JSON-RPC messages with no ID (notifications) must return 202 Accepted with no body.
	s := setupTestStreamableHTTPTransport("/mcp", nil)
	handlerCalled := false
	s.RegisterHandler("notifications/cancelled", func(msg *types.MCPMessage) *types.MCPMessage {
		handlerCalled = true
		return nil
	})

	reqBody := `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{}}`
	c, w, _ := setupTestGinContext("POST", "/mcp", bytes.NewBufferString(reqBody), nil)
	c.Request.Header.Set("Content-Type", "application/json")

	s.HandleMessage(c)

	// c.Status() sets gin's internal status but does not flush to the ResponseRecorder
	// when the handler is invoked directly (bypassing gin's ServeHTTP). In production,
	// ServeHTTP calls WriteHeaderNow() after the handler returns, which propagates the
	// status. We therefore check c.Writer.Status() (the gin-level status) rather than
	// w.Code (the recorder-level status, which stays at 200 until flushed).
	assert.Equal(t, http.StatusAccepted, c.Writer.Status())
	assert.Empty(t, w.Body.String(), "202 response must have no body")
	assert.False(t, handlerCalled, "Handler should not be called for notifications")
}

func TestStreamableHTTPTransport_HandleMessage_NullID(t *testing.T) {
	// Explicit null id also counts as a notification.
	s := setupTestStreamableHTTPTransport("/mcp", nil)

	reqBody := `{"jsonrpc":"2.0","id":null,"method":"some/notification"}`
	c, w, _ := setupTestGinContext("POST", "/mcp", bytes.NewBufferString(reqBody), nil)
	c.Request.Header.Set("Content-Type", "application/json")

	s.HandleMessage(c)

	// See comment in TestStreamableHTTPTransport_HandleMessage_Notification about why
	// we check c.Writer.Status() rather than w.Code here.
	assert.Equal(t, http.StatusAccepted, c.Writer.Status())
	assert.Empty(t, w.Body.String(), "null-id message must have no body")
}

func TestStreamableHTTPTransport_HandleMessage_BadRequestBody(t *testing.T) {
	s := setupTestStreamableHTTPTransport("/mcp", nil)

	reqBody := `{"jsonrpc":"2.0",,"id":"invalid"}` // Invalid JSON
	c, w, _ := setupTestGinContext("POST", "/mcp", bytes.NewBufferString(reqBody), nil)
	c.Request.Header.Set("Content-Type", "application/json")

	s.HandleMessage(c)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "Invalid message format")
}

func TestStreamableHTTPTransport_HandleMessage_HandlerNotFound(t *testing.T) {
	// Unlike SSE, the error is returned directly in the HTTP body (200 OK with JSON-RPC error).
	s := setupTestStreamableHTTPTransport("/mcp", nil)

	reqBody := `{"jsonrpc":"2.0","id":"req-id-nf","method":"unregistered/method","params":{}}`
	c, w, _ := setupTestGinContext("POST", "/mcp", bytes.NewBufferString(reqBody), nil)
	c.Request.Header.Set("Content-Type", "application/json")

	s.HandleMessage(c)

	assert.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, `"id":"req-id-nf"`)
	assert.Contains(t, body, `"code":-32601`)
	assert.Contains(t, body, "not found")
}

func TestStreamableHTTPTransport_HandleMessage_DisallowedOrigin(t *testing.T) {
	s := setupTestStreamableHTTPTransport("/mcp", []string{"https://allowed.example.com"})

	reqBody := `{"jsonrpc":"2.0","id":"1","method":"test/method","params":{}}`
	c, w, _ := setupTestGinContext("POST", "/mcp", bytes.NewBufferString(reqBody), nil)
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.Header.Set("Origin", "https://evil.example.com")

	s.HandleMessage(c)

	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Contains(t, w.Body.String(), "Origin not allowed")
}

func TestStreamableHTTPTransport_HandleMessage_AllowedOriginPermitted(t *testing.T) {
	allowedOrigin := "https://app.example.com"
	s := setupTestStreamableHTTPTransport("/mcp", []string{allowedOrigin})
	s.RegisterHandler("test/method", func(msg *types.MCPMessage) *types.MCPMessage {
		return &types.MCPMessage{Jsonrpc: "2.0", ID: msg.ID, Result: "ok"}
	})

	reqBody := `{"jsonrpc":"2.0","id":"1","method":"test/method","params":{}}`
	c, w, _ := setupTestGinContext("POST", "/mcp", bytes.NewBufferString(reqBody), nil)
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.Header.Set("Origin", allowedOrigin)

	s.HandleMessage(c)

	assert.Equal(t, http.StatusOK, w.Code)
}

func TestStreamableHTTPTransport_HandleMessage_NoOriginAlwaysPermitted(t *testing.T) {
	// Server-to-server calls without Origin header must always be allowed.
	s := setupTestStreamableHTTPTransport("/mcp", []string{"https://allowed.example.com"})
	s.RegisterHandler("test/method", func(msg *types.MCPMessage) *types.MCPMessage {
		return &types.MCPMessage{Jsonrpc: "2.0", ID: msg.ID, Result: "ok"}
	})

	reqBody := `{"jsonrpc":"2.0","id":"1","method":"test/method","params":{}}`
	c, w, _ := setupTestGinContext("POST", "/mcp", bytes.NewBufferString(reqBody), nil)
	c.Request.Header.Set("Content-Type", "application/json")
	// No Origin header set — simulates a server-to-server call.

	s.HandleMessage(c)

	assert.Equal(t, http.StatusOK, w.Code)
}

// --- Auth header forwarding ---

func TestStreamableHTTPTransport_GetAuthHeader(t *testing.T) {
	s := setupTestStreamableHTTPTransport("/mcp", nil)
	authValue := "Bearer test-token-xyz"

	var capturedRequestID string
	s.RegisterHandler("test/auth", func(msg *types.MCPMessage) *types.MCPMessage {
		paramsMap, ok := msg.Params.(map[string]interface{})
		require.True(t, ok, "Params should be a map")
		id, ok := paramsMap["_mcpConnectionID"].(string)
		require.True(t, ok, "_mcpConnectionID should be a string")
		capturedRequestID = id
		// While the handler is running, the auth header must be retrievable.
		assert.Equal(t, authValue, s.GetAuthHeader(id), "Auth header should be retrievable inside handler")
		return &types.MCPMessage{Jsonrpc: "2.0", ID: msg.ID, Result: "ok"}
	})

	reqBody := `{"jsonrpc":"2.0","id":"1","method":"test/auth","params":{}}`
	c, w, _ := setupTestGinContext("POST", "/mcp", bytes.NewBufferString(reqBody), nil)
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.Header.Set("Authorization", authValue)

	s.HandleMessage(c)

	assert.Equal(t, http.StatusOK, w.Code)
	require.NotEmpty(t, capturedRequestID, "Handler should have captured a request ID")

	// After the request completes, the entry must be cleaned up (defer in HandleMessage).
	assert.Empty(t, s.GetAuthHeader(capturedRequestID), "Auth header should be removed after request completes")
}

func TestStreamableHTTPTransport_GetHeader(t *testing.T) {
	s := setupTestStreamableHTTPTransport("/mcp", nil)
	authValue := "Bearer test-token-xyz"
	traceValue := "trace-123"

	var capturedRequestID string
	s.RegisterHandler("test/header", func(msg *types.MCPMessage) *types.MCPMessage {
		paramsMap, ok := msg.Params.(map[string]interface{})
		require.True(t, ok, "Params should be a map")
		id, ok := paramsMap["_mcpConnectionID"].(string)
		require.True(t, ok, "_mcpConnectionID should be a string")
		capturedRequestID = id
		assert.Equal(t, authValue, s.GetHeader(id, "Authorization"), "Authorization should be retrievable inside handler")
		assert.Equal(t, traceValue, s.GetHeader(id, "X-Trace-Id"), "Custom header should be retrievable inside handler")
		return &types.MCPMessage{Jsonrpc: "2.0", ID: msg.ID, Result: "ok"}
	})

	reqBody := `{"jsonrpc":"2.0","id":"1","method":"test/header","params":{}}`
	c, w, _ := setupTestGinContext("POST", "/mcp", bytes.NewBufferString(reqBody), nil)
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.Header.Set("Authorization", authValue)
	c.Request.Header.Set("X-Trace-Id", traceValue)

	s.HandleMessage(c)

	assert.Equal(t, http.StatusOK, w.Code)
	require.NotEmpty(t, capturedRequestID, "Handler should have captured a request ID")
	assert.Empty(t, s.GetHeader(capturedRequestID, "Authorization"), "Authorization should be removed after request completes")
	assert.Empty(t, s.GetHeader(capturedRequestID, "X-Trace-Id"), "Custom headers should be removed after request completes")
}

func TestStreamableHTTPTransport_GetAuthHeader_MissingToken(t *testing.T) {
	s := setupTestStreamableHTTPTransport("/mcp", nil)

	// Unknown request ID returns empty string.
	assert.Empty(t, s.GetAuthHeader("non-existent-uuid"))
}

func TestStreamableHTTPTransport_MCPConnectionID_InjectedIntoParams(t *testing.T) {
	s := setupTestStreamableHTTPTransport("/mcp", nil)

	var receivedParams map[string]interface{}
	s.RegisterHandler("test/params", func(msg *types.MCPMessage) *types.MCPMessage {
		paramsMap, ok := msg.Params.(map[string]interface{})
		require.True(t, ok)
		receivedParams = paramsMap
		return &types.MCPMessage{Jsonrpc: "2.0", ID: msg.ID, Result: "ok"}
	})

	reqBody := `{"jsonrpc":"2.0","id":"1","method":"test/params","params":{"custom":"value"}}`
	c, w, _ := setupTestGinContext("POST", "/mcp", bytes.NewBufferString(reqBody), nil)
	c.Request.Header.Set("Content-Type", "application/json")

	s.HandleMessage(c)

	assert.Equal(t, http.StatusOK, w.Code)
	require.NotNil(t, receivedParams)
	assert.Equal(t, "value", receivedParams["custom"], "Original params must be preserved")
	assert.NotEmpty(t, receivedParams["_mcpConnectionID"], "_mcpConnectionID must be injected")
}

func TestStreamableHTTPTransport_MCPConnectionID_InjectedWhenParamsNil(t *testing.T) {
	// If the message has no params field, HandleMessage must still inject _mcpConnectionID.
	s := setupTestStreamableHTTPTransport("/mcp", nil)

	var receivedParams map[string]interface{}
	s.RegisterHandler("test/nilparams", func(msg *types.MCPMessage) *types.MCPMessage {
		paramsMap, ok := msg.Params.(map[string]interface{})
		require.True(t, ok, "Params should be initialised as a map even when absent in request")
		receivedParams = paramsMap
		return &types.MCPMessage{Jsonrpc: "2.0", ID: msg.ID, Result: "ok"}
	})

	// No "params" field in the request body.
	reqBody := `{"jsonrpc":"2.0","id":"1","method":"test/nilparams"}`
	c, w, _ := setupTestGinContext("POST", "/mcp", bytes.NewBufferString(reqBody), nil)
	c.Request.Header.Set("Content-Type", "application/json")

	s.HandleMessage(c)

	assert.Equal(t, http.StatusOK, w.Code)
	require.NotNil(t, receivedParams)
	assert.NotEmpty(t, receivedParams["_mcpConnectionID"])
}

// --- HandleConnection (optional SSE for server push) ---

func TestStreamableHTTPTransport_HandleConnection_MissingSessionID(t *testing.T) {
	s := setupTestStreamableHTTPTransport("/mcp", nil)
	// No Mcp-Session-Id header.
	c, w, cancel := setupTestGinContext("GET", "/mcp", nil, nil)
	defer cancel()

	s.HandleConnection(c)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "Missing Mcp-Session-Id header")
}

func TestStreamableHTTPTransport_HandleConnection_DisallowedOrigin(t *testing.T) {
	s := setupTestStreamableHTTPTransport("/mcp", []string{"https://allowed.example.com"})
	c, w, cancel := setupTestGinContext("GET", "/mcp", nil, nil)
	defer cancel()
	c.Request.Header.Set("Mcp-Session-Id", "some-session")
	c.Request.Header.Set("Origin", "https://evil.example.com")

	s.HandleConnection(c)

	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Contains(t, w.Body.String(), "Origin not allowed")
}

func TestStreamableHTTPTransport_HandleConnection_SSEHeaders(t *testing.T) {
	s := setupTestStreamableHTTPTransport("/mcp", nil)
	sessionID := "test-session-sse"

	c, w, cancel := setupTestGinContext("GET", "/mcp", nil, nil)
	c.Request.Header.Set("Mcp-Session-Id", sessionID)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		s.HandleConnection(c)
	}()

	// Wait until the session is registered — this happens *after* the response headers are
	// written, giving us a race-free synchronisation point instead of an arbitrary sleep.
	require.Eventually(t, func() bool {
		s.sMu.RLock()
		defer s.sMu.RUnlock()
		_, ok := s.sessions[sessionID]
		return ok
	}, time.Second, 5*time.Millisecond, "Session should be registered")

	// Headers are stable once the session is registered — safe to read without a race.
	assert.Equal(t, "text/event-stream", w.Header().Get("Content-Type"))
	assert.Equal(t, "no-cache", w.Header().Get("Cache-Control"))
	assert.Equal(t, "keep-alive", w.Header().Get("Connection"))

	cancel()
	wg.Wait()

	// Session must be cleaned up after disconnect.
	s.sMu.RLock()
	_, existsAfter := s.sessions[sessionID]
	s.sMu.RUnlock()
	assert.False(t, existsAfter, "Session should be removed after HandleConnection returns")
}

func TestStreamableHTTPTransport_HandleConnection_SendsMessage(t *testing.T) {
	s := setupTestStreamableHTTPTransport("/mcp", nil)
	sessionID := "session-msg-test"

	c, w, cancel := setupTestGinContext("GET", "/mcp", nil, nil)
	c.Request.Header.Set("Mcp-Session-Id", sessionID)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		s.HandleConnection(c)
	}()

	time.Sleep(50 * time.Millisecond)

	// Retrieve the session channel and push a notification.
	s.sMu.RLock()
	sess, exists := s.sessions[sessionID]
	s.sMu.RUnlock()
	require.True(t, exists, "Session must exist before sending")

	testMsg := &types.MCPMessage{
		Jsonrpc: "2.0",
		Method:  "notifications/tools/listChanged",
	}
	sess.Channel <- testMsg

	// Give the goroutine time to write the event.
	time.Sleep(50 * time.Millisecond)

	cancel()
	wg.Wait()

	body, _ := io.ReadAll(w.Body)
	bodyStr := string(body)
	assert.Contains(t, bodyStr, "event: message")
	assert.Contains(t, bodyStr, `"method":"notifications/tools/listChanged"`)
}

func TestStreamableHTTPTransport_HandleConnection_DuplicateSessionID(t *testing.T) {
	// When a client reconnects with the same Mcp-Session-Id while a session is already
	// open, Streamable HTTP silently overwrites the sessions map entry.
	//
	// This is notably different from SSE transport, which explicitly closes the old channel
	// and removes the old entry before setting up the new connection (sse.go:76-82).
	//
	// Consequence: the old goroutine's deferred delete(s.sessions, sessionID) will remove
	// the *new* session from the map when the old goroutine eventually finishes — meaning
	// any NotifyToolsChanged calls sent after the old goroutine exits will not reach the
	// active client.
	s := setupTestStreamableHTTPTransport("/mcp", nil)
	sessionID := "duplicate-session"

	// Simulate an already-open session by injecting it directly, as the SSE test does.
	oldChan := make(chan *types.MCPMessage, 1)
	s.sMu.Lock()
	s.sessions[sessionID] = &streamableSession{Channel: oldChan}
	s.sMu.Unlock()

	c, _, cancel := setupTestGinContext("GET", "/mcp", nil, nil)
	c.Request.Header.Set("Mcp-Session-Id", sessionID)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		s.HandleConnection(c)
	}()

	time.Sleep(100 * time.Millisecond)

	// Unlike SSE transport, the old channel is NOT closed on reconnect — it is only
	// abandoned (and will remain open until the old goroutine's context is cancelled).
	oldChanStillOpen := true
	select {
	case _, ok := <-oldChan:
		if !ok {
			oldChanStillOpen = false
		}
	case <-time.After(50 * time.Millisecond):
		// Timeout: channel is still open — expected for Streamable HTTP.
	}
	assert.True(t, oldChanStillOpen,
		"Streamable HTTP does NOT close the old session channel on reconnect (unlike SSE)")

	// The map now points to the new session.
	s.sMu.RLock()
	currentSess, exists := s.sessions[sessionID]
	s.sMu.RUnlock()
	assert.True(t, exists, "A session should still be registered under the same ID")
	assert.NotEqual(t, oldChan, currentSess.Channel,
		"The sessions map must point to the new session's channel, not the old one")

	cancel()
	wg.Wait()

	// After the new goroutine's context is cancelled, its deferred cleanup removes the entry.
	s.sMu.RLock()
	_, existsAfter := s.sessions[sessionID]
	s.sMu.RUnlock()
	assert.False(t, existsAfter, "Session should be removed after HandleConnection returns")
}

func TestStreamableHTTPTransport_HandleConnection_CORSWithAllowedOrigin(t *testing.T) {
	allowedOrigin := "https://app.example.com"
	s := setupTestStreamableHTTPTransport("/mcp", []string{allowedOrigin})
	sessionID := "session-cors"

	c, w, cancel := setupTestGinContext("GET", "/mcp", nil, nil)
	c.Request.Header.Set("Mcp-Session-Id", sessionID)
	c.Request.Header.Set("Origin", allowedOrigin)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		s.HandleConnection(c)
	}()

	// Wait until the session is registered — headers are written before the session map
	// is updated, so this is a race-free synchronisation point.
	require.Eventually(t, func() bool {
		s.sMu.RLock()
		defer s.sMu.RUnlock()
		_, ok := s.sessions[sessionID]
		return ok
	}, time.Second, 5*time.Millisecond, "Session should be registered")

	// The response ACAO header must echo the validated origin, not "*".
	assert.Equal(t, allowedOrigin, w.Header().Get("Access-Control-Allow-Origin"))

	cancel()
	wg.Wait()
}

// --- NotifyToolsChanged ---

func TestStreamableHTTPTransport_NotifyToolsChanged(t *testing.T) {
	s := setupTestStreamableHTTPTransport("/mcp", nil)

	// Register two SSE sessions manually.
	msgChan1 := make(chan *types.MCPMessage, 1)
	msgChan2 := make(chan *types.MCPMessage, 1)

	s.sMu.Lock()
	s.sessions["session-1"] = &streamableSession{Channel: msgChan1}
	s.sessions["session-2"] = &streamableSession{Channel: msgChan2}
	s.sMu.Unlock()

	s.NotifyToolsChanged()

	receive := func(ch chan *types.MCPMessage) *types.MCPMessage {
		select {
		case msg := <-ch:
			return msg
		case <-time.After(200 * time.Millisecond):
			return nil
		}
	}

	msg1 := receive(msgChan1)
	msg2 := receive(msgChan2)

	require.NotNil(t, msg1, "Session 1 should receive a notification")
	require.NotNil(t, msg2, "Session 2 should receive a notification")

	assert.Equal(t, "notifications/tools/listChanged", msg1.Method)
	assert.Equal(t, "2.0", msg1.Jsonrpc)
	assert.Equal(t, "notifications/tools/listChanged", msg2.Method)
}

func TestStreamableHTTPTransport_NotifyToolsChanged_NoSessions(t *testing.T) {
	s := setupTestStreamableHTTPTransport("/mcp", nil)
	// No sessions registered — must not panic.
	assert.NotPanics(t, func() { s.NotifyToolsChanged() })
}

// --- Concurrency ---

func TestStreamableHTTPTransport_ConcurrentHandleMessage(t *testing.T) {
	// Verify that concurrent POST requests do not race on the requestAuths map.
	s := setupTestStreamableHTTPTransport("/mcp", nil)
	s.RegisterHandler("test/concurrent", func(msg *types.MCPMessage) *types.MCPMessage {
		time.Sleep(5 * time.Millisecond) // simulate work
		return &types.MCPMessage{Jsonrpc: "2.0", ID: msg.ID, Result: "ok"}
	})

	const numRequests = 20
	var wg sync.WaitGroup
	wg.Add(numRequests)

	for i := 0; i < numRequests; i++ { //nolint:intrange
		go func(i int) {
			defer wg.Done()
			reqBody := fmt.Sprintf(`{"jsonrpc":"2.0","id":"%d","method":"test/concurrent","params":{}}`, i)
			c, w, _ := setupTestGinContext("POST", "/mcp", bytes.NewBufferString(reqBody), nil)
			c.Request.Header.Set("Content-Type", "application/json")
			c.Request.Header.Set("Authorization", fmt.Sprintf("Bearer token-%d", i))
			s.HandleMessage(c)
			assert.Equal(t, http.StatusOK, w.Code)
		}(i)
	}

	wg.Wait()

	// After all requests complete, requestAuths must be empty (all cleaned up).
	s.rMu.RLock()
	remaining := len(s.requestAuths)
	s.rMu.RUnlock()
	assert.Zero(t, remaining, "requestAuths should be empty after all requests complete")
}

func TestStreamableHTTPTransport_ConcurrentRegisterHandler(t *testing.T) {
	s := setupTestStreamableHTTPTransport("/mcp", nil)

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s.RegisterHandler(fmt.Sprintf("method/%d", i), func(msg *types.MCPMessage) *types.MCPMessage {
				return nil
			})
		}(i)
	}
	wg.Wait()

	s.hMu.RLock()
	count := len(s.handlers)
	s.hMu.RUnlock()
	assert.Equal(t, 10, count)
}

// --- Stateless behaviour ---

func TestStreamableHTTPTransport_StatelessRequests(t *testing.T) {
	// Each POST is handled independently; no prior connection needed.
	s := setupTestStreamableHTTPTransport("/mcp", nil)
	s.RegisterHandler("test/stateless", func(msg *types.MCPMessage) *types.MCPMessage {
		return &types.MCPMessage{Jsonrpc: "2.0", ID: msg.ID, Result: "stateless-ok"}
	})

	// Send three independent POSTs, each with a different ID.
	for _, id := range []string{"a", "b", "c"} {
		reqBody := fmt.Sprintf(`{"jsonrpc":"2.0","id":"%s","method":"test/stateless","params":{}}`, id)
		c, w, _ := setupTestGinContext("POST", "/mcp", bytes.NewBufferString(reqBody), nil)
		c.Request.Header.Set("Content-Type", "application/json")

		s.HandleMessage(c)

		assert.Equal(t, http.StatusOK, w.Code)
		body := w.Body.String()
		assert.Contains(t, body, fmt.Sprintf(`"id":"%s"`, id))
		assert.Contains(t, body, `"result":"stateless-ok"`)
	}

	// No sessions or auth state should have accumulated.
	s.sMu.RLock()
	sessionCount := len(s.sessions)
	s.sMu.RUnlock()
	assert.Zero(t, sessionCount, "Stateless requests must not create SSE sessions")

	s.rMu.RLock()
	authCount := len(s.requestAuths)
	s.rMu.RUnlock()
	assert.Zero(t, authCount, "requestAuths must be empty after all requests complete")
}

// --- Response body contains correct CORS header for open access ---

func TestStreamableHTTPTransport_HandleMessage_CORSWildcardWhenNoAllowlist(t *testing.T) {
	// When there is no allowlist, Access-Control-Allow-Origin is not set by HandleMessage
	// (it is only set by HandleConnection). This test documents the current behaviour:
	// HandleMessage does not write CORS headers itself — that is the responsibility of
	// any CORS middleware applied to the Gin router.
	s := setupTestStreamableHTTPTransport("/mcp", nil)
	s.RegisterHandler("test/cors", func(msg *types.MCPMessage) *types.MCPMessage {
		return &types.MCPMessage{Jsonrpc: "2.0", ID: msg.ID, Result: "ok"}
	})

	reqBody := `{"jsonrpc":"2.0","id":"1","method":"test/cors","params":{}}`
	c, w, _ := setupTestGinContext("POST", "/mcp", bytes.NewBufferString(reqBody), nil)
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.Header.Set("Origin", "https://browser.example.com")

	s.HandleMessage(c)

	assert.Equal(t, http.StatusOK, w.Code)
	// HandleMessage does not set Access-Control-Allow-Origin for POST requests.
	assert.Empty(t, w.Header().Get("Access-Control-Allow-Origin"))
}

// --- Implements Transport interface ---

func TestStreamableHTTPTransport_ImplementsTransportInterface(t *testing.T) {
	// Compile-time check: StreamableHTTPTransport must satisfy the Transport interface.
	var _ Transport = (*StreamableHTTPTransport)(nil)
}

// --- Long description test for tools/list method name collision ---

func TestStreamableHTTPTransport_HandleMessage_ResponseDirectlyInBody(t *testing.T) {
	// Verify that the response is returned synchronously in the HTTP response body,
	// not via an SSE channel (key difference from SSE transport).
	s := setupTestStreamableHTTPTransport("/mcp", nil)

	expectedResult := "direct-body-response"
	s.RegisterHandler("test/direct", func(msg *types.MCPMessage) *types.MCPMessage {
		return &types.MCPMessage{Jsonrpc: "2.0", ID: msg.ID, Result: expectedResult}
	})

	reqBody := `{"jsonrpc":"2.0","id":"42","method":"test/direct","params":{}}`
	c, w, _ := setupTestGinContext("POST", "/mcp", bytes.NewBufferString(reqBody), nil)
	c.Request.Header.Set("Content-Type", "application/json")

	s.HandleMessage(c)

	// Must return 200 synchronously.
	assert.Equal(t, http.StatusOK, w.Code)

	body := w.Body.String()
	// Response body must contain the JSON-RPC result.
	assert.True(t, strings.Contains(body, expectedResult), "Response body should contain the result directly")
	assert.True(t, strings.Contains(body, `"id":"42"`), "Response body should contain the request ID")

	// No SSE sessions should exist.
	s.sMu.RLock()
	assert.Empty(t, s.sessions, "No SSE session should be created for a POST request")
	s.sMu.RUnlock()
}
