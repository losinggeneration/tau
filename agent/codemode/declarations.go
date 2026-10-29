package codemode

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

const (
	// DefaultInputSchemaMaxChars is the longest rendered input type before it
	// becomes `unknown`.
	DefaultInputSchemaMaxChars = 16000
	maxRefExpansions           = 32
	maxTagChars                = 80
	indent                     = "  "
)

var (
	tagKeywords = []string{"minimum", "exclusiveMinimum", "maximum", "exclusiveMaximum", "multipleOf", "minLength", "maxLength", "pattern", "format", "minItems", "maxItems"}
	lowerBounds = map[string]bool{"minimum": true, "exclusiveMinimum": true, "minLength": true, "minItems": true}
	upperBounds = map[string]bool{"maximum": true, "exclusiveMaximum": true, "maxLength": true, "maxItems": true}
)

// ToolSignature is a tool as a member of `tools`: `name(args: T): Promise<R>;`.
func ToolSignature(name string, inputSchema, outputSchema any) string {
	input := "unknown"
	if inputSchema != nil {
		input = SchemaToType(inputSchema, DefaultInputSchemaMaxChars)
	}
	return fmt.Sprintf("%s(args: %s): Promise<%s>;", Identifier(name), input, OutputType(outputSchema))
}

// ToolSample is a tool's description followed by its declaration, as tool
// listings and ALL_TOOLS show it.
func ToolSample(name, description string, inputSchema, outputSchema any) string {
	return fmt.Sprintf("%s\n\ncodemode tool declaration:\n```ts\ndeclare const tools: { %s };\n```", strings.TrimSpace(description), ToolSignature(name, inputSchema, outputSchema))
}

// OutputType is the type a call resolves to, `unknown` without a schema.
func OutputType(schema any) string {
	if schema == nil {
		return "unknown"
	}
	return SchemaToType(schema, 0)
}

// SchemaToType renders a JSON schema as a TypeScript type. Local references
// resolve against the schema; recursive and remote ones become `unknown`.
// Past maxChars (0 for no limit) it renders again without constraint tags,
// then gives up with `unknown`.
func SchemaToType(schema any, maxChars int) string {
	render := func(tags bool) string {
		return (&schemaContext{root: schema, resolving: map[string]bool{}, tags: tags}).toType(schema)
	}
	out := render(true)
	if maxChars <= 0 || len(out) <= maxChars {
		return out
	}
	if out = render(false); len(out) <= maxChars {
		return out
	}
	return "unknown"
}

type schemaContext struct {
	root       any
	resolving  map[string]bool
	expansions int
	tags       bool
}

func (c *schemaContext) toType(schema any) string {
	switch schema {
	case true:
		return "unknown"
	case false:
		return "never"
	}
	s, ok := schema.(map[string]any)
	if !ok {
		return "unknown"
	}
	if ref, ok := s["$ref"].(string); ok {
		if c.resolving[ref] || c.expansions >= maxRefExpansions {
			return "unknown"
		}
		target, ok := resolveRef(ref, c.root)
		if !ok {
			return "unknown"
		}
		c.expansions++
		c.resolving[ref] = true
		defer delete(c.resolving, ref)
		return c.toType(target)
	}
	if value, ok := s["const"]; ok {
		return jsonText(value)
	}
	if values, ok := list(s["enum"]); ok {
		var types []string
		for _, value := range values {
			types = append(types, jsonText(value))
		}
		return union(types)
	}
	variants, ok := list(s["anyOf"])
	if !ok {
		variants, ok = list(s["oneOf"])
	}
	if ok {
		var types []string
		for _, variant := range variants {
			types = append(types, c.toType(variant))
		}
		return union(types)
	}
	if parts, ok := list(s["allOf"]); ok {
		var rendered []string
		for _, part := range parts {
			if t := c.toType(part); t != "unknown" {
				if strings.Contains(t, " | ") {
					t = "(" + t + ")"
				}
				rendered = append(rendered, t)
			}
		}
		if len(rendered) == 0 {
			return "unknown"
		}
		return strings.Join(rendered, " & ")
	}
	if types, ok := list(s["type"]); ok {
		var rendered []string
		for _, entry := range types {
			variant := map[string]any{}
			for k, v := range s {
				variant[k] = v
			}
			variant["type"] = entry
			rendered = append(rendered, c.toType(variant))
		}
		return union(rendered)
	}
	switch s["type"] {
	case "string":
		return "string"
	case "number", "integer":
		return "number"
	case "boolean":
		return "boolean"
	case "null":
		return "null"
	case "array":
		return c.arrayType(s)
	case "object":
		return c.objectType(s)
	case nil:
		if has(s, "properties", "additionalProperties", "required") {
			return c.objectType(s)
		}
		if has(s, "items", "prefixItems") {
			return c.arrayType(s)
		}
	}
	return "unknown"
}

func (c *schemaContext) arrayType(s map[string]any) string {
	if items, ok := s["items"]; ok {
		if _, isList := list(items); !isList {
			return "Array<" + c.toType(items) + ">"
		}
	}
	tuple, ok := list(s["prefixItems"])
	if !ok {
		tuple, _ = list(s["items"])
	}
	if len(tuple) == 0 {
		return "unknown[]"
	}
	var types []string
	for _, item := range tuple {
		types = append(types, c.toType(item))
	}
	return "[" + strings.Join(types, ", ") + "]"
}

func (c *schemaContext) objectType(s map[string]any) string {
	properties, _ := s["properties"].(map[string]any)
	required := map[string]bool{}
	if names, ok := list(s["required"]); ok {
		for _, name := range names {
			if name, ok := name.(string); ok {
				required[name] = true
			}
		}
	}
	names := make([]string, 0, len(properties))
	for name := range properties {
		names = append(names, name)
	}
	slices.Sort(names)
	var members []string
	for _, name := range names {
		optional := "?"
		if required[name] {
			optional = ""
		}
		members = append(members, fmt.Sprintf("%s%s: %s;", propertyKey(name), optional, c.toType(properties[name])))
	}
	additional, hasAdditional := s["additionalProperties"]
	switch {
	case hasAdditional && additional != false:
		t := "unknown"
		if additional != true {
			t = c.toType(additional)
		}
		members = append(members, "[key: string]: "+t+";")
	case !hasAdditional && len(names) == 0:
		members = append(members, "[key: string]: unknown;")
	}
	if len(members) == 0 {
		return "{}"
	}
	comments := make([][]string, len(names))
	commented := false
	for i, name := range names {
		property, _ := properties[name].(map[string]any)
		description, _ := property["description"].(string)
		for _, line := range strings.FieldsFunc(description, isLineTerminator) {
			if line = strings.TrimSpace(line); line != "" {
				comments[i] = append(comments[i], line)
			}
		}
		if c.tags {
			if tags := constraintTags(property); tags != "" {
				comments[i] = append(comments[i], tags)
			}
		}
		commented = commented || len(comments[i]) > 0
	}
	if !commented {
		return "{ " + strings.Join(members, " ") + " }"
	}
	lines := []string{"{"}
	for i, comment := range comments {
		for _, line := range comment {
			lines = append(lines, indent+"// "+line)
		}
		lines = append(lines, indent+strings.ReplaceAll(members[i], "\n", "\n"+indent))
	}
	for _, member := range members[len(names):] {
		lines = append(lines, indent+member)
	}
	return strings.Join(append(lines, "}"), "\n")
}

// constraintTags lists what a property's type can't express, like
// `@integer @minimum 1 @default 20`. Nullable unions read the non-null branch too.
func constraintTags(property map[string]any) string {
	if property == nil {
		return ""
	}
	variants, ok := list(property["anyOf"])
	if !ok {
		variants, _ = list(property["oneOf"])
	}
	var nonNull []any
	for _, variant := range variants {
		if v, _ := variant.(map[string]any); v == nil || v["type"] != "null" {
			nonNull = append(nonNull, variant)
		}
	}
	schemas := []map[string]any{property}
	if len(nonNull) == 1 {
		if branch, ok := nonNull[0].(map[string]any); ok {
			schemas = append(schemas, branch)
		}
	}
	// Draft-04 boolean exclusive bounds make minimum/maximum exclusive.
	for i, schema := range schemas {
		copied := map[string]any{}
		for k, v := range schema {
			copied[k] = v
		}
		if schema["exclusiveMinimum"] == true {
			copied["minimum"], copied["exclusiveMinimum"] = nil, schema["minimum"]
		}
		if schema["exclusiveMaximum"] == true {
			copied["maximum"], copied["exclusiveMaximum"] = nil, schema["maximum"]
		}
		schemas[i] = copied
	}

	var tags []string
	if slices.ContainsFunc(schemas, func(s map[string]any) bool { return isIntegerType(s["type"]) }) {
		tags = append(tags, "@integer")
	}
	for _, keyword := range tagKeywords {
		if lowerBounds[keyword] || upperBounds[keyword] {
			var numbers []float64
			for _, schema := range schemas {
				if n, ok := number(schema[keyword]); ok {
					numbers = append(numbers, n)
				}
			}
			if len(numbers) == 0 {
				continue
			}
			bound := slices.Min(numbers)
			if lowerBounds[keyword] {
				bound = slices.Max(numbers)
			}
			tags = append(tags, "@"+keyword+" "+jsNumber(bound))
			continue
		}
		var seen []string
		for _, schema := range schemas {
			var text string
			if n, ok := number(schema[keyword]); ok {
				text = jsNumber(n)
			} else if s, ok := schema[keyword].(string); ok {
				text = s
			} else {
				continue
			}
			if !slices.Contains(seen, text) {
				seen = append(seen, text)
				tags = append(tags, "@"+keyword+" "+text)
			}
		}
	}
	if slices.ContainsFunc(schemas, func(s map[string]any) bool { return s["uniqueItems"] == true }) {
		tags = append(tags, "@uniqueItems")
	}
	if value, ok := property["default"]; ok && value != nil {
		tags = append(tags, "@default "+jsonText(value))
	}
	if property["deprecated"] == true {
		tags = append(tags, "@deprecated")
	}
	var kept []string
	for _, tag := range tags {
		if len([]rune(tag)) > maxTagChars {
			continue
		}
		var b strings.Builder
		for _, r := range tag {
			if isLineTerminator(r) {
				fmt.Fprintf(&b, `\u%04x`, r)
			} else {
				b.WriteRune(r)
			}
		}
		kept = append(kept, b.String())
	}
	return strings.Join(kept, " ")
}

func resolveRef(ref string, root any) (any, bool) {
	if ref != "#" && !strings.HasPrefix(ref, "#/") {
		return nil, false
	}
	current := root
	for _, segment := range strings.Split(strings.TrimPrefix(ref, "#"), "/") {
		if segment == "" {
			continue
		}
		key, err := url.PathUnescape(segment)
		if err != nil {
			return nil, false
		}
		key = strings.ReplaceAll(strings.ReplaceAll(key, "~1", "/"), "~0", "~")
		object, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		if current, ok = object[key]; !ok {
			return nil, false
		}
	}
	switch current.(type) {
	case bool, map[string]any:
		return current, true
	}
	return nil, false
}

func union(types []string) string {
	var unique []string
	for _, t := range types {
		if t == "unknown" {
			return "unknown"
		}
		if !slices.Contains(unique, t) {
			unique = append(unique, t)
		}
	}
	if len(unique) == 0 {
		return "never"
	}
	return strings.Join(unique, " | ")
}

func propertyKey(name string) string {
	if identifierPattern.MatchString(name) {
		return name
	}
	return jsonText(name)
}

func has(s map[string]any, keys ...string) bool {
	return slices.ContainsFunc(keys, func(key string) bool { _, ok := s[key]; return ok })
}

func isLineTerminator(r rune) bool {
	return r == '\r' || r == '\n' || r == ' ' || r == ' '
}

func isIntegerType(t any) bool {
	if t == "integer" {
		return true
	}
	types, _ := list(t)
	return slices.Contains(types, any("integer"))
}

// list accepts the slice shapes Go-built and JSON-decoded schemas use.
func list(v any) ([]any, bool) {
	switch v := v.(type) {
	case []any:
		return v, true
	case []string:
		out := make([]any, len(v))
		for i, s := range v {
			out[i] = s
		}
		return out, true
	case []map[string]any:
		out := make([]any, len(v))
		for i, s := range v {
			out[i] = s
		}
		return out, true
	}
	return nil, false
}

func number(v any) (float64, bool) {
	switch v := v.(type) {
	case float64:
		return v, true
	case float32:
		return float64(v), true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case json.Number:
		f, err := v.Float64()
		return f, err == nil
	}
	return 0, false
}

func jsNumber(f float64) string {
	if math.Abs(f) < 1e21 {
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// jsonText is JSON.stringify: no HTML escaping, `unknown` for what JSON
// can't hold.
func jsonText(v any) string {
	var b bytes.Buffer
	encoder := json.NewEncoder(&b)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(v); err != nil {
		return "unknown"
	}
	return strings.TrimSuffix(b.String(), "\n")
}
