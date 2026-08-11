package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/ckanthony/gin-mcp/pkg/convert"
	"github.com/ckanthony/gin-mcp/pkg/transport"
	"github.com/ckanthony/gin-mcp/pkg/types"

	log "github.com/sirupsen/logrus"
)

// isDebugMode returns true if Gin is in debug mode
func isDebugMode() bool {
	return gin.Mode() == gin.DebugMode
}

// GinMCP represents the MCP server configuration for a Gin application
type GinMCP struct {
	engine            *gin.Engine
	name              string
	description       string
	baseURL           string
	serverVersion     string
	tools             []types.Tool
	operations        map[string]types.Operation
	transport         transport.Transport
	config            *Config
	registeredSchemas map[string]types.RegisteredSchemaInfo
	schemasMu         sync.RWMutex
	// executeToolFunc holds the function used to execute a tool.
	// It defaults to defaultExecuteTool but can be overridden for testing.
	executeToolFunc func(ctx context.Context, operationID string, parameters map[string]interface{}) (interface{}, error)
}

// TransportType selects the MCP transport protocol.
type TransportType string

const (
	// TransportTypeSSE uses the original SSE-based transport (MCP spec 2024-11-05).
	// Requires a persistent GET SSE connection before POSTing messages, which means
	// load balancers must route both requests to the same pod (session affinity needed).
	TransportTypeSSE TransportType = "sse"

	// TransportTypeStreamableHTTP uses the modern Streamable HTTP transport (MCP spec 2025-03-26).
	// Each POST is handled independently and the JSON-RPC response is returned directly
	// in the HTTP body. No persistent connection or pod affinity is required, making this
	// the recommended choice for horizontally-scaled deployments.
	TransportTypeStreamableHTTP TransportType = "streamable-http"
)

// MCP protocol versions supported by this server.
const (
	ProtocolVersion20241105 = "2024-11-05" // legacy HTTP+SSE, deprecated
	ProtocolVersion20250326 = "2025-03-26"
	ProtocolVersion20250618 = "2025-06-18"
	ProtocolVersion20251125 = "2025-11-25" // last version with an initialize handshake
	ProtocolVersion20260728 = "2026-07-28" // current, stateless
)

const (
	defaultServerVersion = "0.1.0"
	defaultCacheTTLMs    = 60000
	defaultCacheScope    = "public"
)

// Config represents the configuration options for GinMCP
type Config struct {
	Name              string
	Description       string
	BaseURL           string
	IncludeOperations []string
	ExcludeOperations []string
	IncludeTags       []string
	ExcludeTags       []string
	// ForwardAuthHeaders enables forwarding of Authorization headers from MCP requests
	// to internal tool execution HTTP calls. When enabled, the Authorization header from
	// the connection is captured and included in requests to tool endpoints.
	// Default: false (for backward compatibility)
	ForwardAuthHeaders bool
	// TransportType selects the MCP transport protocol.
	// Default: TransportTypeStreamableHTTP (breaking change from earlier releases which
	// defaulted to TransportTypeSSE). Set TransportTypeSSE explicitly to opt back into
	// the legacy SSE transport.
	TransportType TransportType
	// AllowedOrigins is an optional list of permitted Origin header values for
	// Streamable HTTP connections (e.g. ["https://app.example.com"]).
	// When non-empty, requests carrying an Origin header not in this list are rejected
	// with 403 Forbidden, preventing DNS rebinding attacks (MCP spec 2025-03-26 §Security).
	// Server-to-server requests without an Origin header are always allowed.
	// Default: nil (all origins permitted — rely on authentication for access control).
	AllowedOrigins []string
	// ProtocolVersion pins the server to a single MCP protocol version. Empty means all
	// versions supported by the selected transport.
	ProtocolVersion string
	// ServerVersion is reported in serverInfo. Defaults to "0.1.0".
	ServerVersion string
	// CacheTTLMs is the tools/list cache lifetime advertised for protocol version
	// 2026-07-28. Defaults to 60000.
	CacheTTLMs int
	// CacheScope is the tools/list cache scope advertised for protocol version
	// 2026-07-28. Defaults to "public".
	CacheScope string
}

// New creates a new GinMCP instance
func New(engine *gin.Engine, config *Config) *GinMCP {
	if config == nil {
		config = &Config{
			Name:        "Gin MCP",
			Description: "MCP server for Gin application",
		}
	}
	if config.TransportType == "" {
		config.TransportType = TransportTypeStreamableHTTP
	}
	if config.ServerVersion == "" {
		config.ServerVersion = defaultServerVersion
	}
	if config.CacheTTLMs == 0 {
		config.CacheTTLMs = defaultCacheTTLMs
	}
	if config.CacheScope == "" {
		config.CacheScope = defaultCacheScope
	}

	m := &GinMCP{
		engine:            engine,
		name:              config.Name,
		description:       config.Description,
		baseURL:           config.BaseURL,
		serverVersion:     config.ServerVersion,
		operations:        make(map[string]types.Operation),
		config:            config,
		registeredSchemas: make(map[string]types.RegisteredSchemaInfo),
	}

	m.executeToolFunc = m.defaultExecuteTool // Initialize with the default implementation

	// Add debug logging middleware
	if isDebugMode() {
		engine.Use(func(c *gin.Context) {
			start := time.Now()
			path := c.Request.URL.Path

			log.Printf("[HTTP Request] %s %s (Start)", c.Request.Method, path)
			c.Next()
			log.Printf("[HTTP Request] %s %s completed with status %d in %v",
				c.Request.Method, path, c.Writer.Status(), time.Since(start))
		})
	}

	return m
}

// SetExecuteToolFunc allows overriding the default tool execution function.
// This is useful for implementing dynamic baseURL resolution or custom execution logic.
func (m *GinMCP) SetExecuteToolFunc(fn func(ctx context.Context, operationID string, parameters map[string]interface{}) (interface{}, error)) {
	m.executeToolFunc = fn
}

// RegisterSchema associates Go struct types with a specific route for automatic schema generation.
// Provide nil if a type (Query or Body) is not applicable for the route.
// Example: mcp.RegisterSchema("POST", "/items", nil, main.Item{})
func (m *GinMCP) RegisterSchema(method string, path string, queryType interface{}, bodyType interface{}) {
	m.schemasMu.Lock()
	defer m.schemasMu.Unlock()

	// Ensure method is uppercase for canonical key
	method = strings.ToUpper(method)
	schemaKey := fmt.Sprintf("%s %s", method, path)

	// Validate types slightly (ensure they are structs or pointers to structs if not nil)
	if queryType != nil {
		queryVal := reflect.ValueOf(queryType)
		if queryVal.Kind() == reflect.Ptr {
			queryVal = queryVal.Elem()
		}
		if queryVal.Kind() != reflect.Struct {
			if isDebugMode() {
				log.Printf("Warning: RegisterSchema queryType for %s is not a struct or pointer to struct, reflection might fail.", schemaKey)
			}
		}
	}
	if bodyType != nil {
		bodyVal := reflect.ValueOf(bodyType)
		if bodyVal.Kind() == reflect.Ptr {
			bodyVal = bodyVal.Elem()
		}
		if bodyVal.Kind() != reflect.Struct {
			if isDebugMode() {
				log.Printf("Warning: RegisterSchema bodyType for %s is not a struct or pointer to struct, reflection might fail.", schemaKey)
			}
		}
	}

	m.registeredSchemas[schemaKey] = types.RegisteredSchemaInfo{
		QueryType: queryType,
		BodyType:  bodyType,
	}
	if isDebugMode() {
		log.Printf("Registered schema types for route: %s", schemaKey)
	}
}

// Mount sets up the MCP routes on the given path
func (m *GinMCP) Mount(mountPath string) {
	if mountPath == "" {
		mountPath = "/mcp"
	}

	// 1. Setup tools
	if err := m.SetupServer(); err != nil {
		if isDebugMode() {
			log.Printf("Failed to setup server: %v", err)
		}
		return
	}

	// 2. Create transport and register handlers
	if m.config.TransportType == TransportTypeStreamableHTTP {
		m.transport = transport.NewStreamableHTTPTransport(mountPath, m.config.AllowedOrigins, m.supportedProtocolVersions())
	} else {
		m.transport = transport.NewSSETransport(mountPath)
	}
	m.transport.RegisterHandler("initialize", m.handleInitialize)
	m.transport.RegisterHandler("tools/list", m.handleToolsList)
	m.transport.RegisterHandler("tools/call", m.handleToolCall)
	m.transport.RegisterHandler("logging/setLevel", m.handleLoggingSetLevel)
	m.transport.RegisterHandler("server/discover", m.handleServerDiscover)

	// 3. Setup CORS middleware
	m.engine.Use(func(c *gin.Context) {
		if isDebugMode() {
			log.Printf("[Middleware] Processing request: Method=%s, Path=%s, RemoteAddr=%s", c.Request.Method, c.Request.URL.Path, c.Request.RemoteAddr)
		}

		if strings.HasPrefix(c.Request.URL.Path, mountPath) {
			if isDebugMode() {
				log.Printf("[Middleware] Path %s matches mountPath %s. Applying headers.", c.Request.URL.Path, mountPath)
			}
			c.Header("Access-Control-Allow-Origin", "*")
			c.Header("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			c.Header("Access-Control-Allow-Headers", "Content-Type, Content-Length, Accept-Encoding, X-CSRF-Token, Authorization, accept, origin, Cache-Control, X-Requested-With, X-Connection-ID, MCP-Protocol-Version, Mcp-Method, Mcp-Name")
			c.Header("Access-Control-Expose-Headers", "X-Connection-ID, MCP-Protocol-Version")

			if c.Request.Method == "OPTIONS" {
				if isDebugMode() {
					log.Printf("[Middleware] OPTIONS request for %s. Aborting with 204.", c.Request.URL.Path)
				}
				c.AbortWithStatus(204)
				return
			} else if c.Request.Method == "POST" {
				if isDebugMode() {
					log.Printf("[Middleware] POST request for %s. Proceeding to handler.", c.Request.URL.Path)
				}
			}
		} else {
			if isDebugMode() {
				log.Printf("[Middleware] Path %s does NOT match mountPath %s. Skipping custom logic.", c.Request.URL.Path, mountPath)
			}
		}
		c.Next() // Ensure processing continues
		if isDebugMode() {
			log.Printf("[Middleware] Finished processing request: Method=%s, Path=%s, Status=%d", c.Request.Method, c.Request.URL.Path, c.Writer.Status())
		}
	})

	// 4. Setup endpoints
	if isDebugMode() {
		log.Printf("[Server Mount DEBUG] Defining GET %s route", mountPath)
	}
	m.engine.GET(mountPath, m.handleMCPConnection)
	if isDebugMode() {
		log.Printf("[Server Mount DEBUG] Defining POST %s route", mountPath)
	}
	m.engine.POST(mountPath, func(c *gin.Context) {
		m.transport.HandleMessage(c)
	})
}

// handleMCPConnection handles a new MCP connection request
func (m *GinMCP) handleMCPConnection(c *gin.Context) {
	if isDebugMode() {
		log.Println("[Server DEBUG] handleMCPConnection invoked for GET /mcp")
	}
	// 1. Ensure server is ready
	if len(m.tools) == 0 {
		if err := m.SetupServer(); err != nil {
			errID := fmt.Sprintf("err-%d", time.Now().UnixNano())
			c.JSON(http.StatusInternalServerError, &types.MCPMessage{
				Jsonrpc: "2.0",
				ID:      types.RawMessage([]byte(`"` + errID + `"`)),
				Result: map[string]interface{}{
					"code":    "server_error",
					"message": fmt.Sprintf("Failed to setup server: %v", err),
				},
			})
			return
		}
	}

	// 2. Let transport handle the SSE connection
	m.transport.HandleConnection(c)
}

// supportedProtocolVersions returns the MCP protocol versions this server accepts.
// If Config.ProtocolVersion is set, only that pinned version is supported. Otherwise the
// Streamable HTTP transport supports 2026-07-28 down to 2025-03-26 (newest first), and
// the SSE transport supports only legacy 2024-11-05.
func (m *GinMCP) supportedProtocolVersions() []string {
	if m.config.ProtocolVersion != "" {
		return []string{m.config.ProtocolVersion}
	}
	if m.config.TransportType == TransportTypeSSE {
		return []string{ProtocolVersion20241105}
	}
	return []string{
		ProtocolVersion20260728,
		ProtocolVersion20251125,
		ProtocolVersion20250618,
		ProtocolVersion20250326,
	}
}

// capabilities returns the server capabilities object. Only tools are advertised; there
// are no resource, prompt, or logging handlers.
func (m *GinMCP) capabilities() map[string]interface{} {
	return map[string]interface{}{
		"tools": map[string]interface{}{
			"listChanged": false,
		},
	}
}

// serverInfo returns the server identification object.
func (m *GinMCP) serverInfo() map[string]interface{} {
	return map[string]interface{}{
		"name":    m.name,
		"version": m.serverVersion,
	}
}

// requestProtocolVersion extracts the per-request protocol version from
// params._meta["io.modelcontextprotocol/protocolVersion"]. The transport has already
// enforced header/_meta equality, so handlers can trust this value.
func requestProtocolVersion(msg *types.MCPMessage) string {
	params, ok := msg.Params.(map[string]interface{})
	if !ok {
		return ""
	}
	meta, ok := params["_meta"].(map[string]interface{})
	if !ok {
		return ""
	}
	version, _ := meta["io.modelcontextprotocol/protocolVersion"].(string)
	return version
}

// handleInitialize handles the legacy initialize handshake. Protocol version 2026-07-28
// is stateless and has no initialize handshake, so it is never negotiated here: if the
// client-requested version is supported and initialize-capable it is echoed, otherwise
// the newest initialize-capable supported version is returned.
func (m *GinMCP) handleInitialize(_ context.Context, msg *types.MCPMessage) *types.MCPMessage {
	// Parse initialization parameters
	params, ok := msg.Params.(map[string]interface{})
	if !ok {
		return &types.MCPMessage{
			Jsonrpc: "2.0",
			ID:      msg.ID,
			Error: map[string]interface{}{
				"code":    -32602,
				"message": "Invalid parameters format",
			},
		}
	}

	// Log initialization request
	if isDebugMode() {
		log.Printf("Received initialize request with params: %+v", params)
	}

	supported := m.supportedProtocolVersions()
	requested, _ := params["protocolVersion"].(string)

	chosen := ""
	for _, v := range supported {
		if v == requested && v != ProtocolVersion20260728 {
			chosen = v
			break
		}
	}
	if chosen == "" {
		for _, v := range supported {
			if v != ProtocolVersion20260728 {
				chosen = v
				break
			}
		}
	}
	if chosen == "" {
		// Only 2026-07-28 is supported (pinned); fall back to the last
		// initialize-capable version so legacy clients still get a handshake.
		chosen = ProtocolVersion20251125
	}

	result := map[string]interface{}{
		"protocolVersion": chosen,
		"capabilities":    m.capabilities(),
		"serverInfo":      m.serverInfo(),
	}
	if m.description != "" {
		result["instructions"] = m.description
	}

	return &types.MCPMessage{
		Jsonrpc: "2.0",
		ID:      msg.ID,
		Result:  result,
	}
}

// handleServerDiscover handles the server/discover RPC: up-front discovery of supported
// protocol versions, capabilities, and server info without an initialize handshake.
func (m *GinMCP) handleServerDiscover(_ context.Context, msg *types.MCPMessage) *types.MCPMessage {
	result := map[string]interface{}{
		"resultType":        "complete",
		"supportedVersions": m.supportedProtocolVersions(),
		"capabilities":      m.capabilities(),
		"_meta": map[string]interface{}{
			"io.modelcontextprotocol/serverInfo": m.serverInfo(),
		},
	}
	if m.description != "" {
		result["instructions"] = m.description
	}
	return &types.MCPMessage{
		Jsonrpc: "2.0",
		ID:      msg.ID,
		Result:  result,
	}
}

// handleToolsList handles the tools/list request
func (m *GinMCP) handleToolsList(_ context.Context, msg *types.MCPMessage) *types.MCPMessage {
	// Ensure server is ready
	if err := m.SetupServer(); err != nil {
		return &types.MCPMessage{
			Jsonrpc: "2.0",
			ID:      msg.ID,
			Error: map[string]interface{}{
				"code":    -32603,
				"message": fmt.Sprintf("Failed to setup server: %v", err),
			},
		}
	}

	// Return tools list with proper format
	result := map[string]interface{}{
		"tools": m.tools,
		"metadata": map[string]interface{}{
			"version": "2024-11-05",
			"count":   len(m.tools),
		},
	}

	// Protocol version 2026-07-28 adds completion metadata, cache hints, and server info.
	if requestProtocolVersion(msg) == ProtocolVersion20260728 {
		result["resultType"] = "complete"
		result["ttlMs"] = m.config.CacheTTLMs
		result["cacheScope"] = m.config.CacheScope
		result["_meta"] = map[string]interface{}{
			"io.modelcontextprotocol/serverInfo": m.serverInfo(),
		}
	}

	return &types.MCPMessage{
		Jsonrpc: "2.0",
		ID:      msg.ID,
		Result:  result,
	}
}

// handleLoggingSetLevel handles the logging/setLevel request.
// gin-mcp does not implement log streaming, so this is a no-op that satisfies
// MCP clients (e.g. Claude Inspector) that send this method on startup.
func (m *GinMCP) handleLoggingSetLevel(_ context.Context, msg *types.MCPMessage) *types.MCPMessage {
	return &types.MCPMessage{
		Jsonrpc: "2.0",
		ID:      msg.ID,
		Result:  map[string]interface{}{},
	}
}

// handleToolCall handles the tools/call request
func (m *GinMCP) handleToolCall(ctx context.Context, msg *types.MCPMessage) *types.MCPMessage {
	// Parse parameters from the incoming MCP message
	reqParams, ok := msg.Params.(map[string]interface{})
	if !ok {
		return &types.MCPMessage{
			Jsonrpc: "2.0", ID: msg.ID,
			Error: map[string]interface{}{"code": -32602, "message": "Invalid parameters format"},
		}
	}

	// Extract connection ID from params (injected by transport layer)
	var connID string
	if connIDVal, exists := reqParams["_mcpConnectionID"]; exists {
		if connIDStr, ok := connIDVal.(string); ok {
			connID = connIDStr
			// Remove it from params so it doesn't get passed to tool execution
			delete(reqParams, "_mcpConnectionID")
		}
	}

	// Get tool name and arguments from the params
	toolName, nameOk := reqParams["name"].(string)
	// The actual arguments passed by the LLM are nested under "arguments"
	toolArgs, argsOk := reqParams["arguments"].(map[string]interface{})
	if !nameOk || !argsOk {
		return &types.MCPMessage{
			Jsonrpc: "2.0", ID: msg.ID,
			Error: map[string]interface{}{"code": -32602, "message": "Missing tool name or arguments"},
		}
	}

	// Inject connection ID into toolArgs for auth forwarding (if enabled)
	if connID != "" && m.config.ForwardAuthHeaders {
		if toolArgs == nil {
			toolArgs = make(map[string]interface{})
		}
		toolArgs["_mcpConnectionID"] = connID
	}

	// *** Add check for tool existence BEFORE executing ***
	if _, exists := m.operations[toolName]; !exists {
		if isDebugMode() {
			log.Printf("Error: Tool '%s' not found in operations map.", toolName)
		}
		return &types.MCPMessage{
			Jsonrpc: "2.0",
			ID:      msg.ID,
			Error: map[string]interface{}{
				"code":    -32601, // Method not found
				"message": fmt.Sprintf("Tool '%s' not found", toolName),
			},
		}
	}

	if isDebugMode() {
		log.Printf("Handling tool call: %s with args: %v", toolName, toolArgs)
	}

	// Execute the actual Gin endpoint via internal HTTP call
	execResult, err := m.executeToolFunc(ctx, toolName, toolArgs) // Use the function field
	if err != nil {
		// Handle execution error
		return &types.MCPMessage{
			Jsonrpc: "2.0",
			ID:      msg.ID,
			Error: map[string]interface{}{
				"code":    -32603, // Internal error
				"message": fmt.Sprintf("Error executing tool '%s': %v", toolName, err),
			},
		}
	}

	// Convert execResult to JSON string for the content field
	resultBytes, err := json.Marshal(execResult)
	if err != nil {
		// Handle potential marshalling error if execResult is complex/invalid
		return &types.MCPMessage{
			Jsonrpc: "2.0",
			ID:      msg.ID,
			Error: map[string]interface{}{ // Use appropriate error code
				"code":    -32603, // Internal error
				"message": fmt.Sprintf("Failed to marshal tool execution result: %v", err),
			},
		}
	}

	// Construct the success response using the expected content structure
	result := map[string]interface{}{ // Standard MCP result wrapper
		"content": []map[string]interface{}{ // Content is an array
			{
				"type": string(types.ContentTypeText), // Assuming text response
				"text": string(resultBytes),           // Actual result as JSON string
			},
		},
		// Add other potential fields like isError=false if needed by spec/client
		// "isError": false,
	}

	// Protocol version 2026-07-28 adds completion metadata and server info.
	if requestProtocolVersion(msg) == ProtocolVersion20260728 {
		result["resultType"] = "complete"
		result["_meta"] = map[string]interface{}{
			"io.modelcontextprotocol/serverInfo": m.serverInfo(),
		}
	}

	return &types.MCPMessage{
		Jsonrpc: "2.0",
		ID:      msg.ID,
		Result:  result,
	}
}

// SetupServer initializes the MCP server by discovering routes and converting them to tools
func (m *GinMCP) SetupServer() error {
	if len(m.tools) == 0 {
		// Get all routes from the Gin engine
		routes := m.engine.Routes()

		// Lock schema map while converting
		m.schemasMu.RLock()
		// Convert routes to tools with registered types
		newTools, operations := convert.ConvertRoutesToTools(routes, m.registeredSchemas)
		m.schemasMu.RUnlock()

		// Check if tools have changed
		toolsChanged := m.haveToolsChanged(newTools)

		// Update tools and operations
		m.tools = newTools
		m.operations = operations

		// Filter tools based on configuration (operation/tag filters)
		m.filterTools()

		// Notify clients if tools have changed
		if toolsChanged && m.transport != nil {
			m.transport.NotifyToolsChanged()
		}
	}

	return nil
}

// haveToolsChanged checks if the tools list has changed
func (m *GinMCP) haveToolsChanged(newTools []types.Tool) bool {
	if len(m.tools) != len(newTools) {
		return true
	}

	// Create maps for easier comparison
	oldToolMap := make(map[string]types.Tool)
	for _, tool := range m.tools {
		oldToolMap[tool.Name] = tool
	}

	// Compare tools
	for _, newTool := range newTools {
		oldTool, exists := oldToolMap[newTool.Name]
		if !exists {
			return true
		}
		// Compare tool definitions (you might want to add more detailed comparison)
		if oldTool.Description != newTool.Description {
			return true
		}
	}

	return false
}

// filterTools filters the tools based on configuration
func (m *GinMCP) filterTools() {
	if len(m.tools) == 0 {
		return
	}

	var filteredTools []types.Tool
	config := m.config // Use the GinMCP config

	// Work with local copies to avoid mutating the caller's config
	includeOps := config.IncludeOperations
	includeTags := config.IncludeTags
	excludeOps := config.ExcludeOperations
	excludeTags := config.ExcludeTags

	// Check for conflicting inclusion filters (prefer operations over tags)
	if len(includeOps) > 0 && len(includeTags) > 0 {
		if isDebugMode() {
			log.Printf("Warning: Both IncludeOperations and IncludeTags are set. Preferring IncludeOperations.")
		}
		includeTags = nil
	}

	// Check for conflicting exclusion filters (prefer operations over tags)
	if len(excludeOps) > 0 && len(excludeTags) > 0 {
		if isDebugMode() {
			log.Printf("Warning: Both ExcludeOperations and ExcludeTags are set. Preferring ExcludeOperations.")
		}
		excludeTags = nil
	}

	// Step 1: Apply inclusion filters (operations take precedence over tags)
	if len(includeOps) > 0 {
		includeMap := make(map[string]bool)
		for _, op := range includeOps {
			includeMap[op] = true
		}
		for _, tool := range m.tools {
			if includeMap[tool.Name] {
				filteredTools = append(filteredTools, tool)
			}
		}
		m.tools = filteredTools
		filteredTools = []types.Tool{}
	} else if len(includeTags) > 0 {
		// Include tools that have at least one matching tag
		includeTagsMap := make(map[string]bool)
		for _, tag := range includeTags {
			includeTagsMap[tag] = true
		}
		for _, tool := range m.tools {
			if hasMatchingTag(tool.Tags, includeTagsMap) {
				filteredTools = append(filteredTools, tool)
			}
		}
		m.tools = filteredTools
		filteredTools = []types.Tool{}
	}

	// Step 2: Apply exclusion filters (operations take precedence over tags)
	// Exclusion always wins - it runs on the result of inclusion filtering
	if len(excludeOps) > 0 {
		excludeMap := make(map[string]bool)
		for _, op := range excludeOps {
			excludeMap[op] = true
		}
		for _, tool := range m.tools {
			if !excludeMap[tool.Name] {
				filteredTools = append(filteredTools, tool)
			}
		}
		m.tools = filteredTools
	} else if len(excludeTags) > 0 {
		// Exclude tools that have at least one matching tag
		excludeTagsMap := make(map[string]bool)
		for _, tag := range excludeTags {
			excludeTagsMap[tag] = true
		}
		for _, tool := range m.tools {
			if !hasMatchingTag(tool.Tags, excludeTagsMap) {
				filteredTools = append(filteredTools, tool)
			}
		}
		m.tools = filteredTools
	}
}

// hasMatchingTag checks if any tag in the toolTags slice exists in the filterTags map
func hasMatchingTag(toolTags []string, filterTags map[string]bool) bool {
	for _, tag := range toolTags {
		if filterTags[tag] {
			return true
		}
	}
	return false
}

// ExecuteToolWithDynamicURL executes a tool with a dynamically resolved baseURL.
// This is a helper function for implementing custom executeToolFunc with dynamic baseURL logic.
func (m *GinMCP) ExecuteToolWithDynamicURL(operationID string, parameters map[string]interface{}, baseURL string) (interface{}, error) {
	return m.executeToolWithBaseURL(operationID, parameters, baseURL)
}

// ExecuteToolWithResolver executes a tool using a baseURL resolver function.
// The resolver function can extract baseURL from headers, environment, or any other source.
func (m *GinMCP) ExecuteToolWithResolver(operationID string, parameters map[string]interface{}, resolver func() string) (interface{}, error) {
	baseURL := resolver()
	return m.executeToolWithBaseURL(operationID, parameters, baseURL)
}

// executeToolWithBaseURL is the internal implementation that accepts a specific baseURL
func (m *GinMCP) executeToolWithBaseURL(operationID string, parameters map[string]interface{}, baseURL string) (interface{}, error) {
	if isDebugMode() {
		log.Printf("[Tool Execution] Starting execution of tool '%s' with parameters: %+v, baseURL: %s", operationID, parameters, baseURL)
	}

	// Find the operation associated with the tool name (operationID)
	operation, ok := m.operations[operationID]
	if !ok {
		if isDebugMode() {
			log.Printf("Error: Operation details not found for tool '%s'", operationID)
		}
		return nil, fmt.Errorf("operation '%s' not found", operationID)
	}
	if isDebugMode() {
		log.Printf("[Tool Execution] Found operation for tool '%s': Method=%s, Path=%s", operationID, operation.Method, operation.Path)
	}

	// Continue with the existing tool execution logic using the provided baseURL
	return m.executeToolLogic(operation, parameters, baseURL)
}

// defaultExecuteTool is the default implementation for executing a tool.
// It handles the actual invocation of the underlying Gin handler using the configured baseURL.
func (m *GinMCP) defaultExecuteTool(ctx context.Context, operationID string, parameters map[string]interface{}) (interface{}, error) {
	if isDebugMode() {
		log.Printf("[Tool Execution] Starting execution of tool '%s' with parameters: %+v", operationID, parameters)
	}

	// Find the operation associated with the tool name (operationID)
	operation, ok := m.operations[operationID]
	if !ok {
		if isDebugMode() {
			log.Printf("Error: Operation details not found for tool '%s'", operationID)
		}
		return nil, fmt.Errorf("operation '%s' not found", operationID)
	}
	if isDebugMode() {
		log.Printf("[Tool Execution] Found operation for tool '%s': Method=%s, Path=%s", operationID, operation.Method, operation.Path)
	}

	// Use the configured baseURL (static)
	baseURL := m.baseURL
	if baseURL == "" {
		// Use relative URL if baseURL is not set
		baseURL = ""
		if isDebugMode() {
			log.Printf("[Tool Execution] Using relative URL for request")
		}
	}

	return m.executeToolLogic(operation, parameters, baseURL)
}

// executeToolLogic contains the core tool execution logic that can be reused
// with different baseURL resolution strategies
func (m *GinMCP) executeToolLogic(operation types.Operation, parameters map[string]interface{}, baseURL string) (interface{}, error) {

	// Extract connection ID for auth forwarding (if present)
	var authHeader string
	if m.config.ForwardAuthHeaders {
		if connIDVal, exists := parameters["_mcpConnectionID"]; exists {
			if connID, ok := connIDVal.(string); ok && connID != "" {
				// Get auth header from transport
				authHeader = m.transport.GetAuthHeader(connID)
				if isDebugMode() && authHeader != "" {
					log.Printf("[Tool Execution] Forwarding auth header for connection %s", connID)
				}
			}
			// Remove connection ID from parameters
			delete(parameters, "_mcpConnectionID")
		}
	}

	path := operation.Path
	queryParams := url.Values{}
	pathParams := make(map[string]string)

	// Separate args into path params, query params, and body
	for key, value := range parameters {
		// Check against Gin's format ":key"
		placeholder := ":" + key
		if strings.Contains(path, placeholder) {
			// Store the actual value for substitution later
			pathParams[key] = fmt.Sprintf("%v", value)
			if isDebugMode() {
				log.Printf("[Tool Execution] Found path parameter %s=%v", key, value)
			}
		} else {
			// Assume remaining args are query parameters for GET/DELETE
			if operation.Method == "GET" || operation.Method == "DELETE" {
				queryParams.Add(key, fmt.Sprintf("%v", value))
				if isDebugMode() {
					log.Printf("[Tool Execution] Added query parameter %s=%v", key, value)
				}
			}
		}
	}

	// Substitute path parameters using Gin's format ":key"
	for key, value := range pathParams {
		path = strings.Replace(path, ":"+key, value, -1)
	}

	targetURL := baseURL + path
	if len(queryParams) > 0 {
		targetURL += "?" + queryParams.Encode()
	}

	if isDebugMode() {
		log.Printf("[Tool Execution] Making request: %s %s", operation.Method, targetURL)
	}

	// 3. Create and execute the HTTP request
	var reqBody io.Reader
	if operation.Method == "POST" || operation.Method == "PUT" || operation.Method == "PATCH" {
		// For POST/PUT/PATCH, send all non-path args in the body
		bodyData := make(map[string]interface{})
		for key, value := range parameters {
			// Skip ID field for PUT requests since it's in the path
			if key == "id" && operation.Method == "PUT" {
				continue
			}
			if _, isPath := pathParams[key]; !isPath {
				bodyData[key] = value
				if isDebugMode() {
					log.Printf("[Tool Execution] Added body parameter %s=%v", key, value)
				}
			}
		}
		bodyBytes, err := json.Marshal(bodyData)
		if err != nil {
			if isDebugMode() {
				log.Printf("[Tool Execution] Error marshalling request body: %v", err)
			}
			return nil, err
		}
		reqBody = bytes.NewBuffer(bodyBytes)
		if isDebugMode() {
			log.Printf("[Tool Execution] Request body: %s", string(bodyBytes))
		}
	}

	req, err := http.NewRequest(operation.Method, targetURL, reqBody)
	if err != nil {
		if isDebugMode() {
			log.Printf("[Tool Execution] Error creating request: %v", err)
		}
		return nil, err
	}

	req.Header.Set("Accept", "application/json")
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	// Forward Authorization header if available
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
		if isDebugMode() {
			log.Printf("[Tool Execution] Forwarding Authorization header")
		}
	}

	if isDebugMode() {
		log.Printf("[Tool Execution] Sending request with headers: %+v", req.Header)
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		if isDebugMode() {
			log.Printf("[Tool Execution] Error executing request: %v", err)
		}
		return nil, err
	}
	defer resp.Body.Close()

	// 4. Read and parse the response
	bodyBytes, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		if isDebugMode() {
			log.Printf("[Tool Execution] Error reading response body: %v", err)
		}
		return nil, err
	}

	if isDebugMode() {
		log.Printf("[Tool Execution] Response status: %d, body: %s", resp.StatusCode, string(bodyBytes))
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if isDebugMode() {
			log.Printf("[Tool Execution] Request failed with status %d", resp.StatusCode)
		}
		// Attempt to return the error body, otherwise just the status
		var errorData interface{}
		if json.Unmarshal(bodyBytes, &errorData) == nil {
			return nil, fmt.Errorf("request failed with status %d: %v", resp.StatusCode, errorData)
		}
		return nil, fmt.Errorf("request failed with status %d", resp.StatusCode)
	}

	var resultData interface{}
	if err := json.Unmarshal(bodyBytes, &resultData); err != nil {
		if isDebugMode() {
			log.Printf("[Tool Execution] Error unmarshalling response: %v", err)
		}
		// Return raw body if unmarshalling fails but status was ok
		return string(bodyBytes), nil
	}

	if isDebugMode() {
		log.Printf("[Tool Execution] Successfully completed tool execution")
	}

	return resultData, nil
}

// BaseURLResolver is a function type for resolving baseURL dynamically
type BaseURLResolver func() string

// NewEnvironmentResolver creates a resolver that extracts baseURL from environment variables.
// Useful for Quicknode scenarios where the user endpoint is set via environment.
func NewEnvironmentResolver(envVarName string, fallback string) BaseURLResolver {
	return func() string {
		if value := os.Getenv(envVarName); value != "" {
			return value
		}
		return fallback
	}
}

// NewHeaderResolver creates a resolver that extracts baseURL from HTTP headers.
// This requires access to the current request context.
// Note: This is a template - you'll need to adapt this to your request handling pattern.
func NewHeaderResolver(headerName string, fallback string) BaseURLResolver {
	return func() string {
		// In a real implementation, you would need access to the current request context
		// This could be achieved through:
		// 1. Thread-local storage
		// 2. Context.Context passed through the call chain
		// 3. Middleware that sets a global variable

		// For now, return fallback - see example usage for complete implementation
		return fallback
	}
}

// NewQuicknodeResolver creates a resolver specifically for Quicknode environments.
// It tries multiple common patterns for extracting the user endpoint.
func NewQuicknodeResolver(fallback string) BaseURLResolver {
	return func() string {
		// Try Quicknode-specific environment variables
		if endpoint := os.Getenv("QUICKNODE_USER_ENDPOINT"); endpoint != "" {
			return endpoint
		}
		if endpoint := os.Getenv("USER_ENDPOINT"); endpoint != "" {
			return endpoint
		}
		if host := os.Getenv("HOST"); host != "" {
			if !strings.HasPrefix(host, "http") {
				return "https://" + host
			}
			return host
		}

		return fallback
	}
}

// NewRAGFlowResolver creates a resolver specifically for RAGFlow environments.
// It tries multiple common patterns for extracting the RAGFlow endpoint.
func NewRAGFlowResolver(fallback string) BaseURLResolver {
	return func() string {
		// Try RAGFlow-specific environment variables in order of preference
		if endpoint := os.Getenv("RAGFLOW_ENDPOINT"); endpoint != "" {
			return endpoint
		}
		if workflowURL := os.Getenv("RAGFLOW_WORKFLOW_URL"); workflowURL != "" {
			return workflowURL
		}

		// Try building from base URL and workflow ID
		baseURL := os.Getenv("RAGFLOW_BASE_URL")
		workflowID := os.Getenv("WORKFLOW_ID")
		if baseURL != "" && workflowID != "" {
			baseURL = strings.TrimSuffix(baseURL, "/")
			return baseURL + "/workflow/" + workflowID
		}

		// Try just base URL
		if baseURL != "" {
			return baseURL
		}

		// Try generic HOST variable
		if host := os.Getenv("HOST"); host != "" {
			if !strings.HasPrefix(host, "http") {
				return "https://" + host
			}
			return host
		}

		return fallback
	}
}
