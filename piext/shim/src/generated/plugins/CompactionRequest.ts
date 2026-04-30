// Original file: proto/plugin.proto

import type { Long } from '@grpc/proto-loader';

export interface CompactionRequest {
  'transcript'?: (string);
  'tokensBefore'?: (number | string | Long);
  'sessionId'?: (string);
}

export interface CompactionRequest__Output {
  'transcript'?: (string);
  'tokensBefore'?: (Long);
  'sessionId'?: (string);
}
