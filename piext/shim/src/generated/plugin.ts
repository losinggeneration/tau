import type * as grpc from '@grpc/grpc-js';
import type { MessageTypeDefinition } from '@grpc/proto-loader';

import type { HostClient as _plugins_HostClient, HostDefinition as _plugins_HostDefinition } from './plugins/Host.ts';
import type { PluginClient as _plugins_PluginClient, PluginDefinition as _plugins_PluginDefinition } from './plugins/Plugin.ts';

type SubtypeConstructor<Constructor extends new (...args: any) => any, Subtype> = {
  new(...args: ConstructorParameters<Constructor>): Subtype;
};

export interface ProtoGrpcType {
  plugins: {
    CLIFlag: MessageTypeDefinition
    CommandInvocation: MessageTypeDefinition
    CommandRegistration: MessageTypeDefinition
    CommandResult: MessageTypeDefinition
    CompactionRequest: MessageTypeDefinition
    CompactionResponse: MessageTypeDefinition
    Event: MessageTypeDefinition
    EventResponse: MessageTypeDefinition
    GetConfigRequest: MessageTypeDefinition
    GetConfigResponse: MessageTypeDefinition
    Host: SubtypeConstructor<typeof grpc.Client, _plugins_HostClient> & { service: _plugins_HostDefinition }
    InitRequest: MessageTypeDefinition
    InitResponse: MessageTypeDefinition
    ListRegistrationsRequest: MessageTypeDefinition
    LogRequest: MessageTypeDefinition
    LogResponse: MessageTypeDefinition
    ModelRouteRequest: MessageTypeDefinition
    ModelRouteResponse: MessageTypeDefinition
    Plugin: SubtypeConstructor<typeof grpc.Client, _plugins_PluginClient> & { service: _plugins_PluginDefinition }
    ReachToolsResponse: MessageTypeDefinition
    Registrations: MessageTypeDefinition
    SessionAppendRequest: MessageTypeDefinition
    SessionAppendResponse: MessageTypeDefinition
    SessionCompactRequest: MessageTypeDefinition
    SessionCompactResponse: MessageTypeDefinition
    SessionEntry: MessageTypeDefinition
    SessionGetLabelRequest: MessageTypeDefinition
    SessionGetLabelResponse: MessageTypeDefinition
    SessionScanRequest: MessageTypeDefinition
    SessionScanResponse: MessageTypeDefinition
    SessionSendCustomMessageRequest: MessageTypeDefinition
    SessionSendUserMessageRequest: MessageTypeDefinition
    SessionSendUserMessageResponse: MessageTypeDefinition
    SessionSetLabelRequest: MessageTypeDefinition
    SessionSetLabelResponse: MessageTypeDefinition
    ShutdownRequest: MessageTypeDefinition
    ShutdownResponse: MessageTypeDefinition
    StateAppendRequest: MessageTypeDefinition
    StateAppendResponse: MessageTypeDefinition
    StateDeleteRequest: MessageTypeDefinition
    StateDeleteResponse: MessageTypeDefinition
    StateGetRequest: MessageTypeDefinition
    StateGetResponse: MessageTypeDefinition
    StateSetRequest: MessageTypeDefinition
    StateSetResponse: MessageTypeDefinition
    TerminalInput: MessageTypeDefinition
    TerminalInputResponse: MessageTypeDefinition
    ToolCall: MessageTypeDefinition
    ToolDiscoveryRequest: MessageTypeDefinition
    ToolDiscoveryResponse: MessageTypeDefinition
    ToolProgressEvent: MessageTypeDefinition
    ToolProgressResponse: MessageTypeDefinition
    ToolRegistration: MessageTypeDefinition
    ToolResult: MessageTypeDefinition
    UIConfirmRequest: MessageTypeDefinition
    UIConfirmResponse: MessageTypeDefinition
    UIInputRequest: MessageTypeDefinition
    UIInputResponse: MessageTypeDefinition
    UINotifyRequest: MessageTypeDefinition
    UINotifyResponse: MessageTypeDefinition
    UISelectRequest: MessageTypeDefinition
    UISelectResponse: MessageTypeDefinition
    UISetEditorTextRequest: MessageTypeDefinition
    UISetEditorTextResponse: MessageTypeDefinition
    UISetStatusRequest: MessageTypeDefinition
    UISetStatusResponse: MessageTypeDefinition
    UISetTitleRequest: MessageTypeDefinition
    UISetTitleResponse: MessageTypeDefinition
    UISetToolsExpandedRequest: MessageTypeDefinition
    UISetToolsExpandedResponse: MessageTypeDefinition
    UISetWidgetRequest: MessageTypeDefinition
    UISetWidgetResponse: MessageTypeDefinition
    VirtualModelRegistration: MessageTypeDefinition
  }
}

