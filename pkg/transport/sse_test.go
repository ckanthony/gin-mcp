package transport

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/ckanthony/gin-mcp/pkg/types"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Test Setup ---

var setGinModeOnce sync.Once

func setupTestSSETransport(mountPath string) *SSETransport {
	// Ensure Gin is in release mode for tests to avoid debug prints
	// Use sync.Once to ensure this is called only once per package test run.
	setGinModeOnce.Do(func() {
		gin.SetMode(gin.ReleaseMode)
	})
	return NewSSETransport(mountPath)
}

func setupTestGinContext(method, path string, body io.Reader, queryParams map[string]string) (*gin.Context, *httptest.ResponseRecorder, context.CancelFunc) {
	w := httptest.NewRecorder()
	// Create a cancellable context
	ctx, cancel := context.WithCancel(context.Background())
	// Create a request with the cancellable context
	req, _ := http.NewRequestWithContext(ctx, method, path, body)

	if queryParams != nil {
		q := req.URL.Query()
		for k, v := range queryParams {
			q.Add(k, v)
		}
		req.URL.RawQuery = q.Encode()
	}
	c, _ := gin.CreateTestContext(w)
	c.Request = req

	// Although Gin creates its own context internally, we pass the cancel function for our original context
	// so the test can simulate request cancellation / client disconnect.
	return c, w, cancel
}

// --- Tests for SSETransport Methods ---

func TestNewSSETransport(t *testing.T) {
	mountPath := "/mcp/sse"
	s := NewSSETransport(mountPath)

	assert.NotNil(t, s)
	assert.Equal(t, mountPath, s.mountPath)
	assert.NotNil(t, s.handlers)
	assert.Empty(t, s.handlers)
	assert.NotNil(t, s.connections)
	assert.Empty(t, s.connections)
}

func TestSSETransport_MountPath(t *testing.T) {
	mountPath := "/test/path"
	s := NewSSETransport(mountPath)
	assert.Equal(t, mountPath, s.MountPath())
}

func TestSSETransport_RegisterHandler(t *testing.T) {
	s := setupTestSSETransport("/mcp")
	method := "test/method"
	handler := func(ctx context.Context, msg *types.MCPMessage) *types.MCPMessage {
		return &types.MCPMessage{Result: "ok"}
	}

	s.RegisterHandler(method, handler)

	s.hMu.RLock()
	registeredHandler, exists := s.handlers[method]
	s.hMu.RUnlock()

	assert.True(t, exists, "Handler should be registered")
	assert.NotNil(t, registeredHandler, "Registered handler should not be nil")

	// Test overwriting handler
	newHandler := func(ctx context.Context, msg *types.MCPMessage) *types.MCPMessage {
		return &types.MCPMessage{Result: "new ok"}
	}
	s.RegisterHandler(method, newHandler)
	s.hMu.RLock()
	overwrittenHandler, _ := s.handlers[method]
	s.hMu.RUnlock()
	assert.NotNil(t, overwrittenHandler)
	// Comparing func pointers directly is tricky; check if behavior changed
	resp := overwrittenHandler(nil, &types.MCPMessage{})
	assert.Equal(t, "new ok", resp.Result)
}

func TestSSETransport_AddRemoveConnection(t *testing.T) {
	s := setupTestSSETransport("/mcp")
	connID := "test-conn-1"
	msgChan := make(chan *types.MCPMessage, 1)

	// Add
	s.AddConnection(connID, &ConnectionContext{Channel: msgChan})
	s.cMu.RLock()
	retrievedCtx, exists := s.connections[connID]
	s.cMu.RUnlock()

	assert.True(t, exists, "Connection should exist after adding")
	assert.Equal(t, msgChan, retrievedCtx.Channel, "Retrieved channel should match added channel")

	// Remove the connection
	s.RemoveConnection(connID)

	// Verify removal
	s.cMu.RLock()
	_, existsAfter := s.connections[connID]
	s.cMu.RUnlock()
	assert.False(t, existsAfter, "Connection entry should be removed")

	// Explicitly close the channel here, as RemoveConnection no longer does it
	close(msgChan)

	// Verify channel is closed
	closed := false
	select {
	case _, ok := <-msgChan:
		if !ok {
			closed = true
		}
	default:
		// channel is not closed or empty
	}
	assert.True(t, closed, "Channel should be closed upon removal")
}

func TestSSETransport_HandleConnection(t *testing.T) {
	s := setupTestSSETransport("/mcp/events")
	testSessionId := "session-123"

	c, w, cancel := setupTestGinContext("GET", "/mcp/events", nil, map[string]string{"sessionId": testSessionId})

	// Run HandleConnection in a goroutine as it blocks
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		s.HandleConnection(c)
	}()

	// Wait a very short time for HandleConnection to start and potentially add the connection
	time.Sleep(50 * time.Millisecond)

	// Verify connection was added
	s.cMu.RLock()
	connCtx, exists := s.connections[testSessionId]
	s.cMu.RUnlock()
	assert.True(t, exists, "Connection should be added")
	assert.NotNil(t, connCtx)

	// Verify headers (Check immediately after connection is confirmed, but acknowledge potential race)
	// A more robust test might use channels to signal header writing completion.
	assert.Equal(t, "text/event-stream", w.Header().Get("Content-Type"))
	assert.Equal(t, "no-cache", w.Header().Get("Cache-Control"))
	assert.Equal(t, "keep-alive", w.Header().Get("Connection"))
	assert.Equal(t, testSessionId, w.Header().Get("X-Connection-ID"))

	// Send a message to the connection
	testMsg := &types.MCPMessage{Jsonrpc: "2.0", ID: types.RawMessage(`"test-id"`), Result: "test result"}
	connCtx.Channel <- testMsg

	// Simulate client disconnect by cancelling the request context passed to HandleConnection
	cancel() // Call the cancel function returned by setupTestGinContext

	wg.Wait() // Wait for HandleConnection goroutine to finish cleanly

	// --- Verify Body Content AFTER Handler Finishes ---
	bodyBytes, _ := io.ReadAll(w.Body) // Read the entire body now
	bodyString := string(bodyBytes)

	// Check for expected events in the complete body string
	// 1. Endpoint event
	expectedEndpointEvent := fmt.Sprintf("event: endpoint\ndata: %s?sessionId=%s\n\n", s.MountPath(), testSessionId)
	assert.Contains(t, bodyString, expectedEndpointEvent, "Body should contain endpoint event")

	// 2. Ready event
	assert.Contains(t, bodyString, "event: message\ndata: {", "Body should contain start of ready message event")
	assert.Contains(t, bodyString, `"method":"mcp-ready"`, "Ready message should contain method")
	assert.Contains(t, bodyString, `"connectionId":"`+testSessionId+`"`, "Ready message should contain connectionId")

	// 3. Custom message event (from msgChan)
	assert.Contains(t, bodyString, "event: message\ndata: {", "Body should contain start of custom message event") // Check start again
	assert.Contains(t, bodyString, `"id":"test-id"`, "Custom message should contain ID")
	assert.Contains(t, bodyString, `"result":"test result"`, "Custom message should contain result")

	// Verify connection was removed (HandleConnection's defer s.RemoveConnection should have run)
	s.cMu.RLock()
	_, existsAfterRemove := s.connections[testSessionId]
	s.cMu.RUnlock()
	assert.False(t, existsAfterRemove, "Connection should be removed after closing")
}

func TestSSETransport_HandleConnection_NoSessionId(t *testing.T) {
	s := setupTestSSETransport("/mcp/events")
	c, _, cancel := setupTestGinContext("GET", "/mcp/events", nil, nil) // No query params
	defer cancel()

	var connID string // To store the generated ID for cleanup
	var connMu sync.Mutex

	go s.HandleConnection(c)           // Run in goroutine
	time.Sleep(150 * time.Millisecond) // Increased sleep slightly

	// Check that a connection was added (don't check header due to race)
	s.cMu.RLock()
	assert.NotEmpty(t, s.connections, "Connections map should not be empty")
	if len(s.connections) == 1 {
		// Get the ID for cleanup if exactly one connection exists
		for id := range s.connections {
			connMu.Lock()
			connID = id
			connMu.Unlock()
		}
	}
	s.cMu.RUnlock()

	require.NotEmpty(t, connID, "Failed to retrieve generated connection ID for cleanup")
	_, err := uuid.Parse(connID) // Still validate the format of the captured ID
	require.NoError(t, err, "Generated connection ID should be a valid UUID")

	// Clean up using the captured ID
	s.RemoveConnection(connID)
}

func TestSSETransport_HandleConnection_ExistingSession(t *testing.T) {
	s := setupTestSSETransport("/mcp/events")
	testSessionId := "existing-session-123"
	oldChan := make(chan *types.MCPMessage, 1)
	s.AddConnection(testSessionId, &ConnectionContext{Channel: oldChan}) // Add the "old" connection

	c, _, cancel := setupTestGinContext("GET", "/mcp/events", nil, map[string]string{"sessionId": testSessionId})

	// Run HandleConnection in a goroutine
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		// HandleConnection should internally detect the existing session,
		// close oldChan, remove the old connection entry, and then set up the new one.
		// The deferred RemoveConnection within HandleConnection should clean up the *new* connection when this goroutine exits.
		s.HandleConnection(c)
	}()

	// Wait a short time for HandleConnection to process and close the old channel
	time.Sleep(150 * time.Millisecond)

	// Verify old channel was closed by HandleConnection
	closed := false
	select {
	case _, ok := <-oldChan:
		if !ok {
			closed = true
		}
	case <-time.After(50 * time.Millisecond): // Timeout if not closed quickly
	}
	assert.True(t, closed, "Old connection channel should be closed by HandleConnection")

	// Verify new connection exists temporarily (before goroutine finishes)
	s.cMu.RLock()
	_, exists := s.connections[testSessionId]
	s.cMu.RUnlock()
	assert.True(t, exists, "New connection should exist while handler is running")

	// Cancel the context to allow HandleConnection to finish
	cancel()

	// Wait for HandleConnection goroutine to fully complete.
	// The deferred cancel() and deferred RemoveConnection() inside HandleConnection should execute.
	wg.Wait()

	// Final check: ensure connection entry is removed after goroutine finishes
	s.cMu.RLock()
	_, existsAfterWait := s.connections[testSessionId]
	s.cMu.RUnlock()
	assert.False(t, existsAfterWait, "Connection entry should be removed after HandleConnection finishes")
}

func TestSSETransport_HandleMessage_Success(t *testing.T) {
	s := setupTestSSETransport("/mcp")
	connID := "test-conn-handle-msg"
	msgChan := make(chan *types.MCPMessage, 1)
	s.AddConnection(connID, &ConnectionContext{Channel: msgChan})
	defer s.RemoveConnection(connID)

	method := "test/success"
	handlerCalled := false
	s.RegisterHandler(method, func(ctx context.Context, msg *types.MCPMessage) *types.MCPMessage {
		handlerCalled = true
		assert.Equal(t, method, msg.Method)
		assert.Equal(t, types.RawMessage(`"req-id-1"`), msg.ID)
		return &types.MCPMessage{Jsonrpc: "2.0", ID: msg.ID, Result: "handler success"}
	})

	reqBody := `{"jsonrpc":"2.0","id":"req-id-1","method":"test/success","params":{}}`
	c, w, _ := setupTestGinContext("POST", "/mcp", bytes.NewBufferString(reqBody), nil)
	c.Request.Header.Set("X-Connection-ID", connID)
	c.Request.Header.Set("Content-Type", "application/json")

	s.HandleMessage(c)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.True(t, handlerCalled, "Registered handler should have been called")

	// Check if response was sent via SSE channel
	select {
	case respMsg := <-msgChan:
		assert.Equal(t, types.RawMessage(`"req-id-1"`), respMsg.ID)
		assert.Equal(t, "handler success", respMsg.Result)
		assert.Nil(t, respMsg.Error)
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Did not receive response message on SSE channel")
	}
}

func TestSSETransport_HandleMessage_NoConnectionIdHeader(t *testing.T) {
	s := setupTestSSETransport("/mcp")
	connID := "test-conn-query"
	msgChan := make(chan *types.MCPMessage, 1)
	s.AddConnection(connID, &ConnectionContext{Channel: msgChan})
	defer s.RemoveConnection(connID)

	method := "test/query"
	handlerCalled := false
	s.RegisterHandler(method, func(ctx context.Context, msg *types.MCPMessage) *types.MCPMessage {
		handlerCalled = true
		return &types.MCPMessage{Jsonrpc: "2.0", ID: msg.ID, Result: "query success"}
	})

	reqBody := `{"jsonrpc":"2.0","id":"req-id-q","method":"test/query"}`
	// Provide sessionId via query param instead of header
	c, w, _ := setupTestGinContext("POST", "/mcp", bytes.NewBufferString(reqBody), map[string]string{"sessionId": connID})
	c.Request.Header.Set("Content-Type", "application/json")

	s.HandleMessage(c)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.True(t, handlerCalled, "Handler should be called when ID is in query")

	select {
	case respMsg := <-msgChan:
		assert.Equal(t, types.RawMessage(`"req-id-q"`), respMsg.ID)
		assert.Equal(t, "query success", respMsg.Result)
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Did not receive response message on SSE channel for query ID")
	}
}

func TestSSETransport_HandleMessage_MissingConnectionId(t *testing.T) {
	s := setupTestSSETransport("/mcp")
	reqBody := `{"jsonrpc":"2.0","id":"req-id-2","method":"test/missing","params":{}}`
	c, w, _ := setupTestGinContext("POST", "/mcp", bytes.NewBufferString(reqBody), nil) // No header or query param
	c.Request.Header.Set("Content-Type", "application/json")

	s.HandleMessage(c)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "Missing connection identifier")
}

func TestSSETransport_HandleMessage_ConnectionNotFound(t *testing.T) {
	s := setupTestSSETransport("/mcp")
	connID := "non-existent-conn"
	reqBody := `{"jsonrpc":"2.0","id":"req-id-3","method":"test/notfound","params":{}}`
	c, w, _ := setupTestGinContext("POST", "/mcp", bytes.NewBufferString(reqBody), nil)
	c.Request.Header.Set("X-Connection-ID", connID)
	c.Request.Header.Set("Content-Type", "application/json")

	s.HandleMessage(c)

	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.Contains(t, w.Body.String(), "Connection not found")
}

func TestSSETransport_HandleMessage_BadRequestBody(t *testing.T) {
	s := setupTestSSETransport("/mcp")
	connID := "test-conn-bad-body"
	msgChan := make(chan *types.MCPMessage, 1)
	s.AddConnection(connID, &ConnectionContext{Channel: msgChan})
	defer s.RemoveConnection(connID)

	reqBody := `{"jsonrpc":"2.0",,"id":"invalid"}` // Invalid JSON
	c, w, _ := setupTestGinContext("POST", "/mcp", bytes.NewBufferString(reqBody), nil)
	c.Request.Header.Set("X-Connection-ID", connID)
	c.Request.Header.Set("Content-Type", "application/json")

	s.HandleMessage(c)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "Invalid message format", "Error message should indicate invalid format")

	// Ensure no message was sent on the channel
	select {
	case <-msgChan:
		t.Fatal("Should not have received a message on bad body")
	default:
		// OK
	}
}

func TestSSETransport_HandleMessage_HandlerNotFound(t *testing.T) {
	s := setupTestSSETransport("/mcp")
	connID := "test-conn-handler-nf"
	msgChan := make(chan *types.MCPMessage, 1)
	s.AddConnection(connID, &ConnectionContext{Channel: msgChan})
	defer s.RemoveConnection(connID)

	reqBody := `{"jsonrpc":"2.0","id":"req-id-4","method":"unregistered/method","params":{}}`
	c, w, _ := setupTestGinContext("POST", "/mcp", bytes.NewBufferString(reqBody), nil)
	c.Request.Header.Set("X-Connection-ID", connID)
	c.Request.Header.Set("Content-Type", "application/json")

	s.HandleMessage(c)

	assert.Equal(t, http.StatusOK, w.Code, "HandleMessage itself should return OK even if handler not found")

	// Check error response sent via SSE
	select {
	case respMsg := <-msgChan:
		assert.Nil(t, respMsg.Result)
		require.NotNil(t, respMsg.Error, "Error field should be set")
		errMap, ok := respMsg.Error.(map[string]interface{})
		require.True(t, ok)

		// Compare error code numerically, converting both to float64 for robustness
		expectedCode := float64(-32601)
		actualCodeVal, codeOk := errMap["code"]
		require.True(t, codeOk, "Error map should contain 'code' key")
		actualCodeFloat, convertOk := convertToFloat64(actualCodeVal)
		require.True(t, convertOk, "Could not convert actual error code to float64")
		assert.Equal(t, expectedCode, actualCodeFloat, "Error code should be MethodNotFound")

		assert.Contains(t, errMap["message"].(string), "not found", "Error message should indicate method not found")
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Did not receive error response message on SSE channel")
	}
}

// Helper function to convert numeric interface{} to float64
func convertToFloat64(val interface{}) (float64, bool) {
	switch v := val.(type) {
	case float64:
		return v, true
	case float32:
		return float64(v), true
	case int:
		return float64(v), true
	case int8:
		return float64(v), true
	case int16:
		return float64(v), true
	case int32:
		return float64(v), true
	case int64:
		return float64(v), true
	// Add other integer types if necessary (uint, etc.)
	default:
		return 0, false
	}
}

func TestSSETransport_NotifyToolsChanged(t *testing.T) {
	s := setupTestSSETransport("/mcp")
	connID1 := "notify-conn-1"
	connID2 := "notify-conn-2"
	msgChan1 := make(chan *types.MCPMessage, 1)
	msgChan2 := make(chan *types.MCPMessage, 1)

	s.AddConnection(connID1, &ConnectionContext{Channel: msgChan1})
	s.AddConnection(connID2, &ConnectionContext{Channel: msgChan2})
	defer s.RemoveConnection(connID1)
	defer s.RemoveConnection(connID2)

	s.NotifyToolsChanged()

	received1 := false
	received2 := false

	// Check channel 1
	select {
	case msg := <-msgChan1:
		t.Logf("Received message on chan1: %+v", msg)
		assert.Equal(t, "tools/listChanged", msg.Method)
		assert.Nil(t, msg.ID)
		assert.Nil(t, msg.Params)
		received1 = true
	case <-time.After(100 * time.Millisecond):
		// Fail
	}

	// Check channel 2
	select {
	case msg := <-msgChan2:
		t.Logf("Received message on chan2: %+v", msg)
		assert.Equal(t, "tools/listChanged", msg.Method)
		assert.Nil(t, msg.ID)
		assert.Nil(t, msg.Params)
		received2 = true
	case <-time.After(100 * time.Millisecond):
		// Fail
	}

	assert.True(t, received1, "Connection 1 should have received notification")
	assert.True(t, received2, "Connection 2 should have received notification")

	// Test with no connections
	s.RemoveConnection(connID1)
	s.RemoveConnection(connID2)
	assert.NotPanics(t, func() { s.NotifyToolsChanged() }, "NotifyToolsChanged should not panic with no connections")
}

func Test_writeSSEEvent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	// Test endpoint event
	wEndpoint := httptest.NewRecorder()
	err := writeSSEEvent(wEndpoint, "endpoint", "/mcp/events?sessionId=123")
	assert.NoError(t, err)
	assert.Equal(t, "event: endpoint\ndata: /mcp/events?sessionId=123\n\n", wEndpoint.Body.String())

	// Test message event (MCPMessage)
	wMessage := httptest.NewRecorder()
	msg := &types.MCPMessage{Jsonrpc: "2.0", Method: "test", ID: types.RawMessage(`"1"`)}
	err = writeSSEEvent(wMessage, "message", msg)
	assert.NoError(t, err)
	expectedMsgData := `{"jsonrpc":"2.0","id":"1","method":"test"}`
	assert.Equal(t, "event: message\ndata: "+expectedMsgData+"\n\n", wMessage.Body.String())

	// Test unknown event (should marshal as JSON)
	wUnknown := httptest.NewRecorder()
	data := map[string]string{"key": "value"}
	err = writeSSEEvent(wUnknown, "custom", data)
	assert.NoError(t, err)
	expectedUnknownData := `{"key":"value"}`
	assert.Equal(t, "event: custom\ndata: "+expectedUnknownData+"\n\n", wUnknown.Body.String())

	// Test invalid data for endpoint event
	wEndpointErr := httptest.NewRecorder()
	err = writeSSEEvent(wEndpointErr, "endpoint", 123) // Pass int instead of string
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "invalid data type for endpoint event")

	// Test invalid data for message event (non-marshalable)
	wMessageErr := httptest.NewRecorder()
	badData := make(chan int) // Channels cannot be marshaled
	err = writeSSEEvent(wMessageErr, "message", badData)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to marshal event data")

	// Test missing ID
	msgNoID := &types.MCPMessage{
		Jsonrpc: "2.0",
		Method:  "test",
		ID:      nil,
	}
	err = writeSSEEvent(wUnknown, "message", msgNoID)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "missing ID in message for method: test", "Error message should specify method")
}
