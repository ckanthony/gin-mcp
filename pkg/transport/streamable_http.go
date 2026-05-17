package transport

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/ckanthony/gin-mcp/pkg/types"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
)

// StreamableHTTPTransport handles MCP communication over Streamable HTTP (MCP spec 2025-03-26).
//
// Unlike SSE transport, this is stateless: POST requests return JSON responses directly in the
// HTTP body. No persistent SSE connection is required before sending messages, which means:
//   - Works transparently with any load balancer (no pod affinity required)
//   - Fully compatible with horizontal autoscaling
//   - No connection state to manage or reconnect
//
// An optional GET endpoint is provided for server-initiated notifications. Clients that
// need push notifications (e.g. tools/listChanged) can open an SSE session via GET with
// the Mcp-Session-Id header. This is not required for basic request-response operation.
//
// Security: if AllowedOrigins is non-empty, any request carrying an Origin header whose
// value is not in the allowlist is rejected with 403 Forbidden. This prevents DNS rebinding
// attacks. Server-to-server calls that omit the Origin header are always permitted.
type StreamableHTTPTransport struct {
	mountPath      string
	allowedOrigins []string // nil/empty = allow all origins
	handlers       map[string]MessageHandler
	hMu            sync.RWMutex
	// Transient per-request auth headers: request UUID → Authorization header value.
	// Populated at the start of HandleMessage and deleted when the handler returns.
	requestAuths map[string]string
	rMu          sync.RWMutex
	// Optional SSE sessions for server-initiated notifications: Mcp-Session-Id → session.
	sessions map[string]*streamableSession
	sMu      sync.RWMutex
}

type streamableSession struct {
	Channel chan *types.MCPMessage
}

// NewStreamableHTTPTransport creates a new StreamableHTTPTransport mounted at mountPath.
//
// allowedOrigins is an optional list of permitted Origin header values (e.g.
// ["https://app.example.com"]). Pass nil or an empty slice to allow all origins.
// When non-empty, requests with an Origin header not in the list are rejected with
// 403 Forbidden. Server-to-server requests without an Origin header are always allowed.
func NewStreamableHTTPTransport(mountPath string, allowedOrigins []string) *StreamableHTTPTransport {
	if isDebugMode() {
		log.Infof("[StreamableHTTP] Creating new transport at %s", mountPath)
		if len(allowedOrigins) > 0 {
			log.Infof("[StreamableHTTP] Origin allowlist: %v", allowedOrigins)
		} else {
			log.Infof("[StreamableHTTP] No Origin allowlist configured — all origins permitted")
		}
	}
	return &StreamableHTTPTransport{
		mountPath:      mountPath,
		allowedOrigins: allowedOrigins,
		handlers:       make(map[string]MessageHandler),
		requestAuths:   make(map[string]string),
		sessions:       make(map[string]*streamableSession),
	}
}

// RegisterHandler registers a handler for a specific MCP method.
func (s *StreamableHTTPTransport) RegisterHandler(method string, handler MessageHandler) {
	s.hMu.Lock()
	defer s.hMu.Unlock()
	s.handlers[method] = handler
	if isDebugMode() {
		log.Printf("[StreamableHTTP] Registered handler for method: %s", method)
	}
}

// isOriginAllowed checks whether the given Origin value is permitted.
// Returns true if the allowlist is empty (all origins allowed) or the origin is in the list.
// An empty origin string (absent header) is always allowed — it indicates a server-to-server call.
func (s *StreamableHTTPTransport) isOriginAllowed(origin string) bool {
	if origin == "" {
		// No Origin header: server-to-server call (e.g. curl, Node.js) — always permit.
		return true
	}
	if len(s.allowedOrigins) == 0 {
		// No allowlist configured — allow all origins.
		return true
	}
	for _, allowed := range s.allowedOrigins {
		if allowed == origin {
			return true
		}
	}
	return false
}

// corsOriginHeader returns the value to use for the Access-Control-Allow-Origin response header.
// Echoes the request origin back when it is in the allowlist; falls back to "*" when there is
// no allowlist (open access) or origin is absent.
func (s *StreamableHTTPTransport) corsOriginHeader(requestOrigin string) string {
	if requestOrigin != "" && len(s.allowedOrigins) > 0 {
		return requestOrigin
	}
	return "*"
}

// isNotification reports whether an MCPMessage is a JSON-RPC notification or response (i.e.
// has no id field). Per MCP spec 2025-03-26, pure notifications/responses must receive a
// 202 Accepted with no body rather than a JSON-RPC response.
func isNotification(msg *types.MCPMessage) bool {
	return len(msg.ID) == 0 || string(msg.ID) == "null"
}

// HandleConnection handles GET requests for server-initiated SSE notifications (optional).
// Clients that need push notifications (e.g. tools/listChanged) should connect with an
// Mcp-Session-Id header. Basic request-response usage does not require this endpoint.
func (s *StreamableHTTPTransport) HandleConnection(c *gin.Context) {
	// --- Origin validation (DNS rebinding protection, MCP spec 2025-03-26 §Security) ---
	origin := c.Request.Header.Get("Origin")
	if !s.isOriginAllowed(origin) {
		log.Warnf("[StreamableHTTP] Rejected GET from disallowed origin: %q", origin)
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "Origin not allowed"})
		return
	}

	sessionID := c.GetHeader("Mcp-Session-Id")
	if sessionID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Missing Mcp-Session-Id header"})
		return
	}

	if isDebugMode() {
		log.Printf("[StreamableHTTP] SSE session opened: %s", sessionID)
	}

	h := c.Writer.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	// Echo back the validated origin rather than broadcasting "*".
	h.Set("Access-Control-Allow-Origin", s.corsOriginHeader(origin))

	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Streaming not supported"})
		return
	}

	msgChan := make(chan *types.MCPMessage, 100)
	sess := &streamableSession{Channel: msgChan}

	s.sMu.Lock()
	s.sessions[sessionID] = sess
	s.sMu.Unlock()

	defer func() {
		s.sMu.Lock()
		delete(s.sessions, sessionID)
		s.sMu.Unlock()
		close(msgChan)
		if isDebugMode() {
			log.Printf("[StreamableHTTP] SSE session closed: %s", sessionID)
		}
	}()

	ticker := time.NewTicker(keepAliveInterval)
	defer ticker.Stop()

	for {
		select {
		case <-c.Request.Context().Done():
			return
		case <-ticker.C:
			fmt.Fprintf(c.Writer, ": ping\n\n")
			flusher.Flush()
		case msg, ok := <-msgChan:
			if !ok {
				return
			}
			data, err := json.Marshal(msg)
			if err != nil {
				log.Errorf("[StreamableHTTP] Failed to marshal notification: %v", err)
				continue
			}
			fmt.Fprintf(c.Writer, "event: message\ndata: %s\n\n", string(data))
			flusher.Flush()
		}
	}
}

// HandleMessage processes POST requests containing JSON-RPC messages.
//
// Per MCP spec 2025-03-26:
//   - If the message is a JSON-RPC request (has an id), the handler is invoked and the
//     JSON-RPC response is returned directly in the HTTP body (Content-Type: application/json).
//   - If the message is a JSON-RPC notification or response (no id), 202 Accepted is returned
//     with no body.
//
// No prior SSE connection is needed — every POST is handled independently.
func (s *StreamableHTTPTransport) HandleMessage(c *gin.Context) {
	// --- Origin validation (DNS rebinding protection, MCP spec 2025-03-26 §Security) ---
	origin := c.Request.Header.Get("Origin")
	if !s.isOriginAllowed(origin) {
		log.Warnf("[StreamableHTTP] Rejected POST from disallowed origin: %q", origin)
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "Origin not allowed"})
		return
	}

	// Assign a short-lived request ID to support auth header forwarding.
	requestID := uuid.New().String()
	authHeader := c.Request.Header.Get("Authorization")

	s.rMu.Lock()
	s.requestAuths[requestID] = authHeader
	s.rMu.Unlock()
	defer func() {
		s.rMu.Lock()
		delete(s.requestAuths, requestID)
		s.rMu.Unlock()
	}()

	var reqMsg types.MCPMessage
	if err := c.ShouldBindJSON(&reqMsg); err != nil {
		log.Errorf("[StreamableHTTP] Failed to parse message: %v", err)
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("Invalid message format: %v", err)})
		return
	}

	if isDebugMode() {
		log.Printf("[StreamableHTTP] Received method=%s id=%v", reqMsg.Method, reqMsg.ID)
	}

	// Per MCP spec 2025-03-26: notifications and responses (no id) must receive 202 Accepted
	// with no body — they do not expect a JSON-RPC reply.
	if isNotification(&reqMsg) {
		if isDebugMode() {
			log.Printf("[StreamableHTTP] Notification received (method=%s), returning 202", reqMsg.Method)
		}
		c.Status(http.StatusAccepted)
		return
	}

	// Inject the request ID so that executeToolLogic can retrieve the auth header
	// via GetAuthHeader(requestID) when ForwardAuthHeaders is enabled.
	if reqMsg.Params == nil {
		reqMsg.Params = map[string]interface{}{}
	}
	if paramsMap, ok := reqMsg.Params.(map[string]interface{}); ok {
		paramsMap["_mcpConnectionID"] = requestID
	}

	s.hMu.RLock()
	handler, found := s.handlers[reqMsg.Method]
	s.hMu.RUnlock()

	if !found {
		if isDebugMode() {
			log.Printf("[StreamableHTTP] No handler for method: %s", reqMsg.Method)
		}
		c.JSON(http.StatusOK, &types.MCPMessage{
			Jsonrpc: "2.0",
			ID:      reqMsg.ID,
			Error: map[string]interface{}{
				"code":    -32601,
				"message": fmt.Sprintf("Method '%s' not found", reqMsg.Method),
			},
		})
		return
	}

	respMsg := handler(c, &reqMsg)
	c.JSON(http.StatusOK, respMsg)
}

// GetAuthHeader returns the Authorization header captured during the current POST request.
// requestID is the UUID injected as _mcpConnectionID by HandleMessage.
func (s *StreamableHTTPTransport) GetAuthHeader(requestID string) string {
	s.rMu.RLock()
	defer s.rMu.RUnlock()
	return s.requestAuths[requestID]
}

// NotifyToolsChanged sends a tools/listChanged notification to all open SSE sessions.
func (s *StreamableHTTPTransport) NotifyToolsChanged() {
	notification := &types.MCPMessage{
		Jsonrpc: "2.0",
		Method:  "notifications/tools/listChanged",
	}

	s.sMu.RLock()
	sessions := make([]*streamableSession, 0, len(s.sessions))
	for _, sess := range s.sessions {
		sessions = append(sessions, sess)
	}
	s.sMu.RUnlock()

	if isDebugMode() {
		log.Printf("[StreamableHTTP] Notifying %d SSE sessions about tools change", len(sessions))
	}

	for _, sess := range sessions {
		select {
		case sess.Channel <- notification:
		case <-time.After(2 * time.Second):
		}
	}
}
