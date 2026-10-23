// Original file: proto/plugin.proto


export interface SessionSendCustomMessageRequest {
  'messageJson'?: (string);
  'triggerTurn'?: (boolean);
  'deliverAs'?: (string);
  '_triggerTurn'?: "triggerTurn";
}

export interface SessionSendCustomMessageRequest__Output {
  'messageJson'?: (string);
  'triggerTurn'?: (boolean);
  'deliverAs'?: (string);
}
