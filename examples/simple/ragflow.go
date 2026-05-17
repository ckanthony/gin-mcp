package main

import (
	"context"
	"os"
	"strings"

	server "github.com/ckanthony/gin-mcp"
	"github.com/gin-gonic/gin"
)

// ConfigureMCPForRAGFlow demonstrates how to configure MCP for RAGFlow scenarios
// where each deployment or workflow has its own dynamic endpoint.
//
// Usage:
//
//	r := gin.Default()
//	registerRoutes(r)
//	configureMCPForRAGFlow(r)  // Use this instead of configureMCP(r)
//	r.Run(":8080")
//
// Environment Variables:
//
//	RAGFLOW_ENDPOINT        - The RAGFlow deployment endpoint (e.g., "https://api.ragflow.com/workflow/123")
//	RAGFLOW_WORKFLOW_URL    - Alternative workflow-specific URL
//	RAGFLOW_BASE_URL        - Base RAGFlow URL
//	WORKFLOW_ID             - Will be combined with base URL if provided
func configureMCPForRAGFlow(r *gin.Engine) {
	mcp := server.New(r, &server.Config{
		Name:        "Gaming Store API",
		Description: "RESTful API for managing gaming products with RAGFlow integration",
		// Note: BaseURL is not set here since we'll resolve it dynamically
	})

	// Register request schemas for MCP
	mcp.RegisterSchema("GET", "/products", ListProductsParams{}, nil)
	mcp.RegisterSchema("POST", "/products", nil, Product{})
	mcp.RegisterSchema("PUT", "/products/:id", nil, UpdateProductRequest{})

	// Setup dynamic baseURL resolution for RAGFlow
	resolver := server.NewRAGFlowResolver("http://localhost:8080")

	// Override the default tool execution with dynamic baseURL logic
	mcp.SetExecuteToolFunc(func(ctx context.Context, operationID string, parameters map[string]interface{}) (interface{}, error) {
		return mcp.ExecuteToolWithResolver(operationID, parameters, resolver)
	})

	// Mount MCP endpoint
	mcp.Mount("/mcp")
}

// configureMCPForRAGFlowWithWorkflow shows workflow-specific configuration
func configureMCPForRAGFlowWithWorkflow(r *gin.Engine) {
	mcp := server.New(r, &server.Config{
		Name:        "Gaming Store API",
		Description: "RESTful API for RAGFlow workflow integration",
	})

	// Register request schemas for MCP
	mcp.RegisterSchema("GET", "/products", ListProductsParams{}, nil)
	mcp.RegisterSchema("POST", "/products", nil, Product{})
	mcp.RegisterSchema("PUT", "/products/:id", nil, UpdateProductRequest{})

	// Use environment-specific resolver for workflow URLs
	workflowResolver := server.NewEnvironmentResolver("RAGFLOW_WORKFLOW_URL", "http://localhost:8080")
	mcp.SetExecuteToolFunc(func(ctx context.Context, operationID string, parameters map[string]interface{}) (interface{}, error) {
		return mcp.ExecuteToolWithResolver(operationID, parameters, workflowResolver)
	})

	mcp.Mount("/mcp")
}

// configureMCPForRAGFlowDirectly shows direct URL resolution for RAGFlow
func configureMCPForRAGFlowDirectly(r *gin.Engine) {
	mcp := server.New(r, &server.Config{
		Name:        "Gaming Store API",
		Description: "RESTful API for RAGFlow direct integration",
	})

	// Register request schemas for MCP
	mcp.RegisterSchema("GET", "/products", ListProductsParams{}, nil)
	mcp.RegisterSchema("POST", "/products", nil, Product{})
	mcp.RegisterSchema("PUT", "/products/:id", nil, UpdateProductRequest{})

	// Direct approach: construct RAGFlow URL from components
	mcp.SetExecuteToolFunc(func(ctx context.Context, operationID string, parameters map[string]interface{}) (interface{}, error) {
		endpoint := buildRAGFlowEndpoint()
		return mcp.ExecuteToolWithDynamicURL(operationID, parameters, endpoint)
	})

	mcp.Mount("/mcp")
}

// buildRAGFlowEndpoint constructs the RAGFlow endpoint from environment variables
func buildRAGFlowEndpoint() string {
	// Try direct endpoint first
	if endpoint := os.Getenv("RAGFLOW_ENDPOINT"); endpoint != "" {
		return endpoint
	}

	// Try workflow URL
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

	// Fallback to localhost
	return "http://localhost:8080"
}

// Example of how to switch between configurations based on deployment mode
func ragflowMain() {
	gin.SetMode(gin.DebugMode)
	r := gin.Default()

	// Register API routes (same as main example)
	registerRoutes(r)

	// Choose configuration based on environment
	deploymentMode := os.Getenv("DEPLOYMENT_MODE")
	switch deploymentMode {
	case "ragflow":
		configureMCPForRAGFlow(r)
	case "ragflow-workflow":
		configureMCPForRAGFlowWithWorkflow(r)
	case "ragflow-direct":
		configureMCPForRAGFlowDirectly(r)
	default:
		configureMCP(r) // fallback to regular static baseURL
	}

	r.Run(":8080")
}
