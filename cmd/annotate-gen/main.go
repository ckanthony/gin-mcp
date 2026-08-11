package main

import (
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ckanthony/gin-mcp/pkg/convert"
	"github.com/ckanthony/gin-mcp/pkg/types"
)

func main() {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: annotate-gen [flags] <dir>\n\n")
		fmt.Fprintf(os.Stderr, "Scans Go source files in <dir> for handler annotation comments\n")
		fmt.Fprintf(os.Stderr, "(@summary, @description, @tags, @operationId, @param, @return, @body)\n")
		fmt.Fprintf(os.Stderr, "and struct definitions, then generates annotations_gen.go.\n\n")
		fmt.Fprintf(os.Stderr, "Flags:\n")
		flag.PrintDefaults()
	}
	outputFlag := flag.String("o", "", "Output file path (default: <dir>/annotations_gen.go)")
	flag.Parse()

	dir := "."
	if flag.NArg() > 0 {
		dir = flag.Arg(0)
	}

	dir, err := filepath.Abs(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	// Collect handler docs and struct metadata.
	docs := make(map[string]*convert.HandlerDoc)
	structs := make(map[string]*types.StructMeta)
	pkgName := ""

	err = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			base := d.Name()
			if base == "vendor" || base == ".git" || base == "node_modules" || base == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		// Skip generated files to avoid self-scanning.
		if strings.HasSuffix(path, "_gen.go") || strings.HasSuffix(path, ".gen.go") {
			return nil
		}

		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			return nil // skip unparseable files
		}

		if pkgName == "" {
			pkgName = f.Name.Name
		}

		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Doc == nil {
					continue
				}
				doc := convert.ParseAnnotationLines(d.Doc.Text())
				if isEmptyDoc(doc) {
					continue
				}
				docs[d.Name.Name] = doc

			case *ast.GenDecl:
				for _, spec := range d.Specs {
					ts, ok := spec.(*ast.TypeSpec)
					if !ok {
						continue
					}
					_, ok = ts.Type.(*ast.StructType)
					if !ok {
						continue
					}
					// Check for //ignore-mcp on the type declaration group or the spec itself.
					if hasIgnoreMCP(d.Doc) || hasIgnoreMCP(ts.Doc) || hasIgnoreMCP(ts.Comment) {
						continue
					}
					name := ts.Name.Name
					if existing, exists := structs[name]; exists {
						fmt.Fprintf(os.Stderr, "Warning: duplicate struct %q in %s (previous definition in package %s)\n",
							name, path, existing.Name)
						continue
					}
					sm := parseStructType(name, ts.Type.(*ast.StructType))
					if sm != nil {
						structs[name] = sm
					}
				}
			}
		}
		return nil
	})

	if err != nil {
		fmt.Fprintf(os.Stderr, "Error walking %s: %v\n", dir, err)
		os.Exit(1)
	}

	if pkgName == "" {
		pkgName = "main"
	}

	// Generate output.
	outPath := filepath.Join(dir, "annotations_gen.go")
	if *outputFlag != "" {
		outPath = *outputFlag
	}

	if err := writeGeneratedFile(outPath, pkgName, docs, structs); err != nil {
		fmt.Fprintf(os.Stderr, "Error writing %s: %v\n", outPath, err)
		os.Exit(1)
	}

	fmt.Printf("Generated %s (%d handlers, %d structs)\n", outPath, len(docs), len(structs))
}

// hasIgnoreMCP checks whether a comment group contains "ignore-mcp".
func hasIgnoreMCP(cg *ast.CommentGroup) bool {
	if cg == nil {
		return false
	}
	for _, c := range cg.List {
		if strings.Contains(c.Text, "ignore-mcp") {
			return true
		}
	}
	return false
}

// parseStructType extracts StructMeta from an AST struct type definition.
func parseStructType(name string, st *ast.StructType) *types.StructMeta {
	sm := &types.StructMeta{Name: name}
	if st.Fields == nil {
		return sm
	}

	for _, field := range st.Fields.List {
		// Skip fields with ignore-mcp comment.
		if hasIgnoreMCP(field.Doc) || hasIgnoreMCP(field.Comment) {
			continue
		}

		fm := parseField(field)
		if fm == nil {
			continue
		}
		sm.Fields = append(sm.Fields, *fm)
	}

	return sm
}

// parseField extracts FieldMeta from a single AST field.
func parseField(field *ast.Field) *types.FieldMeta {
	if len(field.Names) == 0 {
		return nil // embedded field, skip
	}

	fm := &types.FieldMeta{
		JSONName: field.Names[0].Name, // default to Go field name
		Type:     astTypeToJSON(field.Type),
	}

	// Parse struct tags.
	if field.Tag != nil {
		tag := field.Tag.Value
		// Strip surrounding backticks.
		tag = strings.Trim(tag, "`")
		tags := parseTagString(tag)

		if jsonTag, ok := tags["json"]; ok {
			parts := strings.Split(jsonTag, ",")
			if parts[0] == "-" {
				return nil // json:"-" → skip
			}
			fm.JSONName = parts[0]
		}

		if jsonschemaTag, ok := tags["jsonschema"]; ok {
			for _, part := range strings.Split(jsonschemaTag, ",") {
				part = strings.TrimSpace(part)
				if part == "required" {
					fm.Required = true
				} else if strings.HasPrefix(part, "description=") {
					fm.Description = strings.TrimPrefix(part, "description=")
				}
			}
		}
	}

	return fm
}

// parseTagString parses a raw struct tag string into a key-value map.
func parseTagString(tag string) map[string]string {
	result := make(map[string]string)
	for tag != "" {
		// Skip leading spaces.
		i := 0
		for i < len(tag) && tag[i] == ' ' {
			i++
		}
		tag = tag[i:]
		if tag == "" {
			break
		}

		// Find key.
		i = 0
		for i < len(tag) && tag[i] > ' ' && tag[i] != ':' && tag[i] != '"' && tag[i] != 0x7f {
			i++
		}
		if i == 0 || i+1 >= len(tag) || tag[i] != ':' {
			break
		}
		key := tag[:i]
		tag = tag[i+1:]

		// Find quoted value.
		if tag[0] != '"' {
			break
		}
		tag = tag[1:]
		i = 0
		for i < len(tag) && tag[i] != '"' {
			if tag[i] == '\\' {
				i++
			}
			i++
		}
		value := tag[:i]
		if i < len(tag) {
			tag = tag[i+1:]
		} else {
			tag = ""
		}
		result[key] = value
	}
	return result
}

// astTypeToJSON maps a Go AST type expression to a JSON schema type string.
func astTypeToJSON(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.Ident:
		switch t.Name {
		case "string":
			return "string"
		case "bool":
			return "boolean"
		case "int", "int8", "int16", "int32", "int64",
			"uint", "uint8", "uint16", "uint32", "uint64",
			"byte", "rune":
			return "integer"
		case "float32", "float64":
			return "number"
		default:
			return "string" // unknown named type, default to string
		}
	case *ast.StarExpr:
		return astTypeToJSON(t.X)
	case *ast.ArrayType:
		return "array"
	case *ast.MapType:
		return "object"
	case *ast.StructType:
		return "object"
	default:
		return "string"
	}
}

// isEmptyDoc returns true when doc carries no annotations.
func isEmptyDoc(doc *convert.HandlerDoc) bool {
	if doc == nil {
		return true
	}
	return doc.Summary == "" &&
		doc.Description == "" &&
		len(doc.Params) == 0 &&
		len(doc.Tags) == 0 &&
		doc.OperationID == "" &&
		doc.Returns == "" &&
		doc.BodyTypeName == ""
}

// writeGeneratedFile writes annotations_gen.go with both handler docs and struct metadata.
func writeGeneratedFile(path, pkgName string, docs map[string]*convert.HandlerDoc, structs map[string]*types.StructMeta) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	fmt.Fprintf(f, "// Code generated by annotate-gen; DO NOT EDIT.\n\n")
	fmt.Fprintf(f, "package %s\n\n", pkgName)
	fmt.Fprintf(f, "import (\n")
	fmt.Fprintf(f, "\t\"github.com/ckanthony/gin-mcp/pkg/convert\"\n")
	fmt.Fprintf(f, "\t\"github.com/ckanthony/gin-mcp/pkg/types\"\n")
	fmt.Fprintf(f, ")\n\n")
	fmt.Fprintf(f, "func init() {\n")

	// --- Handler annotations ---
	if len(docs) > 0 {
		names := sortedKeys(docs)
		fmt.Fprintf(f, "\tconvert.SetGeneratedAnnotations(map[string]*convert.HandlerDoc{\n")
		for _, name := range names {
			doc := docs[name]
			fmt.Fprintf(f, "\t\t%q: {\n", name)
			if doc.Summary != "" {
				fmt.Fprintf(f, "\t\t\tSummary: %q,\n", doc.Summary)
			}
			if doc.Description != "" {
				fmt.Fprintf(f, "\t\t\tDescription: %q,\n", doc.Description)
			}
			if doc.OperationID != "" {
				fmt.Fprintf(f, "\t\t\tOperationID: %q,\n", doc.OperationID)
			}
			if doc.Returns != "" {
				fmt.Fprintf(f, "\t\t\tReturns: %q,\n", doc.Returns)
			}
			if doc.BodyTypeName != "" {
				fmt.Fprintf(f, "\t\t\tBodyTypeName: %q,\n", doc.BodyTypeName)
			}
			if len(doc.Params) > 0 {
				paramKeys := sortedStrKeys(doc.Params)
				fmt.Fprintf(f, "\t\t\tParams: map[string]string{\n")
				for _, k := range paramKeys {
					fmt.Fprintf(f, "\t\t\t\t%q: %q,\n", k, doc.Params[k])
				}
				fmt.Fprintf(f, "\t\t\t},\n")
			}
			if len(doc.Tags) > 0 {
				fmt.Fprintf(f, "\t\t\tTags: []string{")
				for i, t := range doc.Tags {
					if i > 0 {
						fmt.Fprintf(f, ", ")
					}
					fmt.Fprintf(f, "%q", t)
				}
				fmt.Fprintf(f, "},\n")
			}
			fmt.Fprintf(f, "\t\t},\n")
		}
		fmt.Fprintf(f, "\t})\n")
	}

	// --- Struct metadata ---
	if len(structs) > 0 {
		if len(docs) > 0 {
			fmt.Fprintf(f, "\n")
		}
		names := sortedKeys(structs)
		fmt.Fprintf(f, "\tconvert.SetGeneratedStructs(map[string]*types.StructMeta{\n")
		for _, name := range names {
			sm := structs[name]
			fmt.Fprintf(f, "\t\t%q: {\n", name)
			fmt.Fprintf(f, "\t\t\tName: %q,\n", name)
			if len(sm.Fields) > 0 {
				fmt.Fprintf(f, "\t\t\tFields: []types.FieldMeta{\n")
				for _, fm := range sm.Fields {
					fmt.Fprintf(f, "\t\t\t\t{")
					fmt.Fprintf(f, "JSONName: %q, ", fm.JSONName)
					fmt.Fprintf(f, "Type: %q", fm.Type)
					if fm.Description != "" {
						fmt.Fprintf(f, ", Description: %q", fm.Description)
					}
					if fm.Required {
						fmt.Fprintf(f, ", Required: true")
					}
					fmt.Fprintf(f, "},\n")
				}
				fmt.Fprintf(f, "\t\t\t},\n")
			}
			fmt.Fprintf(f, "\t\t},\n")
		}
		fmt.Fprintf(f, "\t})\n")
	}

	fmt.Fprintf(f, "}\n")
	return nil
}

// sortedKeys returns sorted keys from a map of string→T.
func sortedKeys[T any](m map[string]T) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// sortedStrKeys returns sorted keys from a map[string]string.
func sortedStrKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
