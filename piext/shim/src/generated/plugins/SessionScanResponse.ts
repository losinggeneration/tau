// Original file: proto/plugin.proto

import type { SessionEntry as _plugins_SessionEntry, SessionEntry__Output as _plugins_SessionEntry__Output } from '../plugins/SessionEntry.ts';

export interface SessionScanResponse {
  'entries'?: (_plugins_SessionEntry)[];
  'error'?: (string);
}

export interface SessionScanResponse__Output {
  'entries'?: (_plugins_SessionEntry__Output)[];
  'error'?: (string);
}
