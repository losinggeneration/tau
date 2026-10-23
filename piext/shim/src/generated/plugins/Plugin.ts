// Original file: proto/plugin.proto

import type * as grpc from '@grpc/grpc-js'
import type { MethodDefinition } from '@grpc/proto-loader'
import type { CommandInvocation as _plugins_CommandInvocation, CommandInvocation__Output as _plugins_CommandInvocation__Output } from '../plugins/CommandInvocation.ts';
import type { CommandResult as _plugins_CommandResult, CommandResult__Output as _plugins_CommandResult__Output } from '../plugins/CommandResult.ts';
import type { Event as _plugins_Event, Event__Output as _plugins_Event__Output } from '../plugins/Event.ts';
import type { EventResponse as _plugins_EventResponse, EventResponse__Output as _plugins_EventResponse__Output } from '../plugins/EventResponse.ts';
import type { InitRequest as _plugins_InitRequest, InitRequest__Output as _plugins_InitRequest__Output } from '../plugins/InitRequest.ts';
import type { InitResponse as _plugins_InitResponse, InitResponse__Output as _plugins_InitResponse__Output } from '../plugins/InitResponse.ts';
import type { ListRegistrationsRequest as _plugins_ListRegistrationsRequest, ListRegistrationsRequest__Output as _plugins_ListRegistrationsRequest__Output } from '../plugins/ListRegistrationsRequest.ts';
import type { ModelRouteRequest as _plugins_ModelRouteRequest, ModelRouteRequest__Output as _plugins_ModelRouteRequest__Output } from '../plugins/ModelRouteRequest.ts';
import type { ModelRouteResponse as _plugins_ModelRouteResponse, ModelRouteResponse__Output as _plugins_ModelRouteResponse__Output } from '../plugins/ModelRouteResponse.ts';
import type { ReachToolsResponse as _plugins_ReachToolsResponse, ReachToolsResponse__Output as _plugins_ReachToolsResponse__Output } from '../plugins/ReachToolsResponse.ts';
import type { Registrations as _plugins_Registrations, Registrations__Output as _plugins_Registrations__Output } from '../plugins/Registrations.ts';
import type { ShutdownRequest as _plugins_ShutdownRequest, ShutdownRequest__Output as _plugins_ShutdownRequest__Output } from '../plugins/ShutdownRequest.ts';
import type { ShutdownResponse as _plugins_ShutdownResponse, ShutdownResponse__Output as _plugins_ShutdownResponse__Output } from '../plugins/ShutdownResponse.ts';
import type { TerminalInput as _plugins_TerminalInput, TerminalInput__Output as _plugins_TerminalInput__Output } from '../plugins/TerminalInput.ts';
import type { TerminalInputResponse as _plugins_TerminalInputResponse, TerminalInputResponse__Output as _plugins_TerminalInputResponse__Output } from '../plugins/TerminalInputResponse.ts';
import type { ToolCall as _plugins_ToolCall, ToolCall__Output as _plugins_ToolCall__Output } from '../plugins/ToolCall.ts';
import type { ToolResult as _plugins_ToolResult, ToolResult__Output as _plugins_ToolResult__Output } from '../plugins/ToolResult.ts';

export interface PluginClient extends grpc.Client {
  ExecuteCommand(argument: _plugins_CommandInvocation, metadata: grpc.Metadata, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_CommandResult__Output>): grpc.ClientUnaryCall;
  ExecuteCommand(argument: _plugins_CommandInvocation, metadata: grpc.Metadata, callback: grpc.requestCallback<_plugins_CommandResult__Output>): grpc.ClientUnaryCall;
  ExecuteCommand(argument: _plugins_CommandInvocation, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_CommandResult__Output>): grpc.ClientUnaryCall;
  ExecuteCommand(argument: _plugins_CommandInvocation, callback: grpc.requestCallback<_plugins_CommandResult__Output>): grpc.ClientUnaryCall;
  executeCommand(argument: _plugins_CommandInvocation, metadata: grpc.Metadata, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_CommandResult__Output>): grpc.ClientUnaryCall;
  executeCommand(argument: _plugins_CommandInvocation, metadata: grpc.Metadata, callback: grpc.requestCallback<_plugins_CommandResult__Output>): grpc.ClientUnaryCall;
  executeCommand(argument: _plugins_CommandInvocation, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_CommandResult__Output>): grpc.ClientUnaryCall;
  executeCommand(argument: _plugins_CommandInvocation, callback: grpc.requestCallback<_plugins_CommandResult__Output>): grpc.ClientUnaryCall;
  
  ExecuteTool(argument: _plugins_ToolCall, metadata: grpc.Metadata, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_ToolResult__Output>): grpc.ClientUnaryCall;
  ExecuteTool(argument: _plugins_ToolCall, metadata: grpc.Metadata, callback: grpc.requestCallback<_plugins_ToolResult__Output>): grpc.ClientUnaryCall;
  ExecuteTool(argument: _plugins_ToolCall, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_ToolResult__Output>): grpc.ClientUnaryCall;
  ExecuteTool(argument: _plugins_ToolCall, callback: grpc.requestCallback<_plugins_ToolResult__Output>): grpc.ClientUnaryCall;
  executeTool(argument: _plugins_ToolCall, metadata: grpc.Metadata, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_ToolResult__Output>): grpc.ClientUnaryCall;
  executeTool(argument: _plugins_ToolCall, metadata: grpc.Metadata, callback: grpc.requestCallback<_plugins_ToolResult__Output>): grpc.ClientUnaryCall;
  executeTool(argument: _plugins_ToolCall, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_ToolResult__Output>): grpc.ClientUnaryCall;
  executeTool(argument: _plugins_ToolCall, callback: grpc.requestCallback<_plugins_ToolResult__Output>): grpc.ClientUnaryCall;
  
  Initialize(argument: _plugins_InitRequest, metadata: grpc.Metadata, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_InitResponse__Output>): grpc.ClientUnaryCall;
  Initialize(argument: _plugins_InitRequest, metadata: grpc.Metadata, callback: grpc.requestCallback<_plugins_InitResponse__Output>): grpc.ClientUnaryCall;
  Initialize(argument: _plugins_InitRequest, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_InitResponse__Output>): grpc.ClientUnaryCall;
  Initialize(argument: _plugins_InitRequest, callback: grpc.requestCallback<_plugins_InitResponse__Output>): grpc.ClientUnaryCall;
  initialize(argument: _plugins_InitRequest, metadata: grpc.Metadata, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_InitResponse__Output>): grpc.ClientUnaryCall;
  initialize(argument: _plugins_InitRequest, metadata: grpc.Metadata, callback: grpc.requestCallback<_plugins_InitResponse__Output>): grpc.ClientUnaryCall;
  initialize(argument: _plugins_InitRequest, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_InitResponse__Output>): grpc.ClientUnaryCall;
  initialize(argument: _plugins_InitRequest, callback: grpc.requestCallback<_plugins_InitResponse__Output>): grpc.ClientUnaryCall;
  
  ListRegistrations(argument: _plugins_ListRegistrationsRequest, metadata: grpc.Metadata, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_Registrations__Output>): grpc.ClientUnaryCall;
  ListRegistrations(argument: _plugins_ListRegistrationsRequest, metadata: grpc.Metadata, callback: grpc.requestCallback<_plugins_Registrations__Output>): grpc.ClientUnaryCall;
  ListRegistrations(argument: _plugins_ListRegistrationsRequest, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_Registrations__Output>): grpc.ClientUnaryCall;
  ListRegistrations(argument: _plugins_ListRegistrationsRequest, callback: grpc.requestCallback<_plugins_Registrations__Output>): grpc.ClientUnaryCall;
  listRegistrations(argument: _plugins_ListRegistrationsRequest, metadata: grpc.Metadata, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_Registrations__Output>): grpc.ClientUnaryCall;
  listRegistrations(argument: _plugins_ListRegistrationsRequest, metadata: grpc.Metadata, callback: grpc.requestCallback<_plugins_Registrations__Output>): grpc.ClientUnaryCall;
  listRegistrations(argument: _plugins_ListRegistrationsRequest, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_Registrations__Output>): grpc.ClientUnaryCall;
  listRegistrations(argument: _plugins_ListRegistrationsRequest, callback: grpc.requestCallback<_plugins_Registrations__Output>): grpc.ClientUnaryCall;
  
  OnEvent(argument: _plugins_Event, metadata: grpc.Metadata, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_EventResponse__Output>): grpc.ClientUnaryCall;
  OnEvent(argument: _plugins_Event, metadata: grpc.Metadata, callback: grpc.requestCallback<_plugins_EventResponse__Output>): grpc.ClientUnaryCall;
  OnEvent(argument: _plugins_Event, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_EventResponse__Output>): grpc.ClientUnaryCall;
  OnEvent(argument: _plugins_Event, callback: grpc.requestCallback<_plugins_EventResponse__Output>): grpc.ClientUnaryCall;
  onEvent(argument: _plugins_Event, metadata: grpc.Metadata, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_EventResponse__Output>): grpc.ClientUnaryCall;
  onEvent(argument: _plugins_Event, metadata: grpc.Metadata, callback: grpc.requestCallback<_plugins_EventResponse__Output>): grpc.ClientUnaryCall;
  onEvent(argument: _plugins_Event, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_EventResponse__Output>): grpc.ClientUnaryCall;
  onEvent(argument: _plugins_Event, callback: grpc.requestCallback<_plugins_EventResponse__Output>): grpc.ClientUnaryCall;
  
  OnTerminalInput(argument: _plugins_TerminalInput, metadata: grpc.Metadata, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_TerminalInputResponse__Output>): grpc.ClientUnaryCall;
  OnTerminalInput(argument: _plugins_TerminalInput, metadata: grpc.Metadata, callback: grpc.requestCallback<_plugins_TerminalInputResponse__Output>): grpc.ClientUnaryCall;
  OnTerminalInput(argument: _plugins_TerminalInput, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_TerminalInputResponse__Output>): grpc.ClientUnaryCall;
  OnTerminalInput(argument: _plugins_TerminalInput, callback: grpc.requestCallback<_plugins_TerminalInputResponse__Output>): grpc.ClientUnaryCall;
  onTerminalInput(argument: _plugins_TerminalInput, metadata: grpc.Metadata, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_TerminalInputResponse__Output>): grpc.ClientUnaryCall;
  onTerminalInput(argument: _plugins_TerminalInput, metadata: grpc.Metadata, callback: grpc.requestCallback<_plugins_TerminalInputResponse__Output>): grpc.ClientUnaryCall;
  onTerminalInput(argument: _plugins_TerminalInput, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_TerminalInputResponse__Output>): grpc.ClientUnaryCall;
  onTerminalInput(argument: _plugins_TerminalInput, callback: grpc.requestCallback<_plugins_TerminalInputResponse__Output>): grpc.ClientUnaryCall;
  
  ReachTools(argument: _plugins_ToolCall, metadata: grpc.Metadata, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_ReachToolsResponse__Output>): grpc.ClientUnaryCall;
  ReachTools(argument: _plugins_ToolCall, metadata: grpc.Metadata, callback: grpc.requestCallback<_plugins_ReachToolsResponse__Output>): grpc.ClientUnaryCall;
  ReachTools(argument: _plugins_ToolCall, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_ReachToolsResponse__Output>): grpc.ClientUnaryCall;
  ReachTools(argument: _plugins_ToolCall, callback: grpc.requestCallback<_plugins_ReachToolsResponse__Output>): grpc.ClientUnaryCall;
  reachTools(argument: _plugins_ToolCall, metadata: grpc.Metadata, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_ReachToolsResponse__Output>): grpc.ClientUnaryCall;
  reachTools(argument: _plugins_ToolCall, metadata: grpc.Metadata, callback: grpc.requestCallback<_plugins_ReachToolsResponse__Output>): grpc.ClientUnaryCall;
  reachTools(argument: _plugins_ToolCall, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_ReachToolsResponse__Output>): grpc.ClientUnaryCall;
  reachTools(argument: _plugins_ToolCall, callback: grpc.requestCallback<_plugins_ReachToolsResponse__Output>): grpc.ClientUnaryCall;
  
  RouteModel(argument: _plugins_ModelRouteRequest, metadata: grpc.Metadata, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_ModelRouteResponse__Output>): grpc.ClientUnaryCall;
  RouteModel(argument: _plugins_ModelRouteRequest, metadata: grpc.Metadata, callback: grpc.requestCallback<_plugins_ModelRouteResponse__Output>): grpc.ClientUnaryCall;
  RouteModel(argument: _plugins_ModelRouteRequest, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_ModelRouteResponse__Output>): grpc.ClientUnaryCall;
  RouteModel(argument: _plugins_ModelRouteRequest, callback: grpc.requestCallback<_plugins_ModelRouteResponse__Output>): grpc.ClientUnaryCall;
  routeModel(argument: _plugins_ModelRouteRequest, metadata: grpc.Metadata, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_ModelRouteResponse__Output>): grpc.ClientUnaryCall;
  routeModel(argument: _plugins_ModelRouteRequest, metadata: grpc.Metadata, callback: grpc.requestCallback<_plugins_ModelRouteResponse__Output>): grpc.ClientUnaryCall;
  routeModel(argument: _plugins_ModelRouteRequest, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_ModelRouteResponse__Output>): grpc.ClientUnaryCall;
  routeModel(argument: _plugins_ModelRouteRequest, callback: grpc.requestCallback<_plugins_ModelRouteResponse__Output>): grpc.ClientUnaryCall;
  
  Shutdown(argument: _plugins_ShutdownRequest, metadata: grpc.Metadata, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_ShutdownResponse__Output>): grpc.ClientUnaryCall;
  Shutdown(argument: _plugins_ShutdownRequest, metadata: grpc.Metadata, callback: grpc.requestCallback<_plugins_ShutdownResponse__Output>): grpc.ClientUnaryCall;
  Shutdown(argument: _plugins_ShutdownRequest, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_ShutdownResponse__Output>): grpc.ClientUnaryCall;
  Shutdown(argument: _plugins_ShutdownRequest, callback: grpc.requestCallback<_plugins_ShutdownResponse__Output>): grpc.ClientUnaryCall;
  shutdown(argument: _plugins_ShutdownRequest, metadata: grpc.Metadata, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_ShutdownResponse__Output>): grpc.ClientUnaryCall;
  shutdown(argument: _plugins_ShutdownRequest, metadata: grpc.Metadata, callback: grpc.requestCallback<_plugins_ShutdownResponse__Output>): grpc.ClientUnaryCall;
  shutdown(argument: _plugins_ShutdownRequest, options: grpc.CallOptions, callback: grpc.requestCallback<_plugins_ShutdownResponse__Output>): grpc.ClientUnaryCall;
  shutdown(argument: _plugins_ShutdownRequest, callback: grpc.requestCallback<_plugins_ShutdownResponse__Output>): grpc.ClientUnaryCall;
  
}

export interface PluginHandlers extends grpc.UntypedServiceImplementation {
  ExecuteCommand: grpc.handleUnaryCall<_plugins_CommandInvocation__Output, _plugins_CommandResult>;
  
  ExecuteTool: grpc.handleUnaryCall<_plugins_ToolCall__Output, _plugins_ToolResult>;
  
  Initialize: grpc.handleUnaryCall<_plugins_InitRequest__Output, _plugins_InitResponse>;
  
  ListRegistrations: grpc.handleUnaryCall<_plugins_ListRegistrationsRequest__Output, _plugins_Registrations>;
  
  OnEvent: grpc.handleUnaryCall<_plugins_Event__Output, _plugins_EventResponse>;
  
  OnTerminalInput: grpc.handleUnaryCall<_plugins_TerminalInput__Output, _plugins_TerminalInputResponse>;
  
  ReachTools: grpc.handleUnaryCall<_plugins_ToolCall__Output, _plugins_ReachToolsResponse>;
  
  RouteModel: grpc.handleUnaryCall<_plugins_ModelRouteRequest__Output, _plugins_ModelRouteResponse>;
  
  Shutdown: grpc.handleUnaryCall<_plugins_ShutdownRequest__Output, _plugins_ShutdownResponse>;
  
}

export interface PluginDefinition extends grpc.ServiceDefinition {
  ExecuteCommand: MethodDefinition<_plugins_CommandInvocation, _plugins_CommandResult, _plugins_CommandInvocation__Output, _plugins_CommandResult__Output>
  ExecuteTool: MethodDefinition<_plugins_ToolCall, _plugins_ToolResult, _plugins_ToolCall__Output, _plugins_ToolResult__Output>
  Initialize: MethodDefinition<_plugins_InitRequest, _plugins_InitResponse, _plugins_InitRequest__Output, _plugins_InitResponse__Output>
  ListRegistrations: MethodDefinition<_plugins_ListRegistrationsRequest, _plugins_Registrations, _plugins_ListRegistrationsRequest__Output, _plugins_Registrations__Output>
  OnEvent: MethodDefinition<_plugins_Event, _plugins_EventResponse, _plugins_Event__Output, _plugins_EventResponse__Output>
  OnTerminalInput: MethodDefinition<_plugins_TerminalInput, _plugins_TerminalInputResponse, _plugins_TerminalInput__Output, _plugins_TerminalInputResponse__Output>
  ReachTools: MethodDefinition<_plugins_ToolCall, _plugins_ReachToolsResponse, _plugins_ToolCall__Output, _plugins_ReachToolsResponse__Output>
  RouteModel: MethodDefinition<_plugins_ModelRouteRequest, _plugins_ModelRouteResponse, _plugins_ModelRouteRequest__Output, _plugins_ModelRouteResponse__Output>
  Shutdown: MethodDefinition<_plugins_ShutdownRequest, _plugins_ShutdownResponse, _plugins_ShutdownRequest__Output, _plugins_ShutdownResponse__Output>
}
