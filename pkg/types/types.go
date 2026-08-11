package types

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"
)

// RawMessage is a raw encoded JSON value.
// It implements Marshaler and Unmarshaler and can
// be used to delay JSON decoding or precompute a JSON encoding.
// Defined as its own type based on json.RawMessage to be available
// for use in other packages (like server.go) without modifying them.
type RawMessage json.RawMessage

// MarshalJSON returns m as the JSON encoding of m.
func (m RawMessage) MarshalJSON() ([]byte, error) {
	if m == nil {
		return []byte("null"), nil
	}
	return m, nil
}

// UnmarshalJSON sets *m to a copy of data.
func (m *RawMessage) UnmarshalJSON(data []byte) error {
	if m == nil {
		return nil
	}
	*m = append((*m)[0:0], data...)
	return nil
}

// ContentType represents the type of content in a tool call response.
type ContentType string

const (
	ContentTypeText  ContentType = "text"
	ContentTypeJSON  ContentType = "json" // Example: Add other types if needed
	ContentTypeError ContentType = "error"
	ContentTypeImage ContentType = "image"
)

// MCPMessage represents a standard JSON-RPC 2.0 message used in MCP
type MCPMessage struct {
	Jsonrpc string      `json:"jsonrpc"`          // Must be "2.0"
	ID      RawMessage  `json:"id,omitempty"`     // Use our RawMessage type here
	Method  string      `json:"method,omitempty"` // Method name (e.g., "initialize", "tools/list")
	Params  interface{} `json:"params,omitempty"` // Parameters (object or array)
	Result  interface{} `json:"result,omitempty"` // Success result
	Error   interface{} `json:"error,omitempty"`  // Error object
}

// Tool represents a function or capability exposed by the server
type Tool struct {
	Name        string      `json:"name"`                  // Unique identifier for the tool
	Description string      `json:"description,omitempty"` // Human-readable description
	InputSchema *JSONSchema `json:"inputSchema"`           // Schema for the tool's input parameters
	Tags        []string    `json:"tags,omitempty"`        // Tags for filtering tools
	// Add other fields as needed by the MCP spec (e.g., outputSchema)
}

// Operation represents the mapping from a tool name (operation ID) to its underlying HTTP endpoint
type Operation struct {
	Method string   // HTTP Method (GET, POST, etc.)
	Path   string   // Gin route path (e.g., /users/:id)
	Tags   []string // Tags for filtering operations
}

// RegisteredSchemaInfo holds Go types associated with a specific route for schema generation
type RegisteredSchemaInfo struct {
	QueryType interface{} // Go struct or pointer to struct for query parameters (or nil)
	BodyType  interface{} // Go struct or pointer to struct for request body (or nil)
}

// JSONSchema represents a basic JSON Schema structure.
// This needs to be expanded based on actual schema generation needs.
type JSONSchema struct {
	Type        string                 `json:"type"`
	Description string                 `json:"description,omitempty"`
	Properties  map[string]*JSONSchema `json:"properties,omitempty"`
	Required    []string               `json:"required,omitempty"`
	Items       *JSONSchema            `json:"items,omitempty"` // For array type
	// Add other JSON Schema fields as needed (e.g., format, enum, etc.)
}

// GetSchema generates a JSON schema map for the given value using reflection.
// This is a basic implementation; a dedicated library is recommended for complex cases.
func GetSchema(value interface{}) map[string]interface{} {
	if value == nil {
		return nil
	}

	// Handle pointer types by getting the element type
	t := reflect.TypeOf(value)
	// Check for nil interface or nil pointer *before* dereferencing
	if t == nil || (t.Kind() == reflect.Ptr && reflect.ValueOf(value).IsNil()) {
		return nil
	}
	if t.Kind() == reflect.Ptr {
		// if t.Elem() == nil { // This check isn't quite right for nil pointer *values*
		// 	return nil // Handle nil pointer element if necessary
		// }
		t = t.Elem()
	}

	// Ensure it's a struct before proceeding
	if t.Kind() != reflect.Struct {
		// Return nil or a default schema if not a struct
		fmt.Printf("Warning: Cannot generate schema for non-struct type: %s\n", t.Kind())
		return map[string]interface{}{"type": "object"} // Default or error
	}

	schema := map[string]interface{}{
		"type":       "object",
		"properties": map[string]interface{}{},
		"required":   []string{},
	}
	properties := schema["properties"].(map[string]interface{})
	required := schema["required"].([]string)

	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		jsonTag := field.Tag.Get("json")
		formTag := field.Tag.Get("form")             // Also consider 'form' tags for query params
		jsonschemaTag := field.Tag.Get("jsonschema") // Basic jsonschema tag support

		// Determine the field name (prefer json tag, then form tag, then field name)
		fieldName := field.Name
		if jsonTag != "" && jsonTag != "-" {
			parts := strings.Split(jsonTag, ",")
			fieldName = parts[0]
		} else if formTag != "" && formTag != "-" {
			parts := strings.Split(formTag, ",")
			fieldName = parts[0]
		}

		// Skip unexported fields or explicitly ignored fields
		if !field.IsExported() || jsonTag == "-" || formTag == "-" {
			continue
		}

		// Basic type mapping (extend as needed)
		propSchema := map[string]interface{}{}
		switch field.Type.Kind() {
		case reflect.String:
			propSchema["type"] = "string"
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			propSchema["type"] = "integer"
		case reflect.Float32, reflect.Float64:
			propSchema["type"] = "number"
		case reflect.Bool:
			propSchema["type"] = "boolean"
		case reflect.Slice, reflect.Array:
			propSchema["type"] = "array"
			// TODO: Add items schema based on element type
		case reflect.Map:
			propSchema["type"] = "object"
			// TODO: Add properties schema based on map key/value types
		case reflect.Struct, reflect.Ptr: // <-- Also handle reflect.Ptr here
			// Check if the underlying type (after potential Ptr) is a struct
			elemType := field.Type
			if elemType.Kind() == reflect.Ptr {
				elemType = elemType.Elem()
			}
			if elemType.Kind() == reflect.Struct {
				propSchema["type"] = "object"
			} else {
				// Pointer to non-struct, treat as string for now
				propSchema["type"] = "string"
			}
			// Recursive call for nested structs (might need cycle detection)
			// For simplicity, just mark as object for now
			// propSchema["type"] = "object"
		default:
			propSchema["type"] = "string" // Default for unknown types
		}

		// Basic jsonschema tag parsing
		if jsonschemaTag != "" {
			parts := strings.Split(jsonschemaTag, ",")
			for _, part := range parts {
				trimmedPart := strings.TrimSpace(part)
				if trimmedPart == "required" {
					required = append(required, fieldName)
				} else if strings.HasPrefix(trimmedPart, "description=") {
					propSchema["description"] = strings.TrimPrefix(trimmedPart, "description=")
				} else if strings.HasPrefix(trimmedPart, "minimum=") {
					// Attempt to parse number, handle error
					if num, err := strconv.ParseFloat(strings.TrimPrefix(trimmedPart, "minimum="), 64); err == nil {
						propSchema["minimum"] = num
					}
				} else if strings.HasPrefix(trimmedPart, "maximum=") {
					if num, err := strconv.ParseFloat(strings.TrimPrefix(trimmedPart, "maximum="), 64); err == nil {
						propSchema["maximum"] = num
					}
				} // Add more tag parsing (enum, pattern, etc.)
			}
		}

		properties[fieldName] = propSchema
	}

	// Update required list in the main schema if it's not empty
	if len(required) > 0 {
		schema["required"] = required
	} else {
		// Remove the 'required' field if no fields are required
		delete(schema, "required")
	}

	return schema
}

// --- Helper to get underlying type and kind ---
func getUnderlyingType(t reflect.Type) (reflect.Type, reflect.Kind) {
	kind := t.Kind()
	if kind == reflect.Ptr {
		t = t.Elem()
		kind = t.Kind()
	}
	return t, kind
}

// ReflectType recursively gets the underlying element type for pointers and slices.
func ReflectType(t reflect.Type) reflect.Type {
	if t == nil {
		return nil
	}
	for t.Kind() == reflect.Ptr || t.Kind() == reflect.Slice {
		t = t.Elem()
	}
	return t
}
