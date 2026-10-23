// Original file: proto/plugin.proto


export interface ToolProgressEvent {
  'toolCallId'?: (string);
  'content'?: (string);
  'isError'?: (boolean);
  'nestedCalls'?: (string);
}

export interface ToolProgressEvent__Output {
  'toolCallId'?: (string);
  'content'?: (string);
  'isError'?: (boolean);
  'nestedCalls'?: (string);
}
