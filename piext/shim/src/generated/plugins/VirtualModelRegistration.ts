// Original file: proto/plugin.proto

import type { Long } from '@grpc/proto-loader';

export interface VirtualModelRegistration {
  'provider'?: (string);
  'id'?: (string);
  'name'?: (string);
  'thinkingLevels'?: (string)[];
  'input'?: (string)[];
  'contextWindow'?: (number | string | Long);
  'maxTokens'?: (number | string | Long);
}

export interface VirtualModelRegistration__Output {
  'provider'?: (string);
  'id'?: (string);
  'name'?: (string);
  'thinkingLevels'?: (string)[];
  'input'?: (string)[];
  'contextWindow'?: (Long);
  'maxTokens'?: (Long);
}
