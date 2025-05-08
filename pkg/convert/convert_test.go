package convert

import (
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ckanthony/gin-mcp/pkg/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Test Setup ---

type TestQuery struct {
	QueryParam string `form:"queryParam" json:"queryParam" jsonschema:"description=A query parameter"`
	Optional   string `form:"optional,omitempty" json:"optional,omitempty"`
}

type TestBody struct {
	BodyField string `json:"bodyField" jsonschema:"required,description=A required body field"`
	NumField  int    `json:"numField"`
}

func noOpHandler(c *gin.Context) {}

func setupTestRoutes() gin.RoutesInfo {
	// Disable debug print for tests
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	// GET route with path param and query struct
	r.GET("/users/:userId", noOpHandler)
	// POST route with path param and body struct
	r.POST("/items/:itemId", noOpHandler)
	// GET route with no params
	r.GET("/health", noOpHandler)
	// PUT route (no registered schema later)
	r.PUT("/config/:configId", noOpHandler)
	// Route with wildcard
	r.GET("/files/*filepath", noOpHandler)

	return r.Routes()
}

func setupTestRegisteredSchemas() map[string]types.RegisteredSchemaInfo {
	return map[string]types.RegisteredSchemaInfo{
		"GET /users/:userId": {
			QueryType: TestQuery{}, // Use instance for reflect
			BodyType:  nil,
		},
		"POST /items/:itemId": {
			QueryType: nil,
			BodyType:  TestBody{}, // Use instance for reflect
		},
		"GET /health": { // Route with no params/body/query needs entry? Check generateInputSchema logic
			QueryType: nil,
			BodyType:  nil,
		},
		"GET /files/*filepath": { // Route with wildcard
			QueryType: nil,
			BodyType:  nil,
		},
		// "/config/:configId" PUT is intentionally omitted to test missing schema case
	}
}

// --- Tests for ConvertRoutesToTools ---

func TestConvertRoutesToTools(t *testing.T) {
	routes := setupTestRoutes()
	schemas := setupTestRegisteredSchemas()

	tools, operations := ConvertRoutesToTools(routes, schemas)

	assert.Len(t, tools, 5, "Should generate 5 tools")
	assert.Len(t, operations, 5, "Should generate 5 operations")

	// --- Verification for GET /users/:userId ---
	opIDGetUsers := "GET_users_userId"
	assert.Contains(t, operations, opIDGetUsers)
	assert.Equal(t, http.MethodGet, operations[opIDGetUsers].Method)
	assert.Equal(t, "/users/:userId", operations[opIDGetUsers].Path)

	var toolGetUsers *types.Tool
	for i := range tools {
		if tools[i].Name == opIDGetUsers {
			toolGetUsers = &tools[i]
			break
		}
	}
	require.NotNil(t, toolGetUsers, "Tool for GET /users/:userId not found")
	assert.Equal(t, opIDGetUsers, toolGetUsers.Name)
	require.NotNil(t, toolGetUsers.InputSchema, "InputSchema should not be nil")
	require.NotNil(t, toolGetUsers.InputSchema.Properties, "Properties should not be nil")
	// Check path param
	assert.Contains(t, toolGetUsers.InputSchema.Properties, "userId")
	assert.Equal(t, "string", toolGetUsers.InputSchema.Properties["userId"].Type)
	// Check query param (from TestQuery)
	assert.Contains(t, toolGetUsers.InputSchema.Properties, "queryParam")
	assert.Equal(t, "string", toolGetUsers.InputSchema.Properties["queryParam"].Type)
	assert.Equal(t, "A query parameter", toolGetUsers.InputSchema.Properties["queryParam"].Description)
	assert.Contains(t, toolGetUsers.InputSchema.Properties, "optional")
	assert.Equal(t, "string", toolGetUsers.InputSchema.Properties["optional"].Type)
	// Check required fields (path param + required query/body fields)
	assert.Contains(t, toolGetUsers.InputSchema.Required, "userId")        // Path param is required
	assert.NotContains(t, toolGetUsers.InputSchema.Required, "queryParam") // Not marked required
	assert.NotContains(t, toolGetUsers.InputSchema.Required, "optional")   // Marked omitempty

	// --- Verification for POST /items/:itemId ---
	opIDPostItems := "POST_items_itemId"
	assert.Contains(t, operations, opIDPostItems)
	assert.Equal(t, http.MethodPost, operations[opIDPostItems].Method)
	assert.Equal(t, "/items/:itemId", operations[opIDPostItems].Path)

	var toolPostItems *types.Tool
	for i := range tools {
		if tools[i].Name == opIDPostItems {
			toolPostItems = &tools[i]
			break
		}
	}
	require.NotNil(t, toolPostItems, "Tool for POST /items/:itemId not found")
	assert.Equal(t, opIDPostItems, toolPostItems.Name)
	require.NotNil(t, toolPostItems.InputSchema, "InputSchema should not be nil")
	require.NotNil(t, toolPostItems.InputSchema.Properties, "Properties should not be nil")
	// Check path param
	assert.Contains(t, toolPostItems.InputSchema.Properties, "itemId")
	assert.Equal(t, "string", toolPostItems.InputSchema.Properties["itemId"].Type)
	// Check body params (from TestBody)
	assert.Contains(t, toolPostItems.InputSchema.Properties, "bodyField")
	assert.Equal(t, "string", toolPostItems.InputSchema.Properties["bodyField"].Type)
	assert.Equal(t, "A required body field", toolPostItems.InputSchema.Properties["bodyField"].Description)
	assert.Contains(t, toolPostItems.InputSchema.Properties, "numField")
	assert.Equal(t, "integer", toolPostItems.InputSchema.Properties["numField"].Type)
	// Check required fields
	assert.Contains(t, toolPostItems.InputSchema.Required, "itemId")      // Path param
	assert.Contains(t, toolPostItems.InputSchema.Required, "bodyField")   // Marked required in struct tag
	assert.NotContains(t, toolPostItems.InputSchema.Required, "numField") // Not marked required

	// --- Verification for GET /health ---
	opIDGetHealth := "GET_health"
	assert.Contains(t, operations, opIDGetHealth)
	assert.Equal(t, http.MethodGet, operations[opIDGetHealth].Method)
	assert.Equal(t, "/health", operations[opIDGetHealth].Path)
	// Find tool and check schema (should be minimal)
	var toolGetHealth *types.Tool
	for i := range tools {
		if tools[i].Name == opIDGetHealth {
			toolGetHealth = &tools[i]
			break
		}
	}
	require.NotNil(t, toolGetHealth, "Tool for GET /health not found")
	require.NotNil(t, toolGetHealth.InputSchema, "InputSchema should not be nil for parameterless route")
	assert.Empty(t, toolGetHealth.InputSchema.Properties, "Properties should be empty for health check")
	assert.Empty(t, toolGetHealth.InputSchema.Required, "Required should be empty for health check")

	// --- Verification for PUT /config/:configId (Schema not registered) ---
	opIDPutConfig := "PUT_config_configId"
	assert.Contains(t, operations, opIDPutConfig)
	assert.Equal(t, http.MethodPut, operations[opIDPutConfig].Method)
	assert.Equal(t, "/config/:configId", operations[opIDPutConfig].Path)
	// Find tool and check schema (should only have path param)
	var toolPutConfig *types.Tool
	for i := range tools {
		if tools[i].Name == opIDPutConfig {
			toolPutConfig = &tools[i]
			break
		}
	}
	require.NotNil(t, toolPutConfig, "Tool for PUT /config/:configId not found")
	require.NotNil(t, toolPutConfig.InputSchema, "InputSchema should not be nil")
	require.NotNil(t, toolPutConfig.InputSchema.Properties, "Properties should not be nil")
	assert.Len(t, toolPutConfig.InputSchema.Properties, 1, "Should only have path param property")
	assert.Contains(t, toolPutConfig.InputSchema.Properties, "configId")
	assert.Equal(t, "string", toolPutConfig.InputSchema.Properties["configId"].Type)
	assert.Len(t, toolPutConfig.InputSchema.Required, 1, "Should only require path param")
	assert.Contains(t, toolPutConfig.InputSchema.Required, "configId")

	// --- Verification for GET /files/*filepath ---
	opIDGetFiles := "GET_files_*filepath"
	assert.Contains(t, operations, opIDGetFiles)
	assert.Equal(t, http.MethodGet, operations[opIDGetFiles].Method)
	assert.Equal(t, "/files/*filepath", operations[opIDGetFiles].Path)
	// Find tool and check schema (should have wildcard path param)
	var toolGetFiles *types.Tool
	for i := range tools {
		if tools[i].Name == opIDGetFiles {
			toolGetFiles = &tools[i]
			break
		}
	}
	require.NotNil(t, toolGetFiles, "Tool for GET /files/*filepath not found")
	require.NotNil(t, toolGetFiles.InputSchema, "InputSchema should not be nil")
	require.NotNil(t, toolGetFiles.InputSchema.Properties, "Properties should not be nil")
	assert.Len(t, toolGetFiles.InputSchema.Properties, 1, "Should only have path param property")
	assert.Contains(t, toolGetFiles.InputSchema.Properties, "filepath")
	assert.Equal(t, "string", toolGetFiles.InputSchema.Properties["filepath"].Type)
	assert.Len(t, toolGetFiles.InputSchema.Required, 1, "Should only require path param")
	assert.Contains(t, toolGetFiles.InputSchema.Required, "filepath")
	assert.NotContains(t, toolGetFiles.InputSchema.Required, "optional") // omitempty

}

// --- Tests for generateInputSchema (called indirectly by ConvertRoutesToTools) ---
// We test this indirectly via ConvertRoutesToTools, but add specific cases if needed.

func TestGenerateInputSchema_NoParams(t *testing.T) {
	route := gin.RouteInfo{Method: "GET", Path: "/simple"}
	schemas := make(map[string]types.RegisteredSchemaInfo)

	schema := generateInputSchema(route, schemas)

	require.NotNil(t, schema)
	assert.Equal(t, "object", schema.Type)
	assert.Empty(t, schema.Properties)
	assert.Empty(t, schema.Required)
}

func TestGenerateInputSchema_OnlyPathParams(t *testing.T) {
	route := gin.RouteInfo{Method: "DELETE", Path: "/resource/:id/sub/:subId"}
	schemas := make(map[string]types.RegisteredSchemaInfo)

	schema := generateInputSchema(route, schemas)

	require.NotNil(t, schema)
	assert.Equal(t, "object", schema.Type)
	require.NotNil(t, schema.Properties)
	assert.Len(t, schema.Properties, 2)
	assert.Contains(t, schema.Properties, "id")
	assert.Equal(t, "string", schema.Properties["id"].Type)
	assert.Contains(t, schema.Properties, "subId")
	assert.Equal(t, "string", schema.Properties["subId"].Type)

	require.NotNil(t, schema.Required)
	assert.Len(t, schema.Required, 2)
	assert.Contains(t, schema.Required, "id")
	assert.Contains(t, schema.Required, "subId")
}

func TestGenerateInputSchema_WithPathAndQuery(t *testing.T) {
	route := gin.RouteInfo{Method: "GET", Path: "/search/:topic"}
	schemas := map[string]types.RegisteredSchemaInfo{
		"GET /search/:topic": {QueryType: TestQuery{}},
	}

	schema := generateInputSchema(route, schemas)

	require.NotNil(t, schema)
	assert.Equal(t, "object", schema.Type)
	require.NotNil(t, schema.Properties)
	assert.Len(t, schema.Properties, 3) // topic, queryParam, optional
	// Path
	assert.Contains(t, schema.Properties, "topic")
	assert.Equal(t, "string", schema.Properties["topic"].Type)
	// Query
	assert.Contains(t, schema.Properties, "queryParam")
	assert.Equal(t, "string", schema.Properties["queryParam"].Type)
	assert.Contains(t, schema.Properties, "optional")
	assert.Equal(t, "string", schema.Properties["optional"].Type)

	require.NotNil(t, schema.Required)
	assert.Len(t, schema.Required, 1) // Only path param 'topic' is inherently required
	assert.Contains(t, schema.Required, "topic")
	assert.NotContains(t, schema.Required, "queryParam") // Not marked required
	assert.NotContains(t, schema.Required, "optional")   // omitempty
}

func TestGenerateInputSchema_WithPathAndBody(t *testing.T) {
	route := gin.RouteInfo{Method: "POST", Path: "/create/:parentId"}
	schemas := map[string]types.RegisteredSchemaInfo{
		"POST /create/:parentId": {BodyType: TestBody{}},
	}

	schema := generateInputSchema(route, schemas)

	require.NotNil(t, schema)
	assert.Equal(t, "object", schema.Type)
	require.NotNil(t, schema.Properties)
	assert.Len(t, schema.Properties, 3) // parentId, bodyField, numField
	// Path
	assert.Contains(t, schema.Properties, "parentId")
	assert.Equal(t, "string", schema.Properties["parentId"].Type)
	// Body
	assert.Contains(t, schema.Properties, "bodyField")
	assert.Equal(t, "string", schema.Properties["bodyField"].Type)
	assert.Contains(t, schema.Properties, "numField")
	assert.Equal(t, "integer", schema.Properties["numField"].Type)

	require.NotNil(t, schema.Required)
	assert.Len(t, schema.Required, 2) // path param 'parentId' + 'bodyField' (marked required)
	assert.Contains(t, schema.Required, "parentId")
	assert.Contains(t, schema.Required, "bodyField")
	assert.NotContains(t, schema.Required, "numField") // Not marked required
}

// --- Tests for reflectAndAddProperties (also called indirectly) ---

type ReflectTestStruct struct {
	RequiredString string  `json:"req_str" jsonschema:"required,description=A required string"`
	OptionalInt    int     `json:"opt_int,omitempty"`
	DefaultName    bool    // No tags
	Hyphenated     string  `json:"-"`          // Ignored
	FormQuery      float64 `form:"form_query"` // Use form tag if json missing
	unexported     string  // Ignored
	SliceField     []int   `json:"slice_field"` // Basic slice support
	// MapField    map[string]string `json:"map_field"` // TODO: Test when map support added
	// StructField TestBody          `json:"struct_field"` // TODO: Test when struct recursion added
}

func TestReflectAndAddProperties(t *testing.T) {
	properties := make(map[string]*types.JSONSchema)
	required := []string{}

	// Pass the struct value instance, the properties map, required slice pointer, and a prefix string
	reflectAndAddProperties(ReflectTestStruct{}, properties, &required, "test")

	// Check properties
	assert.Len(t, properties, 5, "Should have 5 exported, non-ignored fields")

	// req_str
	assert.Contains(t, properties, "req_str")
	assert.Equal(t, "string", properties["req_str"].Type)
	assert.Equal(t, "A required string", properties["req_str"].Description)

	// opt_int
	assert.Contains(t, properties, "opt_int")
	assert.Equal(t, "integer", properties["opt_int"].Type)

	// DefaultName
	assert.Contains(t, properties, "DefaultName")
	assert.Equal(t, "boolean", properties["DefaultName"].Type)

	// form_query
	assert.Contains(t, properties, "form_query")
	assert.Equal(t, "number", properties["form_query"].Type)

	// slice_field
	assert.Contains(t, properties, "slice_field")
	assert.Equal(t, "array", properties["slice_field"].Type)
	require.NotNil(t, properties["slice_field"].Items, "Array items schema should exist")
	assert.Equal(t, "string", properties["slice_field"].Items.Type, "Basic array item type is string") // Placeholder

	// Check ignored fields
	assert.NotContains(t, properties, "-")
	assert.NotContains(t, properties, "Hyphenated")
	assert.NotContains(t, properties, "unexported")

	// Check required list
	// Default behavior: required only if jsonschema:required
	assert.Len(t, required, 1)
	assert.Contains(t, required, "req_str")        // Marked required
	assert.NotContains(t, required, "DefaultName") // Not marked required
	assert.NotContains(t, required, "form_query")  // Not marked required
	assert.NotContains(t, required, "slice_field") // Not marked required

	assert.NotContains(t, required, "opt_int") // Has omitempty and not marked required
}

func TestReflectAndAddProperties_NilInput(t *testing.T) {
	properties := make(map[string]*types.JSONSchema)
	required := []string{}

	// Test with nil interface{} value
	reflectAndAddProperties(nil, properties, &required, "test_nil_interface")
	assert.Empty(t, properties, "Properties should be empty for nil input")
	assert.Empty(t, required, "Required should be empty for nil input")

	// Test with nil pointer type value
	var ptr *ReflectTestStruct
	// Reset properties and required for the second case within the test
	properties = make(map[string]*types.JSONSchema)
	required = []string{}
	reflectAndAddProperties(ptr, properties, &required, "test_nil_struct_ptr")
	// Depending on implementation, properties might be populated from type info even if value is nil.
	// Check that required list is populated based on struct tags if type info is used.
	assert.Equal(t, []string{"req_str"}, required, "Required should contain fields marked required in the type definition for nil struct pointer input")
}

func TestReflectAndAddProperties_NonStructInput(t *testing.T) {
	properties := make(map[string]*types.JSONSchema)
	required := []string{}

	// Test with int value
	reflectAndAddProperties(123, properties, &required, "test_int")
	assert.Empty(t, properties, "Properties should be empty for non-struct input")
	assert.Empty(t, required, "Required should be empty for non-struct input")

	// Test with string pointer value
	var strPtr *string
	// Reset properties and required
	properties = make(map[string]*types.JSONSchema)
	required = []string{}
	reflectAndAddProperties(strPtr, properties, &required, "test_string_ptr")
	assert.Empty(t, properties, "Properties should be empty for non-struct pointer type")
	assert.Empty(t, required, "Required should be empty for non-struct pointer type")
}

// --- Test PathParamRegex ---

func TestPathParamRegex(t *testing.T) {
	tests := []struct {
		path     string
		expected []string // Just the param names
	}{
		{"/users/:userId", []string{"userId"}},
		{"/items/:itemId/details", []string{"itemId"}},
		{"/orders/:orderId/items/:itemId", []string{"orderId", "itemId"}},
		{"/files/*filepath", []string{"filepath"}},
		{"/config/:config_id/value", []string{"config_id"}},
		{"/a/b/c", []string{}}, // No params
		{"/:a/:b/*c", []string{"a", "b", "c"}},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			matches := PathParamRegex.FindAllStringSubmatch(tt.path, -1)
			actualParams := make([]string, 0, len(matches))
			for _, match := range matches {
				if len(match) > 1 {
					actualParams = append(actualParams, match[1])
				}
			}
			assert.ElementsMatch(t, tt.expected, actualParams)
		})
	}
}

// --- Helper for reflect testing (if needed) ---
// Not strictly necessary now as types.ReflectType handles pointers

func TestReflectTypeHelper(t *testing.T) { // Assuming types.ReflectType exists and handles pointers
	var s TestBody
	var ps *TestBody = &s

	rt := types.ReflectType(reflect.TypeOf(s))
	prt := types.ReflectType(reflect.TypeOf(ps))

	require.NotNil(t, rt)
	require.NotNil(t, prt)
	assert.Equal(t, reflect.Struct, rt.Kind())
	assert.Equal(t, reflect.Struct, prt.Kind())
	assert.Equal(t, rt, prt, "ReflectType should return the underlying struct type for both value and pointer")
}

// handler 是我们要测试的处理函数
// @summary 测试处理器
// @description 这是一个用于测试的处理器
// @param id 用户ID
func handler(c *gin.Context) {
	c.JSON(200, gin.H{"message": "test"})
}

func Test_getHandlerInfo(t *testing.T) {
	// 获取handler信息
	filePath, funcName := getHandlerInfo(handler)

	t.Logf("File Path: %s", filePath)
	t.Logf("Function Name: %s", funcName)

	// 验证函数名是否包含handler
	if funcName == "" || !strings.Contains(funcName, "handler") {
		t.Errorf("Expected function name to contain 'handler', got %s", funcName)
	}

	// 验证文件路径是否存在
	if filePath == "" {
		t.Error("File path should not be empty")
	}

	// 验证文件路径是否包含.go后缀
	if !strings.HasSuffix(filePath, ".go") {
		t.Errorf("Expected .go file, got %s", filePath)
	}
}

func TestParseHandlerComments(t *testing.T) {
	// Create a temporary test file
	tmpFile := `package test

// ListProducts handles product list retrieval
// @summary Get product list
// @description Returns a paginated list of products
// @param page Page number for pagination, starting from 1
// @return List of products
func ListProducts(c *gin.Context) {
	// Implementation
}

// GetProduct handles single product retrieval
// @summary Get product details
// @description Returns detailed information for a specific product
// @param id Product ID
// @return Product details
func GetProduct(c *gin.Context) {
	// Implementation
}
`
	// Create temporary file
	tmpDir := t.TempDir()
	tmpPath := filepath.Join(tmpDir, "handlers_test.go")
	err := os.WriteFile(tmpPath, []byte(tmpFile), 0644)
	assert.NoError(t, err)

	// Test ListProducts function comments
	doc, err := parseHandlerComments(tmpPath, "ListProducts")
	assert.NoError(t, err)
	assert.NotNil(t, doc)
	assert.Equal(t, "Get product list", strings.TrimSpace(doc.Summary))
	assert.Equal(t, "Returns a paginated list of products", strings.TrimSpace(doc.Description))
	assert.Equal(t, "Page number for pagination, starting from 1", strings.TrimSpace(doc.Params["page"]))
	assert.Equal(t, "List of products", strings.TrimSpace(doc.Returns))

	// Test GetProduct function comments
	doc, err = parseHandlerComments(tmpPath, "GetProduct")
	assert.NoError(t, err)
	assert.NotNil(t, doc)
	assert.Equal(t, "Get product details", strings.TrimSpace(doc.Summary))
	assert.Equal(t, "Returns detailed information for a specific product", strings.TrimSpace(doc.Description))
	assert.Equal(t, "Product ID", strings.TrimSpace(doc.Params["id"]))
	assert.Equal(t, "Product details", strings.TrimSpace(doc.Returns))
}

func TestParseHandlerComments_EdgeCases(t *testing.T) {
	tmpFile := `package test

// EmptyDoc
//
func EmptyDoc(c *gin.Context) {}

// MalformedTags
// @summary
// @param
// @return
func MalformedTags(c *gin.Context) {}

// MultipleParams test with multiple parameters
// @summary Test multiple parameters
// @param id User ID
// @param name Username
// @param age User age
// @return User information
func MultipleParams(c *gin.Context) {}
`
	// Create temporary file
	tmpDir := t.TempDir()
	tmpPath := filepath.Join(tmpDir, "edge_cases_test.go")
	err := os.WriteFile(tmpPath, []byte(tmpFile), 0644)
	assert.NoError(t, err)

	// Test empty document
	doc, err := parseHandlerComments(tmpPath, "EmptyDoc")
	assert.NoError(t, err)
	assert.NotNil(t, doc)
	assert.Empty(t, doc.Summary)
	assert.Empty(t, doc.Description)
	assert.Empty(t, doc.Returns)
	assert.Empty(t, doc.Params)

	// Test malformed tags
	doc, err = parseHandlerComments(tmpPath, "MalformedTags")
	assert.NoError(t, err)
	assert.NotNil(t, doc)
	assert.Empty(t, doc.Summary)
	assert.Empty(t, doc.Returns)
	assert.Empty(t, doc.Params)

	// Test multiple parameters
	doc, err = parseHandlerComments(tmpPath, "MultipleParams")
	assert.NoError(t, err)
	assert.NotNil(t, doc)
	assert.Equal(t, "Test multiple parameters", strings.TrimSpace(doc.Summary))
	assert.Equal(t, "User ID", strings.TrimSpace(doc.Params["id"]))
	assert.Equal(t, "Username", strings.TrimSpace(doc.Params["name"]))
	assert.Equal(t, "User age", strings.TrimSpace(doc.Params["age"]))
	assert.Equal(t, "User information", strings.TrimSpace(doc.Returns))
}
