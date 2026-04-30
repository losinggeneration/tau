package ai // import "github.com/losinggeneration/tau/ai"

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/xeipuuv/gojsonschema"
)

// ValidationError represents a validation error with detailed information.
type ValidationError struct {
	ToolName string
	Field    string
	Message  string
	Received any
}

func (e *ValidationError) Error() string {
	if e.Field != "" {
		return fmt.Sprintf("  - %s: %s", e.Field, e.Message)
	}

	return e.Message
}

// ValidationResult contains the result of tool validation.
type ValidationResult struct {
	ToolName string
	Valid    bool
	Errors   []ValidationError
}

func (vr *ValidationResult) Error() string {
	if vr.Valid {
		return ""
	}

	var argsJSON []byte

	for _, err := range vr.Errors {
		if err.Received != nil {
			var jsonErr error
			argsJSON, jsonErr = json.MarshalIndent(err.Received, "", "  ")
			if jsonErr != nil {
				// Fallback to basic format if marshaling fails
				argsJSON = []byte(fmt.Sprintf("%v", err.Received))
			}
			break
		}
	}

	msg := fmt.Sprintf("Validation failed for tool \"%s\":\n", vr.ToolName)
	for _, err := range vr.Errors {
		msg += err.Error() + "\n"
	}

	if len(argsJSON) > 0 {
		msg += "\nReceived arguments:\n" + string(argsJSON)
	}

	return msg
}

// ValidateToolCall validates a tool call against registered tools and returns
// the validated arguments.
func ValidateToolCall(tools []Tool, toolCall ToolCall) (map[string]any, error) {
	// Find the tool
	var tool *Tool

	for i := range tools {
		if tools[i].Name == toolCall.Name {
			tool = &tools[i]

			break
		}
	}

	if tool == nil {
		return nil, fmt.Errorf("Tool %q not found", toolCall.Name)
	}

	args, result := validateToolArguments(tool, toolCall)
	if !result.Valid {
		return nil, errors.New(result.Error())
	}

	return args, nil
}

// ValidateToolArguments validates tool call arguments against the tool's JSON schema.
// It returns validated arguments plus a validation error when validation fails.
func ValidateToolArguments(tool *Tool, toolCall ToolCall) (map[string]any, error) {
	args, result := validateToolArguments(tool, toolCall)
	if !result.Valid {
		return nil, errors.New(result.Error())
	}

	return args, nil
}

func validateToolArguments(tool *Tool, toolCall ToolCall) (map[string]any, *ValidationResult) {
	result := &ValidationResult{
		ToolName: tool.Name,
		Valid:    true,
		Errors:   make([]ValidationError, 0),
	}

	// Parse the schema
	schemaBytes, err := json.Marshal(tool.Parameters)
	if err != nil {
		result.Valid = false
		result.Errors = append(result.Errors, ValidationError{
			Field:   "parameters",
			Message: fmt.Sprintf("invalid JSON schema: %v", err),
		})

		return nil, result
	}

	schemaLoader := gojsonschema.NewStringLoader(string(schemaBytes))

	schema, err := gojsonschema.NewSchema(schemaLoader)
	if err != nil {
		result.Valid = false
		result.Errors = append(result.Errors, ValidationError{
			Field:   "parameters",
			Message: fmt.Sprintf("invalid JSON schema: %v", err),
		})

		return nil, result
	}

	// Parse the arguments
	var args map[string]any
	if err := json.Unmarshal(toolCall.Arguments, &args); err != nil {
		result.Valid = false
		result.Errors = append(result.Errors, ValidationError{
			Field:    "arguments",
			Message:  fmt.Sprintf("invalid JSON: %v", err),
			Received: string(toolCall.Arguments),
		})

		return nil, result
	}

	args = coerceArguments(tool.Parameters, args)

	// Validate against schema
	argsLoader := gojsonschema.NewGoLoader(args)

	resultObj, err := schema.Validate(argsLoader)
	if err != nil {
		result.Valid = false
		result.Errors = append(result.Errors, ValidationError{
			Field:    "validation",
			Message:  fmt.Sprintf("validation error: %v", err),
			Received: args,
		})

		return nil, result
	}

	if !resultObj.Valid() {
		result.Valid = false

		for _, err := range resultObj.Errors() {
			field := err.Field()
			if field == "(root)" {
				field = ""
			}

			result.Errors = append(result.Errors, ValidationError{
				Field:    field,
				Message:  err.Description(),
				Received: args,
			})
		}
	}

	return args, result
}

func coerceArguments(schema json.RawMessage, args map[string]any) map[string]any {
	if len(schema) == 0 || len(args) == 0 {
		return args
	}

	var schemaMap map[string]any
	if err := json.Unmarshal(schema, &schemaMap); err != nil {
		return args
	}

	if coerced, ok := coerceValue(schemaMap, args).(map[string]any); ok {
		return coerced
	}

	return args
}

func coerceValue(schema map[string]any, value any) any {
	switch v := value.(type) {
	case map[string]any:
		return coerceObject(schema, v)
	case []any:
		return coerceArray(schema, v)
	default:
		return coerceScalar(schema, v)
	}
}

func coerceObject(schema map[string]any, value map[string]any) map[string]any {
	properties, _ := schema["properties"].(map[string]any)
	if len(properties) == 0 {
		return value
	}

	out := make(map[string]any, len(value))
	for k, v := range value {
		propSchema, _ := properties[k].(map[string]any)
		if propSchema == nil {
			out[k] = v
			continue
		}
		out[k] = coerceValue(propSchema, v)
	}

	return out
}

func coerceArray(schema map[string]any, value []any) []any {
	itemSchema, _ := schema["items"].(map[string]any)
	if itemSchema == nil {
		return value
	}

	out := make([]any, len(value))
	for i, item := range value {
		out[i] = coerceValue(itemSchema, item)
	}

	return out
}

func coerceScalar(schema map[string]any, value any) any {
	types := schemaTypes(schema)
	for _, typ := range types {
		if coerced, ok := coerceScalarToType(typ, value); ok {
			return coerced
		}
	}

	for _, branch := range []string{"anyOf", "oneOf"} {
		if schemas, ok := schema[branch].([]any); ok {
			for _, candidate := range schemas {
				candidateSchema, ok := candidate.(map[string]any)
				if !ok {
					continue
				}
				if coerced, ok := coerceScalarToKnownTypes(candidateSchema, value); ok {
					return coerced
				}
			}
		}
	}

	return value
}

func coerceScalarToKnownTypes(schema map[string]any, value any) (any, bool) {
	for _, typ := range schemaTypes(schema) {
		if coerced, ok := coerceScalarToType(typ, value); ok {
			return coerced, true
		}
	}

	return nil, false
}

func schemaTypes(schema map[string]any) []string {
	switch t := schema["type"].(type) {
	case string:
		return []string{t}
	case []any:
		types := make([]string, 0, len(t))
		for _, item := range t {
			if s, ok := item.(string); ok {
				types = append(types, s)
			}
		}
		return types
	default:
		return nil
	}
}

func coerceScalarToType(typ string, value any) (any, bool) {
	switch typ {
	case "string":
		if _, ok := value.(string); ok {
			return value, true
		}
		return fmt.Sprint(value), true
	case "integer":
		switch v := value.(type) {
		case float64:
			if float64(int64(v)) == v {
				return int(v), true
			}
		case string:
			n, err := strconv.Atoi(v)
			if err == nil {
				return n, true
			}
		}
	case "number":
		switch v := value.(type) {
		case float64:
			return v, true
		case string:
			n, err := strconv.ParseFloat(v, 64)
			if err == nil {
				return n, true
			}
		}
	case "boolean":
		switch v := value.(type) {
		case bool:
			return v, true
		case string:
			b, err := strconv.ParseBool(v)
			if err == nil {
				return b, true
			}
		}
	}

	return nil, false
}

// FormatToolsForProvider formats tools for a specific provider's API.
func FormatToolsForProvider(tools []Tool, api string) (any, error) {
	switch api {
	case APIAnthropicMessages:
		return formatToolsAnthropic(tools)
	case APIOpenAICompletions, APIOpenAIResponses:
		return formatToolsOpenAI(tools)
	default:
		// OpenAI-compatible format for other providers
		return formatToolsOpenAI(tools)
	}
}

// formatToolsAnthropic formats tools for Anthropic API.
func formatToolsAnthropic(tools []Tool) ([]map[string]any, error) {
	result := make([]map[string]any, len(tools))
	for i, tool := range tools {
		result[i] = map[string]any{
			"name":         tool.Name,
			"description":  tool.Description,
			"input_schema": tool.Parameters,
		}
	}

	return result, nil
}

// formatToolsOpenAI formats tools for OpenAI API.
func formatToolsOpenAI(tools []Tool) ([]map[string]any, error) {
	result := make([]map[string]any, len(tools))
	for i, tool := range tools {
		result[i] = map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        tool.Name,
				"description": tool.Description,
				"parameters":  tool.Parameters,
			},
		}
	}

	return result, nil
}

// ParseJSON parses JSON into a map.
func ParseJSON(data []byte) (map[string]any, error) {
	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, err
	}

	return result, nil
}
