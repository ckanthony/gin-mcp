package transport

import (
	"context"

	"github.com/ckanthony/gin-mcp/pkg/types"
	"github.com/gin-gonic/gin"
)

// MessageHandler defines the function signature for handling incoming MCP messages.
type MessageHandler func(ctx context.Context, msg *types.MCPMessage) *types.MCPMessage

// Transport defines the interface for handling MCP communication over different protocols.
type Transport interface {
	// RegisterHandler registers a handler function for a specific MCP method.
	RegisterHandler(method string, handler MessageHandler)

	// HandleConnection handles the initial connection setup (e.g., SSE).
	HandleConnection(c *gin.Context)

	// HandleMessage processes an incoming message received outside the main connection (e.g., via POST).
	HandleMessage(c *gin.Context)

	// NotifyToolsChanged sends a notification to connected clients that the tool list has changed.
	NotifyToolsChanged()

	// GetAuthHeader retrieves the Authorization header for a connection.
	GetAuthHeader(connID string) string
}
