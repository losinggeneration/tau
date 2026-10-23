// Original file: proto/plugin.proto


export interface ToolResult {
  'output'?: (string);
  'isError'?: (boolean);
  'errorMessage'?: (string);
  'structuredContent'?: (string);
  'usage'?: (string);
  'content'?: (string);
  'nestedCalls'?: (string);
  'rejection'?: (string);
}

export interface ToolResult__Output {
  'output'?: (string);
  'isError'?: (boolean);
  'errorMessage'?: (string);
  'structuredContent'?: (string);
  'usage'?: (string);
  'content'?: (string);
  'nestedCalls'?: (string);
  'rejection'?: (string);
}
