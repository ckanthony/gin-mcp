package transport

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ckanthony/gin-mcp/pkg/types"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
)

// isDebugMode returns true if Gin is in debug mode
func isDebugMode() bool {
	return gin.Mode() == gin.DebugMode
}

const (
	keepAliveInterval = 15 * time.Second
	writeTimeout      = 10 * time.Second
)

// ConnectionContext holds the context for an SSE connection
type ConnectionContext struct {
	Channel    chan *types.MCPMessage
	AuthHeader string // Authorization header from the SSE connection
}

// SSETransport handles MCP communication over Server-Sent Events.
type SSETransport struct {
	mountPath   string
	handlers    map[string]MessageHandler
	connections map[string]*ConnectionContext
	hMu         sync.RWMutex // Mutex for handlers map
	cMu         sync.RWMutex // Mutex for connections map
}

// NewSSETransport creates a new SSETransport instance.
func NewSSETransport(mountPath string) *SSETransport {
	if isDebugMode() {
		log.Infof("[SSE] Creating new transport at %s", mountPath)
	}
	return &SSETransport{
		mountPath:   mountPath,
		handlers:    make(map[string]MessageHandler),
		connections: make(map[string]*ConnectionContext),
	}
}

// MountPath returns the base path where the transport is mounted.
func (s *SSETransport) MountPath() string {
	return s.mountPath
}

// HandleConnection sets up the SSE connection using Gin's SSEvent helper.
func (s *SSETransport) HandleConnection(c *gin.Context) {
	// Get sessionId from query parameter
	connID := c.Query("sessionId")
	if connID == "" {
		connID = uuid.New().String()
	}
	if isDebugMode() {
		log.Printf("[SSE] New connection %s from %s", connID, c.Request.RemoteAddr)
	}

	// Check if connection already exists
	s.cMu.RLock()
	existingChan, exists := s.connections[connID]
	s.cMu.RUnlock()

	if exists {
		if isDebugMode() {
			log.Printf("[SSE] Connection %s already exists, closing old connection", connID)
		}
		close(existingChan.Channel)
		s.RemoveConnection(connID)
	}

	// Set headers before anything else to ensure they're sent
	h := c.Writer.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Access-Control-Allow-Headers", "Content-Type, Content-Length, Accept-Encoding, X-CSRF-Token, Authorization, accept, origin, Cache-Control, X-Requested-With, X-Connection-ID")
	h.Set("Access-Control-Expose-Headers", "X-Connection-ID")
	h.Set("X-Connection-ID", connID)

	// Capture Authorization header for forwarding to tool execution
	authHeader := c.Request.Header.Get("Authorization")

	// Create buffered channel for messages
	msgChan := make(chan *types.MCPMessage, 100)

	// Create connection context with auth header
	connCtx := &ConnectionContext{
		Channel:    msgChan,
		AuthHeader: authHeader,
	}

	// Add connection to registry
	s.AddConnection(connID, connCtx)

	// Create a context with cancel for coordinating goroutines
	ctx, cancel := context.WithCancel(c.Request.Context())
	defer cancel()                   // Ensure all resources are cleaned up
	defer s.RemoveConnection(connID) // Put deferred call back

	// Check if streaming is supported
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		log.Errorf("[SSE] Streaming unsupported for connection %s", connID)
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Streaming not supported"})
		return
	}

	// Send initial endpoint event
	endpointURL := fmt.Sprintf("%s?sessionId=%s", s.mountPath, connID)
	if err := writeSSEEvent(c.Writer, "endpoint", endpointURL); err != nil {
		log.Errorf("[SSE] Failed to send endpoint event: %v", err)
		return
	}
	flusher.Flush()

	// Send ready event
	readyMsg := &types.MCPMessage{
		Jsonrpc: "2.0",
		Method:  "mcp-ready",
		Params: map[string]interface{}{
			"connectionId": connID,
			"status":       "connected",
			"protocol":     "2.0",
		},
	}
	if err := writeSSEEvent(c.Writer, "message", readyMsg); err != nil {
		log.Errorf("[SSE] Failed to send ready event: %v", err)
		return
	}
	flusher.Flush()

	// Start keep-alive goroutine
	go func() {
		ticker := time.NewTicker(keepAliveInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				pingMsg := &types.MCPMessage{
					Jsonrpc: "2.0",
					Method:  "ping",
					Params: map[string]interface{}{
						"timestamp": time.Now().Unix(),
					},
				}
				if err := writeSSEEvent(c.Writer, "message", pingMsg); err != nil {
					if isDebugMode() {
						log.Printf("[SSE] Failed to send keep-alive: %v", err)
					}
					cancel()
					return
				}
				flusher.Flush()
			}
		}
	}()

	// Main message loop
	for {
		select {
		case <-ctx.Done():
			if isDebugMode() {
				log.Printf("[SSE] Connection %s closed", connID)
			}
			return

		case msg, ok := <-msgChan:
			if !ok {
				if isDebugMode() {
					log.Printf("[SSE] Message channel closed for %s", connID)
				}
				return
			}

			if err := writeSSEEvent(c.Writer, "message", msg); err != nil {
				log.Errorf("[SSE] Failed to send message: %v", err)
				return
			}
			flusher.Flush()
		}
	}
}

// writeSSEEvent writes a Server-Sent Event to the response writer
func writeSSEEvent(w http.ResponseWriter, event string, data interface{}) error {
	var dataStr string
	switch event {
	case "endpoint":
		// Endpoint data is expected to be a raw string URL
		urlStr, ok := data.(string)
		if !ok {
			return fmt.Errorf("invalid data type for endpoint event: expected string, got %T", data)
		}
		dataStr = urlStr // Use the raw string
	case "message":
		// Message data should be a JSON-RPC message struct, marshal it
		msg, ok := data.(*types.MCPMessage)
		if !ok {
			if isDebugMode() {
				log.Printf("[SSE writeSSEEvent] Data for 'message' event was not *types.MCPMessage, attempting generic marshal. Type: %T", data)
			}
			jsonData, err := json.Marshal(data)
			if err != nil {
				return fmt.Errorf("failed to marshal event data for message event (non-MCPMessage type): %v", err)
			}
			dataStr = string(jsonData)
		} else {
			// Validate MCPMessage structure minimally (e.g., check for ID unless it's a notification)
			if msg.Method != "" && !strings.HasSuffix(msg.Method, "Changed") && msg.Method != "mcp-ready" && msg.Method != "ping" && msg.ID == nil {
				// Allow missing ID for specific notifications like listChanged, mcp-ready, ping
				return fmt.Errorf("missing ID in message for method: %s", msg.Method)
			}
			jsonData, err := json.Marshal(msg)
			if err != nil {
				return fmt.Errorf("failed to marshal MCPMessage event data: %v", err)
			}
			dataStr = string(jsonData)
		}
	default:
		// For unknown event types, attempt to JSON marshal, but log at debug level
		if isDebugMode() {
			log.Printf("[SSE] Unknown event type '%s' encountered in writeSSEEvent, attempting JSON marshal", event)
		}
		jsonData, err := json.Marshal(data)
		if err != nil {
			return fmt.Errorf("failed to marshal event data for unknown event '%s': %v", event, err)
		}
		dataStr = string(jsonData)
	}

	// Write the event with proper SSE format
	_, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, dataStr)
	if err != nil {
		// If writing fails, the connection is likely closed. Log the error.
		log.Errorf("[SSE] Failed to write SSE event '%s': %v", event, err)
	}
	return err
}

// HandleMessage processes incoming HTTP POST requests containing MCP messages.
func (s *SSETransport) HandleMessage(c *gin.Context) {
	if isDebugMode() {
		log.Printf("[SSE POST Handler] Request received for path: %s", c.Request.URL.Path)
	}

	// Get sessionId from header or query parameter
	connID := c.GetHeader("X-Connection-ID")
	if connID == "" {
		connID = c.Query("sessionId")
		if connID == "" {
			if isDebugMode() {
				log.Printf("[SSE POST] Missing connection ID. Headers: %v, URL: %s", c.Request.Header, c.Request.URL)
			}
			c.JSON(http.StatusBadRequest, gin.H{"error": "Missing connection identifier"})
			return
		} else if isDebugMode() {
			log.Printf("[SSE POST] Using connID from query param: %s", connID)
		}
	} else if isDebugMode() {
		log.Printf("[SSE POST] Using connID from header: %s", connID)
	}

	// Check if connection exists
	s.cMu.RLock()
	connCtx, exists := s.connections[connID]
	activeConnections := s.getActiveConnections() // Get active connections for logging
	s.cMu.RUnlock()

	if isDebugMode() {
		log.Printf("[SSE POST] Checking for connection %s. Exists: %t. Active: %v", connID, exists, activeConnections)
	}

	if !exists {
		if isDebugMode() {
			log.Printf("[SSE POST] Connection %s not found. Returning 404.", connID)
		}
		c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
		return
	}

	// Extract message channel from connection context
	msgChan := connCtx.Channel

	// Read and parse message
	var reqMsg types.MCPMessage
	if err := c.ShouldBindJSON(&reqMsg); err != nil {
		log.Errorf("[SSE] Failed to parse message: %v", err)
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("Invalid message format: %v", err)})
		return
	}

	// Inject connection ID into message for auth forwarding
	// Add _mcpConnectionID to params if params is a map
	if reqMsg.Params != nil {
		if paramsMap, ok := reqMsg.Params.(map[string]interface{}); ok {
			paramsMap["_mcpConnectionID"] = connID
		}
	}

	// Find handler
	s.hMu.RLock()
	handler, found := s.handlers[reqMsg.Method]
	s.hMu.RUnlock()

	if !found {
		if isDebugMode() {
			log.Printf("[SSE] No handler for method '%s'. Available: %v", reqMsg.Method, s.getRegisteredHandlers())
		}
		errMsg := &types.MCPMessage{
			Jsonrpc: "2.0",
			ID:      reqMsg.ID,
			Error: map[string]interface{}{
				"code":    -32601,
				"message": fmt.Sprintf("Method '%s' not found", reqMsg.Method),
			},
		}
		s.trySendMessage(connID, msgChan, errMsg)
		c.Status(http.StatusOK) // Changed from 204 to 200 for mcphost compatibility
		return
	}

	// Execute handler and send response
	respMsg := handler(c, &reqMsg)
	if ok := s.trySendMessage(connID, msgChan, respMsg); ok {
		c.Status(http.StatusOK) // Changed from 204 to 200 for mcphost compatibility
	} else {
		log.Errorf("[SSE] Failed to send response for %s", connID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to send response"})
	}
}

// getActiveConnections returns a list of active connection IDs for debugging
func (s *SSETransport) getActiveConnections() []string {
	s.cMu.RLock()
	defer s.cMu.RUnlock()

	connections := make([]string, 0, len(s.connections))
	for connID := range s.connections {
		connections = append(connections, connID)
	}
	return connections
}

// getRegisteredHandlers returns a list of registered method handlers for debugging
func (s *SSETransport) getRegisteredHandlers() []string {
	s.hMu.RLock()
	defer s.hMu.RUnlock()

	handlers := make([]string, 0, len(s.handlers))
	for method := range s.handlers {
		handlers = append(handlers, method)
	}
	return handlers
}

// SendInitialMessage is not directly used in this SSE flow, but part of interface.
// func (s *SSETransport) SendInitialMessage(c *gin.Context, msg *types.MCPMessage) error { ... }
// Commented out as it's not used by server.go and adds noise to interface implementation check

// RegisterHandler registers a message handler for a specific method.
func (s *SSETransport) RegisterHandler(method string, handler MessageHandler) {
	s.hMu.Lock()
	defer s.hMu.Unlock()
	s.handlers[method] = handler
	if isDebugMode() {
		log.Printf("[Transport DEBUG] Registered handler for method: %s", method)
	}
}

// AddConnection adds a connection context to the map.
func (s *SSETransport) AddConnection(connID string, connCtx *ConnectionContext) {
	s.cMu.Lock()
	defer s.cMu.Unlock()
	s.connections[connID] = connCtx
	if isDebugMode() {
		log.Printf("[Transport DEBUG] Added connection %s. Total: %d", connID, len(s.connections))
	}
}

// RemoveConnection removes a connection channel from the map.
func (s *SSETransport) RemoveConnection(connID string) {
	s.cMu.Lock()
	defer s.cMu.Unlock()
	_, exists := s.connections[connID]
	if exists {
		delete(s.connections, connID)
		if isDebugMode() {
			log.Printf("[Transport DEBUG] Removed connection %s. Total: %d", connID, len(s.connections))
		}
	}
}

// GetAuthHeader retrieves the Authorization header for a connection.
// Returns empty string if connection not found or no auth header was provided.
func (s *SSETransport) GetAuthHeader(connID string) string {
	s.cMu.RLock()
	defer s.cMu.RUnlock()
	if connCtx, exists := s.connections[connID]; exists {
		return connCtx.AuthHeader
	}
	return ""
}

// NotifyToolsChanged sends a tools/listChanged notification to all connected clients.
func (s *SSETransport) NotifyToolsChanged() {
	notification := &types.MCPMessage{
		Jsonrpc: "2.0",
		Method:  "tools/listChanged",
	}

	s.cMu.RLock()
	numConns := len(s.connections)
	channels := make([]chan *types.MCPMessage, 0, numConns)
	connIDs := make([]string, 0, numConns)
	for id, connCtx := range s.connections {
		channels = append(channels, connCtx.Channel)
		connIDs = append(connIDs, id)
	}
	s.cMu.RUnlock()

	if isDebugMode() {
		log.Printf("[Transport DEBUG] Notifying %d connections about tools change", numConns)
	}

	for i, ch := range channels {
		s.trySendMessage(connIDs[i], ch, notification)
	}
}

// trySendMessage attempts to send a message to a client channel with a timeout.
func (s *SSETransport) trySendMessage(connID string, msgChan chan<- *types.MCPMessage, msg *types.MCPMessage) bool {
	if msgChan == nil {
		if isDebugMode() {
			log.Printf("[trySendMessage %s] Cannot send message, channel is nil (connection likely closed or invalid)", connID)
		}
		return false
	}
	if isDebugMode() {
		method := msg.Method
		if method == "" {
			method = "<response>"
		}
		log.Printf("[trySendMessage %s] Attempting to send message to channel. Method: %s, ID: %s", connID, method, string(msg.ID))
	}
	select {
	case msgChan <- msg:
		if isDebugMode() {
			log.Printf("[trySendMessage %s] Successfully sent message to channel.", connID)
		}
		return true
	case <-time.After(2 * time.Second):
		if isDebugMode() {
			log.Printf("[trySendMessage %s] Timeout sending message (channel full or closed?).", connID)
		}
		return false
	}
}

// --- Helper Functions (tryGetRequestID, etc. - kept for reference if needed, but not strictly used by current flow) ---

/* // Remove unused tryGetRequestID function
// tryGetRequestID attempts to extract the 'id' field from a JSON body even if parsing failed
// This is a best-effort attempt for error reporting.
func tryGetRequestID(body io.ReadCloser) json.RawMessage {
	// We need to read the body and then restore it for potential re-reads
	bodyBytes, err := io.ReadAll(body)
	if err != nil {
		return nil
	}
	// Restore the body
	// This requires access to the gin.Context, which we don't have directly here.
	// For simplicity in this standalone transport, we'll omit body restoration.
	// A more robust implementation might pass the context or use a middleware approach.
	// c.Request.Body = io.NopCloser(bytes.NewReader(bodyBytes)) // Cannot do this here

	var raw map[string]json.RawMessage
	if json.Unmarshal(bodyBytes, &raw) == nil {
		if id, ok := raw[\"id\"]; ok {
			// Return the raw JSON bytes for the ID directly
			return id
		}
	}
	return nil
}
*/

/* // Remove unused tryGetRequestIDFromBytes function
// tryGetRequestIDFromBytes attempts to extract the 'id' field from a JSON body even if parsing failed
// This is a best-effort attempt for error reporting.
func tryGetRequestIDFromBytes(bodyBytes []byte) json.RawMessage {
	var raw map[string]json.RawMessage
	if json.Unmarshal(bodyBytes, &raw) == nil {
		if id, ok := raw[\"id\"]; ok {
			// Return the raw JSON bytes for the ID directly
			return id
		}
	}
	return nil
}
*/
