// Typed gRPC plumbing: loads plugin.proto once and exposes typed client
// constructors and service definitions.
//
// This module owns every `as unknown as` cast at the proto-loader boundary;
// everything downstream is fully typed. If the proto changes, the shapes in
// proto-types.ts and the casts here are the only places to touch.

import {
	loadPackageDefinition,
	type CallOptions,
	type Client,
	type Metadata,
	type ServiceClientConstructor,
	type ServiceDefinition,
	type ServiceError,
	type UntypedServiceImplementation,
} from "@grpc/grpc-js";
import { loadSync } from "@grpc/proto-loader";
import path from "node:path";
import { fileURLToPath } from "node:url";

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const PROTO_PATH = path.resolve(__dirname, "../proto/plugin.proto");

interface LoadedProto {
	PluginClient: ServiceClientConstructor;
	HostClient: ServiceClientConstructor;
	PluginService: ServiceDefinition;
	HostService: ServiceDefinition;
}

let cached: LoadedProto | null = null;

export function loadProto(): LoadedProto {
	if (cached) return cached;
	const packageDefinition = loadSync(PROTO_PATH, {
		keepCase: false,
		longs: String,
		enums: String,
		defaults: true,
		oneofs: true,
	});
	// proto-loader returns an untyped GrpcObject; the plugins package exposes
	// exactly the Plugin and Host service constructors.
	const loaded = loadPackageDefinition(packageDefinition) as unknown as {
		plugins: { Plugin: ServiceClientConstructor; Host: ServiceClientConstructor };
	};
	cached = {
		PluginClient: loaded.plugins.Plugin,
		HostClient: loaded.plugins.Host,
		PluginService: loaded.plugins.Plugin.service,
		HostService: loaded.plugins.Host.service,
	};
	return cached;
}

/**
 * A typed unary-method client: calling a method returns a promise of the
 * response. Used for both the Host client (shim → tau) and test clients
 * (test → shim's Plugin service).
 */
export type UnaryMethod<TReq, TRes> = (
	request: TReq,
	metadata: Metadata,
	options: CallOptions,
	callback: (err: ServiceError | null, res?: TRes) => void,
) => void;

export type UnaryClient = Client & Record<string, UnaryMethod<never, never>>;

/** Names of the Host service methods (camelCase per keepCase: false). */
export type HostMethod =
	| "uiConfirm"
	| "uiSelect"
	| "uiInput"
	| "uiNotify"
	| "uiSetStatus"
	| "uiSetWidget"
	| "uiSetTitle"
	| "uiSetEditorText"
	| "uiSetToolsExpanded"
	| "log"
	| "getConfig"
	| "toolProgress"
	| "stateGet"
	| "stateSet"
	| "stateAppend"
	| "stateDelete"
	| "sessionAppend"
	| "sessionScan"
	| "sessionSetLabel"
	| "sessionGetLabel"
	| "sessionSendUserMessage";

/** Names of the Plugin service methods. */
export type PluginMethod =
	| "initialize"
	| "listRegistrations"
	| "onEvent"
	| "executeTool"
	| "executeCommand"
	| "onTerminalInput"
	| "shutdown";

/**
 * Invoke a unary method on a gRPC client, promise-wrapped.
 *
 * The cast is contained here: grpc-js clients created from dynamically loaded
 * protos have untyped methods; callers pass the request/response type
 * parameters explicitly.
 */
export function callUnary<TReq, TRes>(
	client: Client,
	method: string,
	request: TReq,
	metadata: Metadata,
	options: CallOptions,
): Promise<TRes> {
	// eslint-disable-next-line @typescript-eslint/no-unnecessary-type-assertion -- dynamic proto client
	const fn = (client as unknown as Record<string, UnaryMethod<TReq, TRes>>)[method];
	if (typeof fn !== "function") {
		return Promise.reject(new Error(`unknown gRPC method: ${method}`));
	}
	return new Promise<TRes>((resolve, reject) => {
		fn.call(client, request, metadata, options, (err, res) => {
			if (err) reject(err);
			else resolve(res as TRes);
		});
	});
}

export type { UntypedServiceImplementation, ServiceError };
