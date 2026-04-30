// Original file: proto/plugin.proto

import type { CompactionResponse as _plugins_CompactionResponse, CompactionResponse__Output as _plugins_CompactionResponse__Output } from '../plugins/CompactionResponse.ts';

export interface EventResponse {
  'block'?: (boolean);
  'blockReason'?: (string);
  'responseData'?: (string);
  'compaction'?: (_plugins_CompactionResponse | null);
}

export interface EventResponse__Output {
  'block'?: (boolean);
  'blockReason'?: (string);
  'responseData'?: (string);
  'compaction'?: (_plugins_CompactionResponse__Output);
}
