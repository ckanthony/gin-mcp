package main

import (
	"os"

	server "github.com/ckanthony/gin-mcp"
	"github.com/gin-gonic/gin"
)

// ConfigureMCPForQuicknode demonstrates how to configure MCP for Quicknode scenarios
// where each user has their own dynamic endpoint proxied through Quicknode.
//
// Usage:
//
//	r := gin.Default()
//	registerRoutes(r)
//	configureMCPForQuicknode(r)  // Use this instead of configureMCP(r)
//	r.Run(":8080")
//
// Environment Variables:
//
//	QUICKNODE_USER_ENDPOINT - The user's specific endpoint (e.g., "https://user1.quicknode.endpoint.com")
//	USER_ENDPOINT           - Alternative environment variable name
//	HOST                    - Host without protocol (will add https://)
func configureMCPForQuicknode(r *gin.Engine) {
	mcp := server.New(r, &server.Config{
		Name:        "Gaming Store API",
		Description: "RESTful API for managing gaming products with dynamic Quicknode endpoints",
		// Note: BaseURL is not set here since we'll resolve it dynamically
	})

	// Register request schemas for MCP
	mcp.RegisterSchema("GET", "/products", ListProductsParams{}, nil)
	mcp.RegisterSchema("POST", "/products", nil, Product{})
	mcp.RegisterSchema("PUT", "/products/:id", nil, UpdateProductRequest{})

	// Setup dynamic baseURL resolution for Quicknode
	resolver := server.NewQuicknodeResolver("http://localhost:8080")

	// Override the default tool execution with dynamic baseURL logic
	mcp.SetExecuteToolFunc(func(c *gin.Context, operationID string, parameters map[string]interface{}) (interface{}, error) {
		return mcp.ExecuteToolWithResolver(operationID, parameters, resolver)
	})

	// Mount MCP endpoint
	mcp.Mount("/mcp")
}

// configureMCPForQuicknodeWithCustomEnv shows how to use a custom environment variable
func configureMCPForQuicknodeWithCustomEnv(r *gin.Engine) {
	mcp := server.New(r, &server.Config{
		Name:        "Gaming Store API",
		Description: "RESTful API for managing gaming products",
	})

	// Register request schemas for MCP
	mcp.RegisterSchema("GET", "/products", ListProductsParams{}, nil)
	mcp.RegisterSchema("POST", "/products", nil, Product{})
	mcp.RegisterSchema("PUT", "/products/:id", nil, UpdateProductRequest{})

	// Use environment-specific resolver with custom variable name
	envResolver := server.NewEnvironmentResolver("MY_QUICKNODE_ENDPOINT", "http://localhost:8080")
	mcp.SetExecuteToolFunc(func(c *gin.Context, operationID string, parameters map[string]interface{}) (interface{}, error) {
		return mcp.ExecuteToolWithResolver(operationID, parameters, envResolver)
	})

	mcp.Mount("/mcp")
}

// configureMCPForQuicknodeDirectly shows direct URL resolution without helper functions
func configureMCPForQuicknodeDirectly(r *gin.Engine) {
	mcp := server.New(r, &server.Config{
		Name:        "Gaming Store API",
		Description: "RESTful API for managing gaming products",
	})

	// Register request schemas for MCP
	mcp.RegisterSchema("GET", "/products", ListProductsParams{}, nil)
	mcp.RegisterSchema("POST", "/products", nil, Product{})
	mcp.RegisterSchema("PUT", "/products/:id", nil, UpdateProductRequest{})

	// Direct approach: check environment and use dynamic URL
	mcp.SetExecuteToolFunc(func(c *gin.Context, operationID string, parameters map[string]interface{}) (interface{}, error) {
		// Try multiple environment variables in order of preference
		userEndpoint := os.Getenv("QUICKNODE_USER_ENDPOINT")
		if userEndpoint == "" {
			userEndpoint = os.Getenv("USER_ENDPOINT")
		}
		if userEndpoint == "" {
			userEndpoint = os.Getenv("HOST")
			if userEndpoint != "" && !startsWith(userEndpoint, "http") {
				userEndpoint = "https://" + userEndpoint
			}
		}
		if userEndpoint == "" {
			userEndpoint = "http://localhost:8080" // fallback
		}

		return mcp.ExecuteToolWithDynamicURL(operationID, parameters, userEndpoint)
	})

	mcp.Mount("/mcp")
}

// startsWith is a simple helper to check string prefix
func startsWith(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

// Example of how to switch between configurations
func quicknodeMain() {
	gin.SetMode(gin.DebugMode)
	r := gin.Default()

	// Register API routes (same as main example)
	registerRoutes(r)

	// Use Quicknode configuration instead of regular MCP config
	if os.Getenv("QUICKNODE_MODE") == "true" {
		configureMCPForQuicknode(r)
	} else {
		configureMCP(r) // fallback to regular static baseURL
	}

	r.Run(":8080")
}
