// Original file: proto/plugin.proto

import type * as grpc from '@grpc/grpc-js'
import type { MethodDefinition } from '@grpc/proto-loader'
import type { GetConfigRequest as _plugins_GetConfigRequest, GetConfigRequest__Output as _plugins_GetConfigRequest__Output } from '../plugins/GetConfigRequest.ts';
import type { GetConfigResponse as _plugins_GetConfigResponse, GetConfigResponse__Output as _plugins_GetConfigResponse__Output } from '../plugins/GetConfigResponse.ts';
import type { LogRequest as _plugins_LogRequest, LogRequest__Output as _plugins_LogRequest__Output } from '../plugins/LogRequest.ts';
import type { LogResponse as _plugins_LogResponse, LogResponse__Output as _plugins_LogResponse__Output } from '../plugins/LogResponse.ts';
import type { SessionAppendRequest as _plugins_SessionAppendRequest, SessionAppendRequest__Output as _plugins_SessionAppendRequest__Output } from '../plugins/SessionAppendRequest.ts';
import type { SessionAppendResponse as _plugins_SessionAppendResponse, SessionAppendResponse__Output as _plugins_SessionAppendResponse__Output } from '../plugins/SessionAppendResponse.ts';
import type { SessionCompactRequest as _plugins_SessionCompactRequest, SessionCompactRequest__Output as _plugins_SessionCompactRequest__Output } from '../plugins/SessionCompactRequest.ts';
import type { SessionCompactResponse as _plugins_SessionCompactResponse, SessionCompactResponse__Output as _plugins_SessionCompactResponse__Output } from '../plugins/SessionCompactResponse.ts';
import type { SessionGetLabelRequest as _plugins_SessionGetLabelRequest, SessionGetLabelRequest__Output as _plugins_SessionGetLabelRequest__Output } from '../plugins/SessionGetLabelRequest.ts';
import type { SessionGetLabelResponse as _plugins_SessionGetLabelResponse, SessionGetLabelResponse__Output as _plugins_SessionGetLabelResponse__Output } from '../plugins/SessionGetLabelResponse.ts';
import type { SessionScanRequest as _plugins_SessionScanRequest, SessionScanRequest__Output as _plugins_SessionScanRequest__Output } from '../plugins/SessionScanRequest.ts';
import type { SessionScanResponse as _plugins_SessionScanResponse, SessionScanResponse__Output as _plugins_SessionScanResponse__Output } from '../plugins/SessionScanResponse.ts';
import type { SessionSendCustomMessageRequest as _plugins_SessionSendCustomMessageRequest, SessionSendCustomMessageRequest__Output as _plugins_SessionSendCustomMessageRequest__Output } from '../plugins/SessionSendCustomMessageRequest.ts';
import type { SessionSendUserMessageRequest as _plugins_SessionSendUserMessageRequest, SessionSendUserMessageRequest__Output as _plugins_SessionSendUserMessageRequest__Output } from '../plugins/SessionSendUserMessageRequest.ts';
import type { SessionSendUserMessageResponse as _plugins_SessionSendUserMessageResponse, SessionSendUserMessageResponse__Output as _plugins_SessionSendUserMessageResponse__Output } from '../plugins/SessionSendUserMessageResponse.ts';
import type { SessionSetLabelRequest as _plugins_SessionSetLabelRequest, SessionSetLabelRequest__Output as _plugins_SessionSetLabelRequest__Output } from '../plugins/SessionSetLabelRequest.ts';
import type { SessionSetLabelResponse as _plugins_SessionSetLabelResponse, SessionSetLabelResponse__Output as _plugins_SessionSetLabelResponse__Output } from '../plugins/SessionSetLabelResponse.ts';
import type { StateAppendRequest as _plugins_StateAppendRequest, StateAppendRequest__Output as _plugins_StateAppendRequest__Output } from '../plugins/StateAppendRequest.ts';
import type { StateAppendResponse as _plugins_StateAppendResponse, StateAppendResponse__Output as _plugins_StateAppendResponse__Output } from '../plugins/StateAppendResponse.ts';
import type { StateDeleteRequest as _plugins_StateDeleteRequest, StateDeleteRequest__Output as _plugins_StateDeleteRequest__Output } from '../plugins/StateDeleteRequest.ts';
import type { StateDeleteResponse as _plugins_StateDeleteResponse, StateDeleteResponse__Output as _plugins_StateDeleteResponse__Output } from '../plugins/StateDeleteResponse.ts';
import type { StateGetRequest as _plugins_StateGetRequest, StateGetRequest__Output as _plugins_StateGetRequest__Output } from '../plugins/StateGetRequest.ts';
import type { StateGetResponse as _plugins_StateGetResponse, StateGetResponse__Output as _plugins_StateGetResponse__Output } from '../plugins/StateGetResponse.ts';
import type { StateSetRequest as _plugins_StateSetRequest, StateSetRequest__Output as _plugins_StateSetRequest__Output } from '../plugins/StateSetRequest.ts';
import type { StateSetResponse as _plugins_StateSetResponse, StateSetResponse__Output as _plugins_StateSetResponse__Output } from '../plugins/StateSetResponse.ts';
import type { ToolDiscoveryRequest as _plugins_ToolDiscoveryRequest, ToolDiscoveryRequest__Output as _plugins_ToolDiscoveryRequest__Output } from '../plugins/ToolDiscoveryRequest.ts';
import type { ToolDiscoveryResponse as _plugins_ToolDiscoveryResponse, ToolDiscoveryResponse__Output as _plugins_ToolDiscoveryResponse__Output } from '../plugins/ToolDiscoveryResponse.ts';
import type { ToolProgressEvent as _plugins_ToolProgressEvent, ToolProgressEvent__Output as _plugins_ToolProgressEvent__Output } from '../plugins/ToolProgressEvent.ts';
import type { ToolProgressResponse as _plugins_ToolProgressResponse, ToolProgressResponse__Output as _plugins_ToolProgressResponse__Output } from '../plugins/ToolProgressResponse.ts';
import type { UIConfirmRequest as _plugins_UIConfirmRequest, UIConfirmRequest__Output as _plugins_UIConfirmRequest__Output } from '../plugins/UIConfirmRequest.ts';
import type { UIConfirmResponse as _plugins_UIConfirmResponse, UIConfirmResponse__Output as _plugins_UIConfirmResponse__Output } from '../plugins/UIConfirmResponse.ts';
import type { UIInputRequest as _plugins_UIInputRequest, UIInputRequest__Output as _plugins_UIInputRequest__Output } from '../plugins/UIInputRequest.ts';
import type { UIInputResponse as _plugins_UIInputResponse, UIInputResponse__Output as _plugins_UIInputResponse__Output } from '../plugins/UIInputResponse.ts';
import type { UINotifyRequest as _plugins_UINotifyRequest, UINotifyRequest__Output as _plugins_UINotifyRequest__Output } from '../plugins/UINotifyRequest.ts';
import type { UINotifyResponse as _plugins_UINotifyResponse, UINotifyResponse__Output as _plugins_UINotifyResponse__Output } from '../plugins/UINotifyResponse.ts';
import type { UISelectRequest as _plugins_UISelectRequest, UISelectRequest__Output as _plugins_UISelectRequest__Output } from '../plugins/UISelectRequest.ts';
import type { UISelectResponse as _plugins_UISelectResponse, UISelectResponse__Output as _plugins_UISelectResponse__Output } from '../plugins/UISelectResponse.ts';
import type { UISetEditorTextRequest as _plugins_UISetEditorTextRequest, UISetEditorTextRequest__Output as _plugins_UISetEditorTextRequest__Output } from '../plugins/UISetEditorTextRequest.ts';
import type { UISetEditorTextResponse as _plugins_UISetEditorTextResponse, UISetEditorTextResponse__Output as _plugins_UISetEditorTextResponse__Output } from '../plugins/UISetEditorTextResponse.ts';
import type { UISetStatusRequest as _plugins_UISetStatusRequest, UISetStatusRequest__Output as _plugins_UISetStatusRequest__Output } from '../plugins/UISetStatusRequest.ts';
import type { UISetStatusResponse as _plugins_UISetStatusResponse, UISetStatusResponse__Output as _plugins_UISetStatusResponse__Output } from '../plugins/UISetStatusResponse.ts';
import type { UISetTitleRequest as _plugins_UISetTitleRequest, UISetTitleRequest__Output as _plugins_UISetTitleRequest__Output } from '../plugins/UISetTitleRequest.ts';
import type { UISetTitleResponse as _plugins_UISetTitleResponse, UISetTitleResponse__Output as _plugins_UISetTitleResponse__Output } from '../plugins/UISetTitleResponse.ts';
import type { UISetToolsExpandedRequest as _plugins_UISetToolsExpandedRequest, UISetToolsExpandedRequest__Output as _plugins_UISetToolsExpandedRequest__Output } from '../plugins/UISetToolsExpandedRequest.ts';
import type { UISetToolsExpandedResponse as _plugins_UISetToolsExpandedResponse, UISetToolsExpandedResponse__Output as _plugins_UISetToolsExpandedResponse__Output } from '../plugins/UISetToolsExpandedResponse.ts';
import type { UISetWidgetRequest as _plugins_UISetWidgetRequest, UISetWidgetRequest__Output as _plugins_UISetWidgetRequest__Output } from '../plugins/UISetWidgetRequest.ts';
import type { UISetWidgetResponse as _plugins_UISetWidgetResponse, UISetWidgetResponse__Output as _plugins_UISetWidgetResponse__Output } from '../plugins/UISetWidgetResponse.ts';

export interface HostClient extends grpc.Client {
  GetConfig(argument: _plugins_GetConfigRequest, metadata: grpc.Metadata, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_GetConfigResponse__Output>): grpc.ClientUnaryCall;
  GetConfig(argument: _plugins_GetConfigRequest, metadata: grpc.Metadata, callback: grpc.requestCallback<_plugins_GetConfigResponse__Output>): grpc.ClientUnaryCall;
  GetConfig(argument: _plugins_GetConfigRequest, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_GetConfigResponse__Output>): grpc.ClientUnaryCall;
  GetConfig(argument: _plugins_GetConfigRequest, callback: grpc.requestCallback<_plugins_GetConfigResponse__Output>): grpc.ClientUnaryCall;
  getConfig(argument: _plugins_GetConfigRequest, metadata: grpc.Metadata, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_GetConfigResponse__Output>): grpc.ClientUnaryCall;
  getConfig(argument: _plugins_GetConfigRequest, metadata: grpc.Metadata, callback: grpc.requestCallback<_plugins_GetConfigResponse__Output>): grpc.ClientUnaryCall;
  getConfig(argument: _plugins_GetConfigRequest, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_GetConfigResponse__Output>): grpc.ClientUnaryCall;
  getConfig(argument: _plugins_GetConfigRequest, callback: grpc.requestCallback<_plugins_GetConfigResponse__Output>): grpc.ClientUnaryCall;
  
  Log(argument: _plugins_LogRequest, metadata: grpc.Metadata, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_LogResponse__Output>): grpc.ClientUnaryCall;
  Log(argument: _plugins_LogRequest, metadata: grpc.Metadata, callback: grpc.requestCallback<_plugins_LogResponse__Output>): grpc.ClientUnaryCall;
  Log(argument: _plugins_LogRequest, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_LogResponse__Output>): grpc.ClientUnaryCall;
  Log(argument: _plugins_LogRequest, callback: grpc.requestCallback<_plugins_LogResponse__Output>): grpc.ClientUnaryCall;
  log(argument: _plugins_LogRequest, metadata: grpc.Metadata, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_LogResponse__Output>): grpc.ClientUnaryCall;
  log(argument: _plugins_LogRequest, metadata: grpc.Metadata, callback: grpc.requestCallback<_plugins_LogResponse__Output>): grpc.ClientUnaryCall;
  log(argument: _plugins_LogRequest, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_LogResponse__Output>): grpc.ClientUnaryCall;
  log(argument: _plugins_LogRequest, callback: grpc.requestCallback<_plugins_LogResponse__Output>): grpc.ClientUnaryCall;
  
  SessionAppend(argument: _plugins_SessionAppendRequest, metadata: grpc.Metadata, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_SessionAppendResponse__Output>): grpc.ClientUnaryCall;
  SessionAppend(argument: _plugins_SessionAppendRequest, metadata: grpc.Metadata, callback: grpc.requestCallback<_plugins_SessionAppendResponse__Output>): grpc.ClientUnaryCall;
  SessionAppend(argument: _plugins_SessionAppendRequest, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_SessionAppendResponse__Output>): grpc.ClientUnaryCall;
  SessionAppend(argument: _plugins_SessionAppendRequest, callback: grpc.requestCallback<_plugins_SessionAppendResponse__Output>): grpc.ClientUnaryCall;
  sessionAppend(argument: _plugins_SessionAppendRequest, metadata: grpc.Metadata, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_SessionAppendResponse__Output>): grpc.ClientUnaryCall;
  sessionAppend(argument: _plugins_SessionAppendRequest, metadata: grpc.Metadata, callback: grpc.requestCallback<_plugins_SessionAppendResponse__Output>): grpc.ClientUnaryCall;
  sessionAppend(argument: _plugins_SessionAppendRequest, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_SessionAppendResponse__Output>): grpc.ClientUnaryCall;
  sessionAppend(argument: _plugins_SessionAppendRequest, callback: grpc.requestCallback<_plugins_SessionAppendResponse__Output>): grpc.ClientUnaryCall;
  
  SessionCompact(argument: _plugins_SessionCompactRequest, metadata: grpc.Metadata, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_SessionCompactResponse__Output>): grpc.ClientUnaryCall;
  SessionCompact(argument: _plugins_SessionCompactRequest, metadata: grpc.Metadata, callback: grpc.requestCallback<_plugins_SessionCompactResponse__Output>): grpc.ClientUnaryCall;
  SessionCompact(argument: _plugins_SessionCompactRequest, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_SessionCompactResponse__Output>): grpc.ClientUnaryCall;
  SessionCompact(argument: _plugins_SessionCompactRequest, callback: grpc.requestCallback<_plugins_SessionCompactResponse__Output>): grpc.ClientUnaryCall;
  sessionCompact(argument: _plugins_SessionCompactRequest, metadata: grpc.Metadata, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_SessionCompactResponse__Output>): grpc.ClientUnaryCall;
  sessionCompact(argument: _plugins_SessionCompactRequest, metadata: grpc.Metadata, callback: grpc.requestCallback<_plugins_SessionCompactResponse__Output>): grpc.ClientUnaryCall;
  sessionCompact(argument: _plugins_SessionCompactRequest, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_SessionCompactResponse__Output>): grpc.ClientUnaryCall;
  sessionCompact(argument: _plugins_SessionCompactRequest, callback: grpc.requestCallback<_plugins_SessionCompactResponse__Output>): grpc.ClientUnaryCall;
  
  SessionGetLabel(argument: _plugins_SessionGetLabelRequest, metadata: grpc.Metadata, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_SessionGetLabelResponse__Output>): grpc.ClientUnaryCall;
  SessionGetLabel(argument: _plugins_SessionGetLabelRequest, metadata: grpc.Metadata, callback: grpc.requestCallback<_plugins_SessionGetLabelResponse__Output>): grpc.ClientUnaryCall;
  SessionGetLabel(argument: _plugins_SessionGetLabelRequest, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_SessionGetLabelResponse__Output>): grpc.ClientUnaryCall;
  SessionGetLabel(argument: _plugins_SessionGetLabelRequest, callback: grpc.requestCallback<_plugins_SessionGetLabelResponse__Output>): grpc.ClientUnaryCall;
  sessionGetLabel(argument: _plugins_SessionGetLabelRequest, metadata: grpc.Metadata, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_SessionGetLabelResponse__Output>): grpc.ClientUnaryCall;
  sessionGetLabel(argument: _plugins_SessionGetLabelRequest, metadata: grpc.Metadata, callback: grpc.requestCallback<_plugins_SessionGetLabelResponse__Output>): grpc.ClientUnaryCall;
  sessionGetLabel(argument: _plugins_SessionGetLabelRequest, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_SessionGetLabelResponse__Output>): grpc.ClientUnaryCall;
  sessionGetLabel(argument: _plugins_SessionGetLabelRequest, callback: grpc.requestCallback<_plugins_SessionGetLabelResponse__Output>): grpc.ClientUnaryCall;
  
  SessionScan(argument: _plugins_SessionScanRequest, metadata: grpc.Metadata, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_SessionScanResponse__Output>): grpc.ClientUnaryCall;
  SessionScan(argument: _plugins_SessionScanRequest, metadata: grpc.Metadata, callback: grpc.requestCallback<_plugins_SessionScanResponse__Output>): grpc.ClientUnaryCall;
  SessionScan(argument: _plugins_SessionScanRequest, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_SessionScanResponse__Output>): grpc.ClientUnaryCall;
  SessionScan(argument: _plugins_SessionScanRequest, callback: grpc.requestCallback<_plugins_SessionScanResponse__Output>): grpc.ClientUnaryCall;
  sessionScan(argument: _plugins_SessionScanRequest, metadata: grpc.Metadata, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_SessionScanResponse__Output>): grpc.ClientUnaryCall;
  sessionScan(argument: _plugins_SessionScanRequest, metadata: grpc.Metadata, callback: grpc.requestCallback<_plugins_SessionScanResponse__Output>): grpc.ClientUnaryCall;
  sessionScan(argument: _plugins_SessionScanRequest, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_SessionScanResponse__Output>): grpc.ClientUnaryCall;
  sessionScan(argument: _plugins_SessionScanRequest, callback: grpc.requestCallback<_plugins_SessionScanResponse__Output>): grpc.ClientUnaryCall;
  
  SessionSendCustomMessage(argument: _plugins_SessionSendCustomMessageRequest, metadata: grpc.Metadata, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_SessionSendUserMessageResponse__Output>): grpc.ClientUnaryCall;
  SessionSendCustomMessage(argument: _plugins_SessionSendCustomMessageRequest, metadata: grpc.Metadata, callback: grpc.requestCallback<_plugins_SessionSendUserMessageResponse__Output>): grpc.ClientUnaryCall;
  SessionSendCustomMessage(argument: _plugins_SessionSendCustomMessageRequest, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_SessionSendUserMessageResponse__Output>): grpc.ClientUnaryCall;
  SessionSendCustomMessage(argument: _plugins_SessionSendCustomMessageRequest, callback: grpc.requestCallback<_plugins_SessionSendUserMessageResponse__Output>): grpc.ClientUnaryCall;
  sessionSendCustomMessage(argument: _plugins_SessionSendCustomMessageRequest, metadata: grpc.Metadata, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_SessionSendUserMessageResponse__Output>): grpc.ClientUnaryCall;
  sessionSendCustomMessage(argument: _plugins_SessionSendCustomMessageRequest, metadata: grpc.Metadata, callback: grpc.requestCallback<_plugins_SessionSendUserMessageResponse__Output>): grpc.ClientUnaryCall;
  sessionSendCustomMessage(argument: _plugins_SessionSendCustomMessageRequest, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_SessionSendUserMessageResponse__Output>): grpc.ClientUnaryCall;
  sessionSendCustomMessage(argument: _plugins_SessionSendCustomMessageRequest, callback: grpc.requestCallback<_plugins_SessionSendUserMessageResponse__Output>): grpc.ClientUnaryCall;
  
  SessionSendUserMessage(argument: _plugins_SessionSendUserMessageRequest, metadata: grpc.Metadata, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_SessionSendUserMessageResponse__Output>): grpc.ClientUnaryCall;
  SessionSendUserMessage(argument: _plugins_SessionSendUserMessageRequest, metadata: grpc.Metadata, callback: grpc.requestCallback<_plugins_SessionSendUserMessageResponse__Output>): grpc.ClientUnaryCall;
  SessionSendUserMessage(argument: _plugins_SessionSendUserMessageRequest, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_SessionSendUserMessageResponse__Output>): grpc.ClientUnaryCall;
  SessionSendUserMessage(argument: _plugins_SessionSendUserMessageRequest, callback: grpc.requestCallback<_plugins_SessionSendUserMessageResponse__Output>): grpc.ClientUnaryCall;
  sessionSendUserMessage(argument: _plugins_SessionSendUserMessageRequest, metadata: grpc.Metadata, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_SessionSendUserMessageResponse__Output>): grpc.ClientUnaryCall;
  sessionSendUserMessage(argument: _plugins_SessionSendUserMessageRequest, metadata: grpc.Metadata, callback: grpc.requestCallback<_plugins_SessionSendUserMessageResponse__Output>): grpc.ClientUnaryCall;
  sessionSendUserMessage(argument: _plugins_SessionSendUserMessageRequest, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_SessionSendUserMessageResponse__Output>): grpc.ClientUnaryCall;
  sessionSendUserMessage(argument: _plugins_SessionSendUserMessageRequest, callback: grpc.requestCallback<_plugins_SessionSendUserMessageResponse__Output>): grpc.ClientUnaryCall;
  
  SessionSetLabel(argument: _plugins_SessionSetLabelRequest, metadata: grpc.Metadata, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_SessionSetLabelResponse__Output>): grpc.ClientUnaryCall;
  SessionSetLabel(argument: _plugins_SessionSetLabelRequest, metadata: grpc.Metadata, callback: grpc.requestCallback<_plugins_SessionSetLabelResponse__Output>): grpc.ClientUnaryCall;
  SessionSetLabel(argument: _plugins_SessionSetLabelRequest, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_SessionSetLabelResponse__Output>): grpc.ClientUnaryCall;
  SessionSetLabel(argument: _plugins_SessionSetLabelRequest, callback: grpc.requestCallback<_plugins_SessionSetLabelResponse__Output>): grpc.ClientUnaryCall;
  sessionSetLabel(argument: _plugins_SessionSetLabelRequest, metadata: grpc.Metadata, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_SessionSetLabelResponse__Output>): grpc.ClientUnaryCall;
  sessionSetLabel(argument: _plugins_SessionSetLabelRequest, metadata: grpc.Metadata, callback: grpc.requestCallback<_plugins_SessionSetLabelResponse__Output>): grpc.ClientUnaryCall;
  sessionSetLabel(argument: _plugins_SessionSetLabelRequest, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_SessionSetLabelResponse__Output>): grpc.ClientUnaryCall;
  sessionSetLabel(argument: _plugins_SessionSetLabelRequest, callback: grpc.requestCallback<_plugins_SessionSetLabelResponse__Output>): grpc.ClientUnaryCall;
  
  StateAppend(argument: _plugins_StateAppendRequest, metadata: grpc.Metadata, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_StateAppendResponse__Output>): grpc.ClientUnaryCall;
  StateAppend(argument: _plugins_StateAppendRequest, metadata: grpc.Metadata, callback: grpc.requestCallback<_plugins_StateAppendResponse__Output>): grpc.ClientUnaryCall;
  StateAppend(argument: _plugins_StateAppendRequest, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_StateAppendResponse__Output>): grpc.ClientUnaryCall;
  StateAppend(argument: _plugins_StateAppendRequest, callback: grpc.requestCallback<_plugins_StateAppendResponse__Output>): grpc.ClientUnaryCall;
  stateAppend(argument: _plugins_StateAppendRequest, metadata: grpc.Metadata, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_StateAppendResponse__Output>): grpc.ClientUnaryCall;
  stateAppend(argument: _plugins_StateAppendRequest, metadata: grpc.Metadata, callback: grpc.requestCallback<_plugins_StateAppendResponse__Output>): grpc.ClientUnaryCall;
  stateAppend(argument: _plugins_StateAppendRequest, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_StateAppendResponse__Output>): grpc.ClientUnaryCall;
  stateAppend(argument: _plugins_StateAppendRequest, callback: grpc.requestCallback<_plugins_StateAppendResponse__Output>): grpc.ClientUnaryCall;
  
  StateDelete(argument: _plugins_StateDeleteRequest, metadata: grpc.Metadata, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_StateDeleteResponse__Output>): grpc.ClientUnaryCall;
  StateDelete(argument: _plugins_StateDeleteRequest, metadata: grpc.Metadata, callback: grpc.requestCallback<_plugins_StateDeleteResponse__Output>): grpc.ClientUnaryCall;
  StateDelete(argument: _plugins_StateDeleteRequest, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_StateDeleteResponse__Output>): grpc.ClientUnaryCall;
  StateDelete(argument: _plugins_StateDeleteRequest, callback: grpc.requestCallback<_plugins_StateDeleteResponse__Output>): grpc.ClientUnaryCall;
  stateDelete(argument: _plugins_StateDeleteRequest, metadata: grpc.Metadata, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_StateDeleteResponse__Output>): grpc.ClientUnaryCall;
  stateDelete(argument: _plugins_StateDeleteRequest, metadata: grpc.Metadata, callback: grpc.requestCallback<_plugins_StateDeleteResponse__Output>): grpc.ClientUnaryCall;
  stateDelete(argument: _plugins_StateDeleteRequest, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_StateDeleteResponse__Output>): grpc.ClientUnaryCall;
  stateDelete(argument: _plugins_StateDeleteRequest, callback: grpc.requestCallback<_plugins_StateDeleteResponse__Output>): grpc.ClientUnaryCall;
  
  StateGet(argument: _plugins_StateGetRequest, metadata: grpc.Metadata, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_StateGetResponse__Output>): grpc.ClientUnaryCall;
  StateGet(argument: _plugins_StateGetRequest, metadata: grpc.Metadata, callback: grpc.requestCallback<_plugins_StateGetResponse__Output>): grpc.ClientUnaryCall;
  StateGet(argument: _plugins_StateGetRequest, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_StateGetResponse__Output>): grpc.ClientUnaryCall;
  StateGet(argument: _plugins_StateGetRequest, callback: grpc.requestCallback<_plugins_StateGetResponse__Output>): grpc.ClientUnaryCall;
  stateGet(argument: _plugins_StateGetRequest, metadata: grpc.Metadata, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_StateGetResponse__Output>): grpc.ClientUnaryCall;
  stateGet(argument: _plugins_StateGetRequest, metadata: grpc.Metadata, callback: grpc.requestCallback<_plugins_StateGetResponse__Output>): grpc.ClientUnaryCall;
  stateGet(argument: _plugins_StateGetRequest, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_StateGetResponse__Output>): grpc.ClientUnaryCall;
  stateGet(argument: _plugins_StateGetRequest, callback: grpc.requestCallback<_plugins_StateGetResponse__Output>): grpc.ClientUnaryCall;
  
  StateSet(argument: _plugins_StateSetRequest, metadata: grpc.Metadata, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_StateSetResponse__Output>): grpc.ClientUnaryCall;
  StateSet(argument: _plugins_StateSetRequest, metadata: grpc.Metadata, callback: grpc.requestCallback<_plugins_StateSetResponse__Output>): grpc.ClientUnaryCall;
  StateSet(argument: _plugins_StateSetRequest, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_StateSetResponse__Output>): grpc.ClientUnaryCall;
  StateSet(argument: _plugins_StateSetRequest, callback: grpc.requestCallback<_plugins_StateSetResponse__Output>): grpc.ClientUnaryCall;
  stateSet(argument: _plugins_StateSetRequest, metadata: grpc.Metadata, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_StateSetResponse__Output>): grpc.ClientUnaryCall;
  stateSet(argument: _plugins_StateSetRequest, metadata: grpc.Metadata, callback: grpc.requestCallback<_plugins_StateSetResponse__Output>): grpc.ClientUnaryCall;
  stateSet(argument: _plugins_StateSetRequest, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_StateSetResponse__Output>): grpc.ClientUnaryCall;
  stateSet(argument: _plugins_StateSetRequest, callback: grpc.requestCallback<_plugins_StateSetResponse__Output>): grpc.ClientUnaryCall;
  
  ToolDiscovery(argument: _plugins_ToolDiscoveryRequest, metadata: grpc.Metadata, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_ToolDiscoveryResponse__Output>): grpc.ClientUnaryCall;
  ToolDiscovery(argument: _plugins_ToolDiscoveryRequest, metadata: grpc.Metadata, callback: grpc.requestCallback<_plugins_ToolDiscoveryResponse__Output>): grpc.ClientUnaryCall;
  ToolDiscovery(argument: _plugins_ToolDiscoveryRequest, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_ToolDiscoveryResponse__Output>): grpc.ClientUnaryCall;
  ToolDiscovery(argument: _plugins_ToolDiscoveryRequest, callback: grpc.requestCallback<_plugins_ToolDiscoveryResponse__Output>): grpc.ClientUnaryCall;
  toolDiscovery(argument: _plugins_ToolDiscoveryRequest, metadata: grpc.Metadata, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_ToolDiscoveryResponse__Output>): grpc.ClientUnaryCall;
  toolDiscovery(argument: _plugins_ToolDiscoveryRequest, metadata: grpc.Metadata, callback: grpc.requestCallback<_plugins_ToolDiscoveryResponse__Output>): grpc.ClientUnaryCall;
  toolDiscovery(argument: _plugins_ToolDiscoveryRequest, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_ToolDiscoveryResponse__Output>): grpc.ClientUnaryCall;
  toolDiscovery(argument: _plugins_ToolDiscoveryRequest, callback: grpc.requestCallback<_plugins_ToolDiscoveryResponse__Output>): grpc.ClientUnaryCall;
  
  ToolProgress(argument: _plugins_ToolProgressEvent, metadata: grpc.Metadata, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_ToolProgressResponse__Output>): grpc.ClientUnaryCall;
  ToolProgress(argument: _plugins_ToolProgressEvent, metadata: grpc.Metadata, callback: grpc.requestCallback<_plugins_ToolProgressResponse__Output>): grpc.ClientUnaryCall;
  ToolProgress(argument: _plugins_ToolProgressEvent, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_ToolProgressResponse__Output>): grpc.ClientUnaryCall;
  ToolProgress(argument: _plugins_ToolProgressEvent, callback: grpc.requestCallback<_plugins_ToolProgressResponse__Output>): grpc.ClientUnaryCall;
  toolProgress(argument: _plugins_ToolProgressEvent, metadata: grpc.Metadata, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_ToolProgressResponse__Output>): grpc.ClientUnaryCall;
  toolProgress(argument: _plugins_ToolProgressEvent, metadata: grpc.Metadata, callback: grpc.requestCallback<_plugins_ToolProgressResponse__Output>): grpc.ClientUnaryCall;
  toolProgress(argument: _plugins_ToolProgressEvent, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_ToolProgressResponse__Output>): grpc.ClientUnaryCall;
  toolProgress(argument: _plugins_ToolProgressEvent, callback: grpc.requestCallback<_plugins_ToolProgressResponse__Output>): grpc.ClientUnaryCall;
  
  UIConfirm(argument: _plugins_UIConfirmRequest, metadata: grpc.Metadata, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_UIConfirmResponse__Output>): grpc.ClientUnaryCall;
  UIConfirm(argument: _plugins_UIConfirmRequest, metadata: grpc.Metadata, callback: grpc.requestCallback<_plugins_UIConfirmResponse__Output>): grpc.ClientUnaryCall;
  UIConfirm(argument: _plugins_UIConfirmRequest, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_UIConfirmResponse__Output>): grpc.ClientUnaryCall;
  UIConfirm(argument: _plugins_UIConfirmRequest, callback: grpc.requestCallback<_plugins_UIConfirmResponse__Output>): grpc.ClientUnaryCall;
  uiConfirm(argument: _plugins_UIConfirmRequest, metadata: grpc.Metadata, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_UIConfirmResponse__Output>): grpc.ClientUnaryCall;
  uiConfirm(argument: _plugins_UIConfirmRequest, metadata: grpc.Metadata, callback: grpc.requestCallback<_plugins_UIConfirmResponse__Output>): grpc.ClientUnaryCall;
  uiConfirm(argument: _plugins_UIConfirmRequest, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_UIConfirmResponse__Output>): grpc.ClientUnaryCall;
  uiConfirm(argument: _plugins_UIConfirmRequest, callback: grpc.requestCallback<_plugins_UIConfirmResponse__Output>): grpc.ClientUnaryCall;
  
  UIInput(argument: _plugins_UIInputRequest, metadata: grpc.Metadata, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_UIInputResponse__Output>): grpc.ClientUnaryCall;
  UIInput(argument: _plugins_UIInputRequest, metadata: grpc.Metadata, callback: grpc.requestCallback<_plugins_UIInputResponse__Output>): grpc.ClientUnaryCall;
  UIInput(argument: _plugins_UIInputRequest, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_UIInputResponse__Output>): grpc.ClientUnaryCall;
  UIInput(argument: _plugins_UIInputRequest, callback: grpc.requestCallback<_plugins_UIInputResponse__Output>): grpc.ClientUnaryCall;
  uiInput(argument: _plugins_UIInputRequest, metadata: grpc.Metadata, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_UIInputResponse__Output>): grpc.ClientUnaryCall;
  uiInput(argument: _plugins_UIInputRequest, metadata: grpc.Metadata, callback: grpc.requestCallback<_plugins_UIInputResponse__Output>): grpc.ClientUnaryCall;
  uiInput(argument: _plugins_UIInputRequest, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_UIInputResponse__Output>): grpc.ClientUnaryCall;
  uiInput(argument: _plugins_UIInputRequest, callback: grpc.requestCallback<_plugins_UIInputResponse__Output>): grpc.ClientUnaryCall;
  
  UINotify(argument: _plugins_UINotifyRequest, metadata: grpc.Metadata, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_UINotifyResponse__Output>): grpc.ClientUnaryCall;
  UINotify(argument: _plugins_UINotifyRequest, metadata: grpc.Metadata, callback: grpc.requestCallback<_plugins_UINotifyResponse__Output>): grpc.ClientUnaryCall;
  UINotify(argument: _plugins_UINotifyRequest, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_UINotifyResponse__Output>): grpc.ClientUnaryCall;
  UINotify(argument: _plugins_UINotifyRequest, callback: grpc.requestCallback<_plugins_UINotifyResponse__Output>): grpc.ClientUnaryCall;
  uiNotify(argument: _plugins_UINotifyRequest, metadata: grpc.Metadata, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_UINotifyResponse__Output>): grpc.ClientUnaryCall;
  uiNotify(argument: _plugins_UINotifyRequest, metadata: grpc.Metadata, callback: grpc.requestCallback<_plugins_UINotifyResponse__Output>): grpc.ClientUnaryCall;
  uiNotify(argument: _plugins_UINotifyRequest, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_UINotifyResponse__Output>): grpc.ClientUnaryCall;
  uiNotify(argument: _plugins_UINotifyRequest, callback: grpc.requestCallback<_plugins_UINotifyResponse__Output>): grpc.ClientUnaryCall;
  
  UISelect(argument: _plugins_UISelectRequest, metadata: grpc.Metadata, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_UISelectResponse__Output>): grpc.ClientUnaryCall;
  UISelect(argument: _plugins_UISelectRequest, metadata: grpc.Metadata, callback: grpc.requestCallback<_plugins_UISelectResponse__Output>): grpc.ClientUnaryCall;
  UISelect(argument: _plugins_UISelectRequest, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_UISelectResponse__Output>): grpc.ClientUnaryCall;
  UISelect(argument: _plugins_UISelectRequest, callback: grpc.requestCallback<_plugins_UISelectResponse__Output>): grpc.ClientUnaryCall;
  uiSelect(argument: _plugins_UISelectRequest, metadata: grpc.Metadata, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_UISelectResponse__Output>): grpc.ClientUnaryCall;
  uiSelect(argument: _plugins_UISelectRequest, metadata: grpc.Metadata, callback: grpc.requestCallback<_plugins_UISelectResponse__Output>): grpc.ClientUnaryCall;
  uiSelect(argument: _plugins_UISelectRequest, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_UISelectResponse__Output>): grpc.ClientUnaryCall;
  uiSelect(argument: _plugins_UISelectRequest, callback: grpc.requestCallback<_plugins_UISelectResponse__Output>): grpc.ClientUnaryCall;
  
  UISetEditorText(argument: _plugins_UISetEditorTextRequest, metadata: grpc.Metadata, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_UISetEditorTextResponse__Output>): grpc.ClientUnaryCall;
  UISetEditorText(argument: _plugins_UISetEditorTextRequest, metadata: grpc.Metadata, callback: grpc.requestCallback<_plugins_UISetEditorTextResponse__Output>): grpc.ClientUnaryCall;
  UISetEditorText(argument: _plugins_UISetEditorTextRequest, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_UISetEditorTextResponse__Output>): grpc.ClientUnaryCall;
  UISetEditorText(argument: _plugins_UISetEditorTextRequest, callback: grpc.requestCallback<_plugins_UISetEditorTextResponse__Output>): grpc.ClientUnaryCall;
  uiSetEditorText(argument: _plugins_UISetEditorTextRequest, metadata: grpc.Metadata, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_UISetEditorTextResponse__Output>): grpc.ClientUnaryCall;
  uiSetEditorText(argument: _plugins_UISetEditorTextRequest, metadata: grpc.Metadata, callback: grpc.requestCallback<_plugins_UISetEditorTextResponse__Output>): grpc.ClientUnaryCall;
  uiSetEditorText(argument: _plugins_UISetEditorTextRequest, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_UISetEditorTextResponse__Output>): grpc.ClientUnaryCall;
  uiSetEditorText(argument: _plugins_UISetEditorTextRequest, callback: grpc.requestCallback<_plugins_UISetEditorTextResponse__Output>): grpc.ClientUnaryCall;
  
  UISetStatus(argument: _plugins_UISetStatusRequest, metadata: grpc.Metadata, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_UISetStatusResponse__Output>): grpc.ClientUnaryCall;
  UISetStatus(argument: _plugins_UISetStatusRequest, metadata: grpc.Metadata, callback: grpc.requestCallback<_plugins_UISetStatusResponse__Output>): grpc.ClientUnaryCall;
  UISetStatus(argument: _plugins_UISetStatusRequest, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_UISetStatusResponse__Output>): grpc.ClientUnaryCall;
  UISetStatus(argument: _plugins_UISetStatusRequest, callback: grpc.requestCallback<_plugins_UISetStatusResponse__Output>): grpc.ClientUnaryCall;
  uiSetStatus(argument: _plugins_UISetStatusRequest, metadata: grpc.Metadata, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_UISetStatusResponse__Output>): grpc.ClientUnaryCall;
  uiSetStatus(argument: _plugins_UISetStatusRequest, metadata: grpc.Metadata, callback: grpc.requestCallback<_plugins_UISetStatusResponse__Output>): grpc.ClientUnaryCall;
  uiSetStatus(argument: _plugins_UISetStatusRequest, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_UISetStatusResponse__Output>): grpc.ClientUnaryCall;
  uiSetStatus(argument: _plugins_UISetStatusRequest, callback: grpc.requestCallback<_plugins_UISetStatusResponse__Output>): grpc.ClientUnaryCall;
  
  UISetTitle(argument: _plugins_UISetTitleRequest, metadata: grpc.Metadata, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_UISetTitleResponse__Output>): grpc.ClientUnaryCall;
  UISetTitle(argument: _plugins_UISetTitleRequest, metadata: grpc.Metadata, callback: grpc.requestCallback<_plugins_UISetTitleResponse__Output>): grpc.ClientUnaryCall;
  UISetTitle(argument: _plugins_UISetTitleRequest, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_UISetTitleResponse__Output>): grpc.ClientUnaryCall;
  UISetTitle(argument: _plugins_UISetTitleRequest, callback: grpc.requestCallback<_plugins_UISetTitleResponse__Output>): grpc.ClientUnaryCall;
  uiSetTitle(argument: _plugins_UISetTitleRequest, metadata: grpc.Metadata, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_UISetTitleResponse__Output>): grpc.ClientUnaryCall;
  uiSetTitle(argument: _plugins_UISetTitleRequest, metadata: grpc.Metadata, callback: grpc.requestCallback<_plugins_UISetTitleResponse__Output>): grpc.ClientUnaryCall;
  uiSetTitle(argument: _plugins_UISetTitleRequest, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_UISetTitleResponse__Output>): grpc.ClientUnaryCall;
  uiSetTitle(argument: _plugins_UISetTitleRequest, callback: grpc.requestCallback<_plugins_UISetTitleResponse__Output>): grpc.ClientUnaryCall;
  
  UISetToolsExpanded(argument: _plugins_UISetToolsExpandedRequest, metadata: grpc.Metadata, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_UISetToolsExpandedResponse__Output>): grpc.ClientUnaryCall;
  UISetToolsExpanded(argument: _plugins_UISetToolsExpandedRequest, metadata: grpc.Metadata, callback: grpc.requestCallback<_plugins_UISetToolsExpandedResponse__Output>): grpc.ClientUnaryCall;
  UISetToolsExpanded(argument: _plugins_UISetToolsExpandedRequest, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_UISetToolsExpandedResponse__Output>): grpc.ClientUnaryCall;
  UISetToolsExpanded(argument: _plugins_UISetToolsExpandedRequest, callback: grpc.requestCallback<_plugins_UISetToolsExpandedResponse__Output>): grpc.ClientUnaryCall;
  uiSetToolsExpanded(argument: _plugins_UISetToolsExpandedRequest, metadata: grpc.Metadata, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_UISetToolsExpandedResponse__Output>): grpc.ClientUnaryCall;
  uiSetToolsExpanded(argument: _plugins_UISetToolsExpandedRequest, metadata: grpc.Metadata, callback: grpc.requestCallback<_plugins_UISetToolsExpandedResponse__Output>): grpc.ClientUnaryCall;
  uiSetToolsExpanded(argument: _plugins_UISetToolsExpandedRequest, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_UISetToolsExpandedResponse__Output>): grpc.ClientUnaryCall;
  uiSetToolsExpanded(argument: _plugins_UISetToolsExpandedRequest, callback: grpc.requestCallback<_plugins_UISetToolsExpandedResponse__Output>): grpc.ClientUnaryCall;
  
  UISetWidget(argument: _plugins_UISetWidgetRequest, metadata: grpc.Metadata, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_UISetWidgetResponse__Output>): grpc.ClientUnaryCall;
  UISetWidget(argument: _plugins_UISetWidgetRequest, metadata: grpc.Metadata, callback: grpc.requestCallback<_plugins_UISetWidgetResponse__Output>): grpc.ClientUnaryCall;
  UISetWidget(argument: _plugins_UISetWidgetRequest, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_UISetWidgetResponse__Output>): grpc.ClientUnaryCall;
  UISetWidget(argument: _plugins_UISetWidgetRequest, callback: grpc.requestCallback<_plugins_UISetWidgetResponse__Output>): grpc.ClientUnaryCall;
  uiSetWidget(argument: _plugins_UISetWidgetRequest, metadata: grpc.Metadata, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_UISetWidgetResponse__Output>): grpc.ClientUnaryCall;
  uiSetWidget(argument: _plugins_UISetWidgetRequest, metadata: grpc.Metadata, callback: grpc.requestCallback<_plugins_UISetWidgetResponse__Output>): grpc.ClientUnaryCall;
  uiSetWidget(argument: _plugins_UISetWidgetRequest, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_UISetWidgetResponse__Output>): grpc.ClientUnaryCall;
  uiSetWidget(argument: _plugins_UISetWidgetRequest, callback: grpc.requestCallback<_plugins_UISetWidgetResponse__Output>): grpc.ClientUnaryCall;
  
}

export interface HostHandlers extends grpc.UntypedServiceImplementation {
  GetConfig: grpc.handleUnaryCall<_plugins_GetConfigRequest__Output, _plugins_GetConfigResponse>;
  
  Log: grpc.handleUnaryCall<_plugins_LogRequest__Output, _plugins_LogResponse>;
  
  SessionAppend: grpc.handleUnaryCall<_plugins_SessionAppendRequest__Output, _plugins_SessionAppendResponse>;
  
  SessionCompact: grpc.handleUnaryCall<_plugins_SessionCompactRequest__Output, _plugins_SessionCompactResponse>;
  
  SessionGetLabel: grpc.handleUnaryCall<_plugins_SessionGetLabelRequest__Output, _plugins_SessionGetLabelResponse>;
  
  SessionScan: grpc.handleUnaryCall<_plugins_SessionScanRequest__Output, _plugins_SessionScanResponse>;
  
  SessionSendCustomMessage: grpc.handleUnaryCall<_plugins_SessionSendCustomMessageRequest__Output, _plugins_SessionSendUserMessageResponse>;
  
  SessionSendUserMessage: grpc.handleUnaryCall<_plugins_SessionSendUserMessageRequest__Output, _plugins_SessionSendUserMessageResponse>;
  
  SessionSetLabel: grpc.handleUnaryCall<_plugins_SessionSetLabelRequest__Output, _plugins_SessionSetLabelResponse>;
  
  StateAppend: grpc.handleUnaryCall<_plugins_StateAppendRequest__Output, _plugins_StateAppendResponse>;
  
  StateDelete: grpc.handleUnaryCall<_plugins_StateDeleteRequest__Output, _plugins_StateDeleteResponse>;
  
  StateGet: grpc.handleUnaryCall<_plugins_StateGetRequest__Output, _plugins_StateGetResponse>;
  
  StateSet: grpc.handleUnaryCall<_plugins_StateSetRequest__Output, _plugins_StateSetResponse>;
  
  ToolDiscovery: grpc.handleUnaryCall<_plugins_ToolDiscoveryRequest__Output, _plugins_ToolDiscoveryResponse>;
  
  ToolProgress: grpc.handleUnaryCall<_plugins_ToolProgressEvent__Output, _plugins_ToolProgressResponse>;
  
  UIConfirm: grpc.handleUnaryCall<_plugins_UIConfirmRequest__Output, _plugins_UIConfirmResponse>;
  
  UIInput: grpc.handleUnaryCall<_plugins_UIInputRequest__Output, _plugins_UIInputResponse>;
  
  UINotify: grpc.handleUnaryCall<_plugins_UINotifyRequest__Output, _plugins_UINotifyResponse>;
  
  UISelect: grpc.handleUnaryCall<_plugins_UISelectRequest__Output, _plugins_UISelectResponse>;
  
  UISetEditorText: grpc.handleUnaryCall<_plugins_UISetEditorTextRequest__Output, _plugins_UISetEditorTextResponse>;
  
  UISetStatus: grpc.handleUnaryCall<_plugins_UISetStatusRequest__Output, _plugins_UISetStatusResponse>;
  
  UISetTitle: grpc.handleUnaryCall<_plugins_UISetTitleRequest__Output, _plugins_UISetTitleResponse>;
  
  UISetToolsExpanded: grpc.handleUnaryCall<_plugins_UISetToolsExpandedRequest__Output, _plugins_UISetToolsExpandedResponse>;
  
  UISetWidget: grpc.handleUnaryCall<_plugins_UISetWidgetRequest__Output, _plugins_UISetWidgetResponse>;
  
}

export interface HostDefinition extends grpc.ServiceDefinition {
  GetConfig: MethodDefinition<_plugins_GetConfigRequest, _plugins_GetConfigResponse, _plugins_GetConfigRequest__Output, _plugins_GetConfigResponse__Output>
  Log: MethodDefinition<_plugins_LogRequest, _plugins_LogResponse, _plugins_LogRequest__Output, _plugins_LogResponse__Output>
  SessionAppend: MethodDefinition<_plugins_SessionAppendRequest, _plugins_SessionAppendResponse, _plugins_SessionAppendRequest__Output, _plugins_SessionAppendResponse__Output>
  SessionCompact: MethodDefinition<_plugins_SessionCompactRequest, _plugins_SessionCompactResponse, _plugins_SessionCompactRequest__Output, _plugins_SessionCompactResponse__Output>
  SessionGetLabel: MethodDefinition<_plugins_SessionGetLabelRequest, _plugins_SessionGetLabelResponse, _plugins_SessionGetLabelRequest__Output, _plugins_SessionGetLabelResponse__Output>
  SessionScan: MethodDefinition<_plugins_SessionScanRequest, _plugins_SessionScanResponse, _plugins_SessionScanRequest__Output, _plugins_SessionScanResponse__Output>
  SessionSendCustomMessage: MethodDefinition<_plugins_SessionSendCustomMessageRequest, _plugins_SessionSendUserMessageResponse, _plugins_SessionSendCustomMessageRequest__Output, _plugins_SessionSendUserMessageResponse__Output>
  SessionSendUserMessage: MethodDefinition<_plugins_SessionSendUserMessageRequest, _plugins_SessionSendUserMessageResponse, _plugins_SessionSendUserMessageRequest__Output, _plugins_SessionSendUserMessageResponse__Output>
  SessionSetLabel: MethodDefinition<_plugins_SessionSetLabelRequest, _plugins_SessionSetLabelResponse, _plugins_SessionSetLabelRequest__Output, _plugins_SessionSetLabelResponse__Output>
  StateAppend: MethodDefinition<_plugins_StateAppendRequest, _plugins_StateAppendResponse, _plugins_StateAppendRequest__Output, _plugins_StateAppendResponse__Output>
  StateDelete: MethodDefinition<_plugins_StateDeleteRequest, _plugins_StateDeleteResponse, _plugins_StateDeleteRequest__Output, _plugins_StateDeleteResponse__Output>
  StateGet: MethodDefinition<_plugins_StateGetRequest, _plugins_StateGetResponse, _plugins_StateGetRequest__Output, _plugins_StateGetResponse__Output>
  StateSet: MethodDefinition<_plugins_StateSetRequest, _plugins_StateSetResponse, _plugins_StateSetRequest__Output, _plugins_StateSetResponse__Output>
  ToolDiscovery: MethodDefinition<_plugins_ToolDiscoveryRequest, _plugins_ToolDiscoveryResponse, _plugins_ToolDiscoveryRequest__Output, _plugins_ToolDiscoveryResponse__Output>
  ToolProgress: MethodDefinition<_plugins_ToolProgressEvent, _plugins_ToolProgressResponse, _plugins_ToolProgressEvent__Output, _plugins_ToolProgressResponse__Output>
  UIConfirm: MethodDefinition<_plugins_UIConfirmRequest, _plugins_UIConfirmResponse, _plugins_UIConfirmRequest__Output, _plugins_UIConfirmResponse__Output>
  UIInput: MethodDefinition<_plugins_UIInputRequest, _plugins_UIInputResponse, _plugins_UIInputRequest__Output, _plugins_UIInputResponse__Output>
  UINotify: MethodDefinition<_plugins_UINotifyRequest, _plugins_UINotifyResponse, _plugins_UINotifyRequest__Output, _plugins_UINotifyResponse__Output>
  UISelect: MethodDefinition<_plugins_UISelectRequest, _plugins_UISelectResponse, _plugins_UISelectRequest__Output, _plugins_UISelectResponse__Output>
  UISetEditorText: MethodDefinition<_plugins_UISetEditorTextRequest, _plugins_UISetEditorTextResponse, _plugins_UISetEditorTextRequest__Output, _plugins_UISetEditorTextResponse__Output>
  UISetStatus: MethodDefinition<_plugins_UISetStatusRequest, _plugins_UISetStatusResponse, _plugins_UISetStatusRequest__Output, _plugins_UISetStatusResponse__Output>
  UISetTitle: MethodDefinition<_plugins_UISetTitleRequest, _plugins_UISetTitleResponse, _plugins_UISetTitleRequest__Output, _plugins_UISetTitleResponse__Output>
  UISetToolsExpanded: MethodDefinition<_plugins_UISetToolsExpandedRequest, _plugins_UISetToolsExpandedResponse, _plugins_UISetToolsExpandedRequest__Output, _plugins_UISetToolsExpandedResponse__Output>
  UISetWidget: MethodDefinition<_plugins_UISetWidgetRequest, _plugins_UISetWidgetResponse, _plugins_UISetWidgetRequest__Output, _plugins_UISetWidgetResponse__Output>
}
