package ai

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// constrainedSampling decodes a tool's constrained-sampling request; nil
// means the tool asked for none.
func constrainedSampling(tool Tool) (*ConstrainedSamplingConfig, error) {
	if len(tool.ConstrainedSampling) == 0 || string(tool.ConstrainedSampling) == "false" || string(tool.ConstrainedSampling) == "null" {
		return nil, nil
	}
	var config ConstrainedSamplingConfig
	if err := json.Unmarshal(tool.ConstrainedSampling, &config); err != nil {
		return nil, fmt.Errorf("tool %q constrained sampling: %w", tool.Name, err)
	}
	return &config, nil
}

// strictToolParameters returns the tool's parameters, converted to the strict
// subset when it asks for JSON-schema constrained sampling and the provider
// supports it. A required tool fails when strictness is impossible; a
// preferred one falls back to its plain schema.
func strictToolParameters(tool Tool, supported bool, unsupported func(string, any) bool) (json.RawMessage, bool, error) {
	config, err := constrainedSampling(tool)
	if err != nil {
		return nil, false, err
	}
	if config == nil || config.Type != "json_schema" {
		return tool.Parameters, false, nil
	}
	if config.Strict != "prefer" && config.Strict != "require" {
		return nil, false, fmt.Errorf("tool %q: invalid strict preference %q", tool.Name, config.Strict)
	}
	var schema map[string]any
	if !supported {
		err = errors.New("strict tools are unsupported")
	} else if err = json.Unmarshal(tool.Parameters, &schema); err == nil {
		err = makeStrictSchema(schema, unsupported)
		if err == nil && schema["type"] != "object" {
			err = errors.New("root schema must have type object")
		}
	}
	if err != nil {
		if config.Strict == "require" {
			return nil, false, fmt.Errorf("tool %q requires JSON-schema constrained sampling: %w", tool.Name, err)
		}
		return tool.Parameters, false, nil
	}
	encoded, err := json.Marshal(schema)
	if err != nil {
		return nil, false, fmt.Errorf("encode strict schema: %w", err)
	}
	return encoded, true, nil
}

// grammarTool is an OpenAI custom tool whose whole input is sampled from a
// grammar and delivered as one string argument.
type grammarTool struct {
	syntax     string
	definition string
	property   string
}

// resolveGrammarTool returns the grammar for a tool that asks for grammar
// constrained sampling, or nil when it does not or the provider lacks
// grammar tools (the tool is then sent with its JSON schema).
func resolveGrammarTool(tool Tool, supported bool) (*grammarTool, error) {
	config, err := constrainedSampling(tool)
	if err != nil || config == nil || config.Type != "grammar" || !supported {
		return nil, err
	}
	grammar := &grammarTool{syntax: "lark", definition: config.Variants["openai_lark"]}
	if strings.TrimSpace(grammar.definition) == "" {
		grammar = &grammarTool{syntax: "regex", definition: config.Variants["openai_regex"]}
	}
	if strings.TrimSpace(grammar.definition) == "" {
		return nil, fmt.Errorf("tool %q cannot use grammar constrained sampling: no supported grammar variant was provided", tool.Name)
	}
	var schema struct {
		Type       string `json:"type"`
		Required   []any  `json:"required"`
		Properties map[string]struct {
			Type any `json:"type"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(tool.Parameters, &schema); err != nil || schema.Type != "object" {
		return nil, fmt.Errorf("tool %q cannot use grammar constrained sampling: grammar constrained sampling requires an object parameter schema", tool.Name)
	}
	name, ok := "", len(schema.Required) == 1
	if ok {
		name, ok = schema.Required[0].(string)
	}
	if !ok {
		return nil, fmt.Errorf("tool %q cannot use grammar constrained sampling: grammar constrained sampling requires exactly one required string property", tool.Name)
	}
	property, ok := schema.Properties[name]
	if !ok {
		return nil, fmt.Errorf("tool %q cannot use grammar constrained sampling: grammar constrained sampling requires a properties entry for %s", tool.Name, name)
	}
	if property.Type != "string" {
		return nil, fmt.Errorf("tool %q cannot use grammar constrained sampling: grammar constrained sampling property %s must have type string", tool.Name, name)
	}
	grammar.property = name
	return grammar, nil
}

// grammarInputProperties maps each grammar tool's name to the argument that
// carries its raw input.
func grammarInputProperties(tools []Tool, supported bool) (map[string]string, error) {
	properties := map[string]string{}
	for _, tool := range tools {
		grammar, err := resolveGrammarTool(tool, supported)
		if err != nil {
			return nil, err
		}
		if grammar != nil {
			properties[tool.Name] = grammar.property
		}
	}
	return properties, nil
}

// grammarToolInput returns the raw input a grammar tool call carries in
// property, for replay as a custom tool call.
func grammarToolInput(name string, arguments json.RawMessage, property string) (string, error) {
	var args map[string]any
	if err := json.Unmarshal(arguments, &args); err == nil {
		if input, ok := args[property].(string); ok {
			return input, nil
		}
	}
	return "", fmt.Errorf("grammar tool call %q requires argument %q to be a string", name, property)
}

// grammarInputBuffer turns a custom tool call's raw input stream into JSON
// argument deltas of the form {"property":"input"}.
type grammarInputBuffer struct {
	property string
	input    string
	started  bool
	closed   bool
}

func newGrammarInputBuffer(property string) *grammarInputBuffer {
	return &grammarInputBuffer{property: property}
}

// append records the input seen so far and returns the JSON delta to emit;
// closing ends the object. Input must only grow.
func (b *grammarInputBuffer) append(next string, closing bool) (string, error) {
	if b.closed {
		if closing && next == b.input {
			return "", nil
		}
		return "", fmt.Errorf("grammar tool input for property %q changed after it was closed", b.property)
	}
	if !strings.HasPrefix(next, b.input) {
		return "", fmt.Errorf("grammar tool input for property %q changed non-monotonically", b.property)
	}
	added := next[len(b.input):]
	if !closing && added == "" {
		return "", nil
	}
	var delta string
	if !b.started {
		delta = "{" + jsonQuote(b.property) + `:"`
		b.started = true
	}
	quoted := jsonQuote(added)
	delta += quoted[1 : len(quoted)-1]
	b.input = next
	if closing {
		delta += `"}`
		b.closed = true
	}
	return delta, nil
}

// finish closes the JSON object, returning the final delta.
func (b *grammarInputBuffer) finish() string {
	delta, err := b.append(b.input, true)
	if err != nil {
		// Unreachable: closing with the recorded input never conflicts.
		return ""
	}
	return delta
}

// arguments returns the call's arguments for the input seen so far.
func (b *grammarInputBuffer) arguments() json.RawMessage {
	return json.RawMessage("{" + jsonQuote(b.property) + ":" + jsonQuote(b.input) + "}")
}

// jsonQuote encodes s as a JSON string.
func jsonQuote(s string) string {
	encoded, _ := json.Marshal(s) // encoding a string cannot fail
	return string(encoded)
}

func makeStrictSchema(value any, unsupported func(string, any) bool) error {
	schema, ok := value.(map[string]any)
	if !ok || schema == nil {
		return fmt.Errorf("boolean or null schemas are unsupported")
	}
	for key, value := range schema {
		if slices.Contains([]string{"$ref", "$defs", "definitions", "allOf", "oneOf", "patternProperties", "dependentSchemas", "dependencies", "unevaluatedProperties", "propertyNames", "contains", "prefixItems", "not", "if", "then", "else"}, key) || unsupported != nil && unsupported(key, value) {
			return fmt.Errorf("unsupported schema keyword %s", key)
		}
	}
	if variants, exists := schema["anyOf"]; exists {
		values, ok := variants.([]any)
		if !ok || len(values) == 0 {
			return fmt.Errorf("anyOf must contain schemas")
		}
		for _, variant := range values {
			if object, ok := variant.(map[string]any); ok && (object["type"] == "object" || object["type"] == "array" || object["properties"] != nil || object["items"] != nil) {
				return fmt.Errorf("structured unions are unsupported")
			}
			if err := makeStrictSchema(variant, unsupported); err != nil {
				return err
			}
		}
	}
	if items, exists := schema["items"]; exists {
		if err := makeStrictSchema(items, unsupported); err != nil {
			return err
		}
	}
	if schema["type"] != "object" {
		if _, exists := schema["properties"]; exists {
			return fmt.Errorf("properties require type object")
		}
		return nil
	}
	if additional, exists := schema["additionalProperties"]; exists && additional != false {
		return fmt.Errorf("additionalProperties must be false")
	}
	properties := map[string]any{}
	if value, exists := schema["properties"]; exists {
		var ok bool
		properties, ok = value.(map[string]any)
		if !ok {
			return fmt.Errorf("properties must be a schema map")
		}
	}
	required := map[string]bool{}
	if value, exists := schema["required"]; exists {
		keys, ok := value.([]any)
		if !ok {
			return fmt.Errorf("required must be a string array")
		}
		for _, key := range keys {
			name, ok := key.(string)
			if !ok {
				return fmt.Errorf("required must be a string array")
			}
			if _, exists := properties[name]; !exists {
				return fmt.Errorf("unknown required property %s", name)
			}
			required[name] = true
		}
	}
	names := make([]string, 0, len(properties))
	for name, property := range properties {
		if err := makeStrictSchema(property, unsupported); err != nil {
			return err
		}
		if !required[name] && !schemaAllowsNull(property) {
			properties[name] = map[string]any{"anyOf": []any{property, map[string]any{"type": "null"}}}
		}
		names = append(names, name)
	}
	slices.Sort(names)
	schema["required"] = names
	schema["additionalProperties"] = false
	return nil
}

func schemaAllowsNull(value any) bool {
	schema, ok := value.(map[string]any)
	if !ok {
		return false
	}
	if schema["type"] == "null" {
		return true
	}
	if value, exists := schema["const"]; exists && value == nil {
		return true
	}
	for _, key := range []string{"type", "enum", "anyOf"} {
		if values, ok := schema[key].([]any); ok {
			for _, value := range values {
				if key == "type" && value == "null" || key == "enum" && value == nil || key == "anyOf" && schemaAllowsNull(value) {
					return true
				}
			}
		}
	}
	return false
}

func anthropicUnsupportedStrictKeyword(key string, value any) bool {
	switch key {
	case "minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum", "multipleOf", "maxItems", "uniqueItems", "minContains", "maxContains", "minProperties", "maxProperties":
		return true
	case "minItems":
		return value != float64(0) && value != float64(1)
	case "format":
		format, ok := value.(string)
		return !ok || !slices.Contains([]string{"date-time", "time", "date", "duration", "email", "hostname", "uri", "ipv4", "ipv6", "uuid"}, format)
	}
	return false
}
