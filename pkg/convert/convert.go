package convert

import (
	"fmt"
	"reflect"
	"regexp"
	"runtime"
	"strings"

	"go/ast"
	"go/parser"
	"go/token"

	"github.com/ckanthony/gin-mcp/pkg/types"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

// isDebugMode returns true if Gin is in debug mode
func isDebugMode() bool {
	return gin.Mode() == gin.DebugMode
}

// generatedAnnotations holds handler docs that were pre-generated at build time
// (via cmd/annotate-gen).  Keyed by handler function name, e.g. "listProducts".
// When set, ConvertRoutesToTools checks this map first and only falls back to
// on-disk source parsing when a handler is not found here.
var generatedAnnotations map[string]*HandlerDoc

// SetGeneratedAnnotations injects pre-generated annotation mappings.
// Multiple packages may call this (via their own annotations_gen.go init());
// later calls merge into the existing map so @body structs defined in a
// different package are still resolved.
func SetGeneratedAnnotations(m map[string]*HandlerDoc) {
	if generatedAnnotations == nil {
		generatedAnnotations = m
		return
	}
	for k, v := range m {
		generatedAnnotations[k] = v
	}
}

// generatedStructs holds pre-computed struct field metadata keyed by struct name.
var generatedStructs map[string]*types.StructMeta

// SetGeneratedStructs injects pre-computed struct metadata for @body resolution.
// Merges with existing data so structs from different packages accumulate.
func SetGeneratedStructs(m map[string]*types.StructMeta) {
	if generatedStructs == nil {
		generatedStructs = m
		return
	}
	for k, v := range m {
		generatedStructs[k] = v
	}
}

// ConvertRoutesToTools converts Gin routes into a list of MCP Tools and an operation map.
func ConvertRoutesToTools(routes gin.RoutesInfo, registeredSchemas map[string]types.RegisteredSchemaInfo) ([]types.Tool, map[string]types.Operation) {
	ttools := make([]types.Tool, 0)
	operations := make(map[string]types.Operation)
	usedOperationIDs := make(map[string]bool)

	if isDebugMode() {
		log.Printf("Starting conversion of %d routes to MCP tools...", len(routes))
	}

	for _, route := range routes {
		// Default operation ID generation (e.g., GET_users_id)
		operationID := strings.ToUpper(route.Method) + strings.ReplaceAll(strings.ReplaceAll(route.Path, "/", "_"), ":", "")

		filePath, handlerName := getHandlerInfo(route.HandlerFunc)

		// Look up handler annotations: generated map first, then on-disk source
		var handlerDoc *HandlerDoc
		if generatedAnnotations != nil {
			handlerDoc = generatedAnnotations[handlerName]
		}
		if handlerDoc == nil {
			handlerDoc, _ = parseHandlerComments(filePath, handlerName)
		}

		// Override with custom @operationId if present
		if handlerDoc != nil && handlerDoc.OperationID != "" {
			customOpID := handlerDoc.OperationID
			if usedOperationIDs[customOpID] {
				// Always log duplicate warnings, not just in debug mode
				log.Printf("Warning: Duplicate @operationId '%s' for route %s %s. Skipping this route to maintain consistency. First declaration wins.", customOpID, route.Method, route.Path)
				continue // Skip this route entirely
			}
			operationID = customOpID
		}

		// Check for duplicates even with default IDs (shouldn't happen but be defensive)
		if usedOperationIDs[operationID] {
			log.Printf("Warning: Duplicate operation ID '%s' for route %s %s. Skipping this route.", operationID, route.Method, route.Path)
			continue
		}

		// Track used operation IDs
		usedOperationIDs[operationID] = true

		if isDebugMode() {
			log.Printf("Processing route: %s %s -> OpID: %s", route.Method, route.Path, operationID)
		}

		// Generate description information
		description := fmt.Sprintf("Handler for %s %s", route.Method, route.Path)
		if handlerDoc != nil {
			if handlerDoc.Summary != "" {
				description = handlerDoc.Summary
			}
			if handlerDoc.Description != "" {
				description += "\n\n" + handlerDoc.Description
			}
		}

		// Generate schema for the tool's input
		inputSchema := generateInputSchema(route, registeredSchemas, handlerDoc)

		// Add parameter descriptions to schema if available
		if handlerDoc != nil && len(handlerDoc.Params) > 0 {
			for paramName, paramDesc := range handlerDoc.Params {
				if prop, ok := inputSchema.Properties[paramName]; ok {
					prop.Description = paramDesc
				}
			}
		}

		// Extract tags from handler doc
		var tags []string
		if handlerDoc != nil && len(handlerDoc.Tags) > 0 {
			tags = handlerDoc.Tags
		}

		tool := types.Tool{
			Name:        operationID,
			Description: description,
			InputSchema: inputSchema,
			Tags:        tags,
		}

		ttools = append(ttools, tool)
		operations[operationID] = types.Operation{
			Method: route.Method,
			Path:   route.Path,
			Tags:   tags,
		}
	}

	if isDebugMode() {
		log.Printf("Finished route conversion. Generated %d tools.", len(ttools))
	}

	return ttools, operations
}

// PathParamRegex is used to find path parameters like :id or *action
var PathParamRegex = regexp.MustCompile(`[:\*]([a-zA-Z0-9_]+)`)

// generateInputSchema creates the JSON schema for the tool's input parameters.
// This is a simplified version using basic reflection and not an external library.
func generateInputSchema(route gin.RouteInfo, registeredSchemas map[string]types.RegisteredSchemaInfo, handlerDoc *HandlerDoc) *types.JSONSchema {
	// Base schema structure
	schema := &types.JSONSchema{
		Type:       "object",
		Properties: make(map[string]*types.JSONSchema),
		Required:   make([]string, 0),
	}
	properties := schema.Properties
	required := schema.Required

	// 1. Extract Path Parameters
	matches := PathParamRegex.FindAllStringSubmatch(route.Path, -1)
	for _, match := range matches {
		if len(match) > 1 {
			paramName := match[1]
			properties[paramName] = &types.JSONSchema{
				Type:        "string",
				Description: fmt.Sprintf("Path parameter: %s", paramName),
			}
			required = append(required, paramName) // Path params are always required
		}
	}

	// 2. Incorporate Registered Query and Body Types
	schemaKey := route.Method + " " + route.Path
	hasRegisteredBody := false
	if schemaInfo, exists := registeredSchemas[schemaKey]; exists {
		if isDebugMode() {
			log.Printf("Using registered schema for %s", schemaKey)
		}

		// Reflect Query Parameters (if applicable for method and type exists)
		if (route.Method == "GET" || route.Method == "DELETE") && schemaInfo.QueryType != nil {
			reflectAndAddProperties(schemaInfo.QueryType, properties, &required, "query")
		}

		// Reflect Body Parameters (if applicable for method and type exists)
		if (route.Method == "POST" || route.Method == "PUT" || route.Method == "PATCH") && schemaInfo.BodyType != nil {
			reflectAndAddProperties(schemaInfo.BodyType, properties, &required, "body")
			hasRegisteredBody = true
		}
	}

	// 3. Incorporate @body struct from generated metadata (fallback when no registered body)
	if !hasRegisteredBody &&
		(route.Method == "POST" || route.Method == "PUT" || route.Method == "PATCH") &&
		handlerDoc != nil && handlerDoc.BodyTypeName != "" &&
		generatedStructs != nil {
		if sm, ok := generatedStructs[handlerDoc.BodyTypeName]; ok {
			addStructMetaToSchema(sm, properties, &required)
		}
	}

	// Update the required slice in the main schema
	schema.Required = required

	// If no properties were added (beyond path params), handle appropriately.
	// Depending on the spec, an empty properties object might be required.
	// if len(properties) == 0 { // Keep properties map even if empty
	// 	// Return schema with empty properties
	// 	return schema
	// }

	return schema
}

// reflectAndAddProperties uses basic reflection to add properties to the schema.
func reflectAndAddProperties(goType interface{}, properties map[string]*types.JSONSchema, required *[]string, source string) {
	if goType == nil {
		return // Handle nil input gracefully
	}
	t := types.ReflectType(reflect.TypeOf(goType)) // Use helper from types pkg
	if t == nil || t.Kind() != reflect.Struct {
		if isDebugMode() {
			log.Printf("Skipping schema generation for non-struct type: %v (%s)", reflect.TypeOf(goType), source)
		}
		return
	}

	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		jsonTag := field.Tag.Get("json")
		formTag := field.Tag.Get("form")             // Used for query params often
		jsonschemaTag := field.Tag.Get("jsonschema") // Basic support

		fieldName := field.Name // Default to field name
		ignoreField := false

		// Determine field name from tags (prefer json, then form)
		if jsonTag != "" {
			parts := strings.Split(jsonTag, ",")
			if parts[0] == "-" {
				ignoreField = true
			} else {
				fieldName = parts[0]
			}
			if len(parts) > 1 && parts[1] == "omitempty" {
				// omitempty = true // Variable removed
			}
		} else if formTag != "" {
			parts := strings.Split(formTag, ",")
			if parts[0] == "-" {
				ignoreField = true
			} else {
				fieldName = parts[0]
			}
			// form tag doesn't typically have omitempty in the same way
		}

		if ignoreField || !field.IsExported() {
			continue
		}

		propSchema := &types.JSONSchema{}

		// Basic type mapping
		switch field.Type.Kind() {
		case reflect.String:
			propSchema.Type = "string"
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
			reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			propSchema.Type = "integer"
		case reflect.Float32, reflect.Float64:
			propSchema.Type = "number"
		case reflect.Bool:
			propSchema.Type = "boolean"
		case reflect.Slice, reflect.Array:
			propSchema.Type = "array"
			// TODO: Implement items schema based on element type
			propSchema.Items = &types.JSONSchema{Type: "string"} // Placeholder
		case reflect.Map:
			propSchema.Type = "object"
			// TODO: Implement properties schema based on map key/value types
		case reflect.Struct:
			propSchema.Type = "object"
			// Potentially recurse, but keep simple for now
		default:
			propSchema.Type = "string" // Default fallback
		}

		// Basic 'required' and 'description' handling from jsonschema tag
		isRequired := false // Default to not required
		if jsonschemaTag != "" {
			parts := strings.Split(jsonschemaTag, ",")
			for _, part := range parts {
				trimmed := strings.TrimSpace(part)
				if trimmed == "required" {
					isRequired = true
				} else if strings.HasPrefix(trimmed, "description=") {
					propSchema.Description = strings.TrimPrefix(trimmed, "description=")
				}
				// TODO: Add more tag parsing (minimum, maximum, enum, etc.)
			}
		}

		// Add to properties map
		properties[fieldName] = propSchema

		// Add to required list if necessary
		if isRequired {
			*required = append(*required, fieldName)
		}
	}
}

// addStructMetaToSchema adds pre-computed struct field metadata to a JSON schema.
func addStructMetaToSchema(sm *types.StructMeta, properties map[string]*types.JSONSchema, required *[]string) {
	for _, fm := range sm.Fields {
		propSchema := &types.JSONSchema{
			Type:        fm.Type,
			Description: fm.Description,
		}
		// Array type gets a placeholder items schema.
		if fm.Type == "array" {
			propSchema.Items = &types.JSONSchema{Type: "string"}
		}
		properties[fm.JSONName] = propSchema
		if fm.Required {
			*required = append(*required, fm.JSONName)
		}
	}
}

// HandlerDoc stores function documentation
type HandlerDoc struct {
	Summary      string
	Description  string
	Params       map[string]string
	Returns      string
	Tags         []string
	OperationID  string
	BodyTypeName string // name of the request-body struct (from @body annotation)
}

// parseHandlerComments parses function documentation from source code
func parseHandlerComments(filePath string, handlerName string) (*HandlerDoc, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filePath, nil, parser.ParseComments)
	if err != nil {
		log.Printf("Failed to parse file %s: %v", filePath, err)
		return nil, err
	}

	// Iterate through top-level declarations
	for _, decl := range f.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok {
			if fn.Name.String() == handlerName {
				if fn.Doc == nil {
					return nil, nil
				}
				return ParseAnnotationLines(fn.Doc.Text()), nil
			}
		}
	}

	return nil, nil
}

// ParseAnnotationLines parses annotation tags from a comment text block.
// Exported so the code generator can reuse the same parsing logic.
func ParseAnnotationLines(commentText string) *HandlerDoc {
	doc := &HandlerDoc{
		Params: make(map[string]string),
	}

	lines := strings.Split(commentText, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "@summary"):
			doc.Summary = strings.TrimSpace(strings.TrimPrefix(line, "@summary"))
		case strings.HasPrefix(line, "@description"):
			doc.Description = strings.TrimSpace(strings.TrimPrefix(line, "@description"))
		case strings.HasPrefix(line, "@param"):
			paramText := strings.TrimSpace(strings.TrimPrefix(line, "@param"))
			parts := strings.SplitN(paramText, " ", 2)
			if len(parts) == 2 {
				paramName := strings.TrimSpace(parts[0])
				paramDesc := strings.TrimSpace(parts[1])
				doc.Params[paramName] = paramDesc
			}
		case strings.HasPrefix(line, "@return"):
			doc.Returns = strings.TrimSpace(strings.TrimPrefix(line, "@return"))
		case strings.HasPrefix(line, "@tags"):
			tagsText := strings.TrimSpace(strings.TrimPrefix(line, "@tags"))
			var tags []string
			for _, sep := range []string{",", " "} {
				parts := strings.Split(tagsText, sep)
				for _, part := range parts {
					trimmed := strings.TrimSpace(part)
					if trimmed != "" && !contains(tags, trimmed) {
						tags = append(tags, trimmed)
					}
				}
				if sep == "," {
					tagsText = strings.Join(tags, " ")
					tags = []string{}
				}
			}
			doc.Tags = tags
		case strings.HasPrefix(line, "@operationId"):
			opID := strings.TrimSpace(strings.TrimPrefix(line, "@operationId"))
			if opID != "" && doc.OperationID == "" {
				doc.OperationID = opID
			}
		case strings.HasPrefix(line, "@body"):
			bodyType := strings.TrimSpace(strings.TrimPrefix(line, "@body"))
			if bodyType != "" && doc.BodyTypeName == "" {
				doc.BodyTypeName = bodyType
			}
		}
	}

	return doc
}

func getHandlerInfo(handler gin.HandlerFunc) (string, string) {
	// Get function reflection value
	v := reflect.ValueOf(handler)
	ptr := v.Pointer()

	// Get function name and info
	funcInfo := runtime.FuncForPC(ptr)
	if funcInfo == nil {
		return "", ""
	}

	fullName := funcInfo.Name()
	filePath, _ := funcInfo.FileLine(ptr)

	// Extract short function name from full name.
	//
	// fullName can be one of:
	//   "main.listProducts"                           (top-level function)
	//   "main.(*Server).login-fm"                     (method value → wrapper)
	//   "main.(*Server).login"                        (method expression)
	//   "github.com/foo/bar.(*Controller).Handler-fm" (method value in another package)
	fullName = strings.TrimSuffix(fullName, "-fm")
	parts := strings.Split(fullName, ".")
	shortName := parts[len(parts)-1]
	// If shortName still carries a receiver suffix like "Controller).Login",
	// keep only the method name after the closing paren.
	if idx := strings.LastIndex(shortName, ")"); idx >= 0 {
		shortName = strings.TrimPrefix(shortName[idx+1:], ".")
	}

	return filePath, shortName
}

// contains checks if a string slice contains a specific string
func contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}
