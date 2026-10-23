// Original file: proto/plugin.proto


export interface ToolRegistration {
  'name'?: (string);
  'description'?: (string);
  'parameters'?: ({[key: string]: string});
  'exposure'?: (string);
  'outputSchema'?: (string);
  'constrainedSampling'?: (string);
  'namespace'?: (string);
  'defaultActive'?: (boolean);
  'hasReachesTools'?: (boolean);
  '_defaultActive'?: "defaultActive";
}

export interface ToolRegistration__Output {
  'name'?: (string);
  'description'?: (string);
  'parameters'?: ({[key: string]: string});
  'exposure'?: (string);
  'outputSchema'?: (string);
  'constrainedSampling'?: (string);
  'namespace'?: (string);
  'defaultActive'?: (boolean);
  'hasReachesTools'?: (boolean);
}
