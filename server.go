package server

import (
	"bytes"
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

	"github.com/dolfly/gin-mcp/pkg/convert"
	"github.com/dolfly/gin-mcp/pkg/transport"
	"github.com/dolfly/gin-mcp/pkg/types"

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
	tools             []types.Tool
	operations        map[string]types.Operation
	transport         transport.Transport
	config            *Config
	registeredSchemas map[string]types.RegisteredSchemaInfo
	schemasMu         sync.RWMutex
	// executeToolFunc holds the function used to execute a tool.
	// It defaults to defaultExecuteTool but can be overridden for testing.
	executeToolFunc func(operationID string, parameters map[string]interface{}) (interface{}, error)
}

// TransportType selects the MCP transport protocol.
type TransportType string

const (
	// ProtocolVersion20241105 is the MCP protocol version used by older SSE clients.
	ProtocolVersion20241105 = "2024-11-05"

	// ProtocolVersion20250326 introduced Streamable HTTP transport.
	ProtocolVersion20250326 = "2025-03-26"

	// ProtocolVersion20250618 is a post-Streamable HTTP MCP protocol revision.
	ProtocolVersion20250618 = "2025-06-18"

	// ProtocolVersion20251125 is the newest initialize-capable protocol supported here.
	ProtocolVersion20251125 = "2025-11-25"

	// ProtocolVersion20260728 is the current MCP 2.0-era protocol version.
	// It uses server/discover instead of the legacy initialize handshake.
	ProtocolVersion20260728 = "2026-07-28"

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
	// ForwardHeaders lists additional request headers to copy from the MCP request into
	// the internal tool execution request. Header names are matched case-insensitively.
	// Use ForwardAuthHeaders for Authorization; this slice is for extra headers.
	ForwardHeaders []string
	// TransportType selects the MCP transport protocol.
	// Default: TransportTypeSSE (for backward compatibility).
	// Use TransportTypeStreamableHTTP for horizontally-scaled deployments.
	TransportType TransportType
	// AllowedOrigins is an optional list of permitted Origin header values for
	// Streamable HTTP connections (e.g. ["https://app.example.com"]).
	// When non-empty, requests carrying an Origin header not in this list are rejected
	// with 403 Forbidden, preventing DNS rebinding attacks (MCP spec 2025-03-26 §Security).
	// Server-to-server requests without an Origin header are always allowed.
	// Default: nil (all origins permitted — rely on authentication for access control).
	AllowedOrigins []string
	// ProtocolVersion optionally pins the MCP protocol version this server advertises.
	// When empty, SSE defaults to 2024-11-05 and Streamable HTTP advertises the latest
	// supported version while still accepting older client versions.
	ProtocolVersion string
}

// New creates a new GinMCP instance
func New(engine *gin.Engine, config *Config) *GinMCP {
	if config == nil {
		config = &Config{
			Name:        "Gin MCP",
			Description: "MCP server for Gin application",
		}
	}

	m := &GinMCP{
		engine:            engine,
		name:              config.Name,
		description:       config.Description,
		baseURL:           config.BaseURL,
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

var defaultAllowedHeaders = []string{
	"Content-Type",
	"Content-Length",
	"Accept-Encoding",
	"X-CSRF-Token",
	"Authorization",
	"accept",
	"origin",
	"Cache-Control",
	"X-Requested-With",
	"X-Connection-ID",
	"MCP-Protocol-Version",
	"Mcp-Method",
	"Mcp-Name",
	"Mcp-Session-Id",
}

func mergeHeaderNames(base, extra []string) []string {
	seen := make(map[string]struct{}, len(base)+len(extra))
	merged := make([]string, 0, len(base)+len(extra))

	add := func(name string) {
		name = strings.TrimSpace(name)
		if name == "" {
			return
		}
		key := strings.ToLower(name)
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		merged = append(merged, name)
	}

	for _, name := range base {
		add(name)
	}
	for _, name := range extra {
		add(name)
	}

	return merged
}

func (m *GinMCP) shouldForwardHeaders() bool {
	return m.config != nil && (m.config.ForwardAuthHeaders || len(m.config.ForwardHeaders) > 0)
}

func (m *GinMCP) forwardHeaderNames() []string {
	if m.config == nil {
		return nil
	}

	headers := make([]string, 0, 1+len(m.config.ForwardHeaders))
	if m.config.ForwardAuthHeaders {
		headers = append(headers, "Authorization")
	}
	headers = append(headers, m.config.ForwardHeaders...)
	return headers
}

func (m *GinMCP) allowedRequestHeaders() []string {
	return mergeHeaderNames(defaultAllowedHeaders, m.forwardHeaderNames())
}

func (m *GinMCP) collectForwardHeaders(connID string) http.Header {
	headers := make(http.Header)
	if connID == "" || m.transport == nil || !m.shouldForwardHeaders() {
		return headers
	}

	for _, headerName := range m.forwardHeaderNames() {
		if value := m.transport.GetHeader(connID, headerName); value != "" {
			headers.Set(headerName, value)
		}
	}

	return headers
}

// SetExecuteToolFunc allows overriding the default tool execution function.
// This is useful for implementing dynamic baseURL resolution or custom execution logic.
func (m *GinMCP) SetExecuteToolFunc(fn func(operationID string, parameters map[string]interface{}) (interface{}, error)) {
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
		streamable := transport.NewStreamableHTTPTransport(mountPath, m.config.AllowedOrigins)
		streamable.SetSupportedProtocolVersions(m.supportedProtocolVersions())
		m.transport = streamable
	} else {
		m.transport = transport.NewSSETransport(mountPath, m.forwardHeaderNames()...)
	}
	m.transport.RegisterHandler("initialize", m.handleInitialize)
	m.transport.RegisterHandler("server/discover", m.handleServerDiscover)
	m.transport.RegisterHandler("tools/list", m.handleToolsList)
	m.transport.RegisterHandler("tools/call", m.handleToolCall)
	m.transport.RegisterHandler("logging/setLevel", m.handleLoggingSetLevel)

	// 3. Setup CORS middleware
	m.engine.Use(func(c *gin.Context) {
		if isDebugMode() {
			log.Printf("[Middleware] Processing request: Method=%s, Path=%s, RemoteAddr=%s", c.Request.Method, c.Request.URL.Path, c.Request.RemoteAddr)
		}

		if strings.HasPrefix(c.Request.URL.Path, mountPath) {
			if isDebugMode() {
				log.Printf("[Middleware] Path %s matches mountPath %s. Applying headers.", c.Request.URL.Path, mountPath)
			}
			allowedHeaders := strings.Join(m.allowedRequestHeaders(), ", ")
			c.Header("Access-Control-Allow-Origin", "*")
			c.Header("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			c.Header("Access-Control-Allow-Headers", allowedHeaders)
			c.Header("Access-Control-Expose-Headers", "X-Connection-ID")

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

func (m *GinMCP) supportedProtocolVersions() []string {
	if m.config != nil && strings.TrimSpace(m.config.ProtocolVersion) != "" {
		return []string{strings.TrimSpace(m.config.ProtocolVersion)}
	}
	if m.config != nil && m.config.TransportType == TransportTypeStreamableHTTP {
		return []string{
			ProtocolVersion20260728,
			ProtocolVersion20251125,
			ProtocolVersion20250618,
			ProtocolVersion20250326,
		}
	}
	return []string{ProtocolVersion20241105}
}

func (m *GinMCP) defaultProtocolVersion() string {
	versions := m.supportedProtocolVersions()
	if len(versions) == 0 {
		return ProtocolVersion20241105
	}
	return versions[0]
}

func (m *GinMCP) initializeProtocolVersion(clientVersion string) string {
	if clientVersion != "" {
		for _, version := range m.supportedProtocolVersions() {
			if version == clientVersion && version != ProtocolVersion20260728 {
				return version
			}
		}
	}
	for _, version := range m.supportedProtocolVersions() {
		if version != ProtocolVersion20260728 {
			return version
		}
	}
	return ProtocolVersion20251125
}

func (m *GinMCP) capabilities() map[string]interface{} {
	return map[string]interface{}{
		"tools": map[string]interface{}{
			"listChanged": false,
		},
		"resources": map[string]interface{}{},
		"prompts":   map[string]interface{}{},
		"logging":   map[string]interface{}{},
	}
}

func (m *GinMCP) serverInfo(protocolVersion string) map[string]interface{} {
	return map[string]interface{}{
		"name":       m.name,
		"version":    protocolVersion,
		"apiVersion": protocolVersion,
	}
}

// handleInitialize handles the initialize request from clients
func (m *GinMCP) handleInitialize(msg *types.MCPMessage) *types.MCPMessage {
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

	clientVersion, _ := params["protocolVersion"].(string)
	protocolVersion := m.initializeProtocolVersion(clientVersion)

	return &types.MCPMessage{
		Jsonrpc: "2.0",
		ID:      msg.ID,
		Result: map[string]interface{}{
			"protocolVersion": protocolVersion,
			"capabilities":    m.capabilities(),
			"serverInfo":      m.serverInfo(protocolVersion),
		},
	}
}

// handleServerDiscover handles MCP 2.0 server discovery requests.
func (m *GinMCP) handleServerDiscover(msg *types.MCPMessage) *types.MCPMessage {
	protocolVersion := m.defaultProtocolVersion()
	result := map[string]interface{}{
		"resultType":        "complete",
		"protocolVersion":   protocolVersion,
		"supportedVersions": m.supportedProtocolVersions(),
		"capabilities":      m.capabilities(),
		"serverInfo":        m.serverInfo(protocolVersion),
	}
	if strings.TrimSpace(m.description) != "" {
		result["instructions"] = m.description
	}

	return &types.MCPMessage{
		Jsonrpc: "2.0",
		ID:      msg.ID,
		Result:  result,
	}
}

// handleToolsList handles the tools/list request
func (m *GinMCP) handleToolsList(msg *types.MCPMessage) *types.MCPMessage {
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
	return &types.MCPMessage{
		Jsonrpc: "2.0",
		ID:      msg.ID,
		Result: map[string]interface{}{
			"resultType": "complete",
			"tools":      m.tools,
			"metadata": map[string]interface{}{
				"version": m.defaultProtocolVersion(),
				"count":   len(m.tools),
			},
		},
	}
}

// handleLoggingSetLevel handles the logging/setLevel request.
// gin-mcp does not implement log streaming, so this is a no-op that satisfies
// MCP clients (e.g. Claude Inspector) that send this method on startup.
func (m *GinMCP) handleLoggingSetLevel(msg *types.MCPMessage) *types.MCPMessage {
	return &types.MCPMessage{
		Jsonrpc: "2.0",
		ID:      msg.ID,
		Result:  map[string]interface{}{},
	}
}

// handleToolCall handles the tools/call request
func (m *GinMCP) handleToolCall(msg *types.MCPMessage) *types.MCPMessage {
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
	if connID != "" && m.shouldForwardHeaders() {
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
	execResult, err := m.executeToolFunc(toolName, toolArgs) // Use the function field
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
	return &types.MCPMessage{
		Jsonrpc: "2.0",
		ID:      msg.ID,
		Result: map[string]interface{}{ // Standard MCP result wrapper
			"resultType": "complete",
			"content": []map[string]interface{}{ // Content is an array
				{
					"type": string(types.ContentTypeText), // Assuming text response
					"text": string(resultBytes),           // Actual result as JSON string
				},
			},
			"isError": false,
		},
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
func (m *GinMCP) defaultExecuteTool(operationID string, parameters map[string]interface{}) (interface{}, error) {
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

	// Extract connection ID for header forwarding (if present)
	var forwardedHeaders http.Header
	if m.shouldForwardHeaders() {
		if connIDVal, exists := parameters["_mcpConnectionID"]; exists {
			if connID, ok := connIDVal.(string); ok && connID != "" {
				forwardedHeaders = m.collectForwardHeaders(connID)
				if isDebugMode() && len(forwardedHeaders) > 0 {
					log.Printf("[Tool Execution] Forwarding headers for connection %s: %+v", connID, forwardedHeaders)
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

	// Forward configured headers if available.
	for headerName, values := range forwardedHeaders {
		if strings.EqualFold(headerName, "Accept") || strings.EqualFold(headerName, "Content-Type") {
			continue
		}
		for _, value := range values {
			req.Header.Add(headerName, value)
		}
		if isDebugMode() {
			log.Printf("[Tool Execution] Forwarding %s header", headerName)
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
