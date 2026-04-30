package ai

// TSchema is a small Go alias for JSON-schema-like objects accepted by tool
// validation. It exists as an API-shape counterpart to Pi's TypeBox TSchema.
type TSchema = map[string]any

// StringEnum builds the JSON Schema shape emitted by Pi's TypeBox StringEnum
// helper: a string schema with an enum array, plus any caller-supplied schema
// options such as description or default.
func StringEnum(values []string, options ...map[string]any) TSchema {
	schema := TSchema{"type": "string"}
	if len(options) > 0 && options[0] != nil {
		for k, v := range options[0] {
			schema[k] = v
		}
	}
	enum := make([]any, len(values))
	for i, value := range values {
		enum[i] = value
	}
	schema["enum"] = enum
	return schema
}
