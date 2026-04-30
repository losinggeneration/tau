// The pi extension API subset the shim implements.
//
// Naming follows pi/coding-agent/src/core/extensions/types.ts (vendored at
// ../../pi/). Members pi exposes but the shim does not support yet are
// present on the interface where extensions call them — the implementations
// warn once and return a safe default (see pi-api.ts) — so this file stays
// an honest contract of what extensions can expect at runtime.

// ---------------------------------------------------------------------------
// Shared value types
// ---------------------------------------------------------------------------

export type NotifyLevel = "info" | "success" | "warning" | "error";

/** JSON schema fragment for tool parameters (TypeBox-style or plain JSON Schema). */
export interface ToolParameters {
	type?: string;
	properties?: Record<string, unknown>;
	required?: string[];
	[key: string]: unknown;
}

export interface ToolDefinition {
	name: string;
	description?: string;
	parameters?: ToolParameters;
	execute: (args: Record<string, unknown>, ctx: ExtensionContext) => Promise<ToolExecuteResult | void> | ToolExecuteResult | void;
}

export interface ToolExecuteResult {
	content?: Array<{ type: string; text?: string } | Record<string, unknown>>;
	isError?: boolean;
	details?: unknown;
}

export interface RegisteredCommand {
	name: string;
	description?: string;
	argumentHint?: string;
	handler: (args: string, ctx: ExtensionContext) => Promise<unknown> | unknown;
}

export interface ExecOptions {
	cwd?: string;
	env?: Record<string, string>;
	timeout?: number;
}

export interface ExecResult {
	stdout: string;
	stderr: string;
	exitCode: number;
	killed: boolean;
}

// ---------------------------------------------------------------------------
// Event shapes (what handlers receive)
// ---------------------------------------------------------------------------

/**
 * The event object handed to pi.on() handlers. The `type` discriminates;
 * per-event fields are exposed as optional because handlers commonly guard
 * (`event.systemPrompt ?? ""`). Unknown extra fields from tau's payloads
 * pass through.
 */
export interface PiEvent {
	type: string;
	// before_agent_start
	systemPrompt?: string;
	// input
	text?: string;
	source?: string;
	// tool_call / user_bash / tool_result
	toolName?: string;
	toolCallId?: string;
	input?: Record<string, unknown>;
	result?: string;
	isError?: boolean;
	// message_* / after_provider_response
	message?: Record<string, unknown>;
	// model_select / before_provider_request
	model?: Record<string, unknown>;
	// session_start
	id?: string;
	cwd?: string;
	reason?: string;
	previousSessionFile?: string;
	// session_shutdown
	sessionId?: string;
	// passthrough for tool_execution_* and future events
	[key: string]: unknown;
}

/** Union of result shapes handlers may return (all fields optional). */
export interface PiEventHandlerResult {
	block?: boolean;
	reason?: string;
	systemPrompt?: string;
	text?: string;
	content?: Array<{ type: string; text?: string } | Record<string, unknown>>;
	isError?: boolean;
	[key: string]: unknown;
}

export type PiEventHandler = (
	event: PiEvent,
	ctx: ExtensionContext,
) => PiEventHandlerResult | Promise<PiEventHandlerResult | void> | void;

// ---------------------------------------------------------------------------
// ctx (ExtensionContext / ExtensionCommandContext subset)
// ---------------------------------------------------------------------------

export interface PiTheme {
	fg: (color: string, text: string) => string;
	bg: (color: string, text: string) => string;
}

export interface PiUI {
	notify: (message: string, level?: NotifyLevel) => void;
	confirm: (title: string, message: string) => Promise<boolean>;
	select: (title: string, options: string[]) => Promise<string | undefined>;
	input: (title: string, placeholder: string) => Promise<string>;
	setStatus: (key: string, message: string) => void;
	setWidget: (key: string, lines: string[]) => void;
	setTitle: (title: string) => void;
	setHeader: (text: string) => void;
	setFooter: (text: string) => void;
	setTheme: (name: string) => void;
	setEditorText: (text: string) => void;
	setEditorComponent: (component: unknown) => void;
	setHiddenThinkingLabel: (label: string) => void;
	setToolsExpanded: (expanded: boolean) => void;
	onTerminalInput: (handler: (input: unknown) => void | { consume?: boolean }) => () => void;
	readonly theme: PiTheme;
	readonly custom: unknown;
	readonly editor: unknown;
}

export interface SessionManagerLike {
	// pi's SessionManager is in-memory and synchronous; the shim satisfies
	// this by pre-fetching entries when session_start arrives.
	getEntries: () => SessionEntryLike[];
	appendEntry: () => Promise<never>;
	fork: () => Promise<never>;
	navigateTree: () => Promise<never>;
	switchSession: () => Promise<never>;
}

export interface SessionEntryLike {
	id: string;
	// pi's SessionManager surface uses `type`; the shim bridges from the
	// proto `entry_type` field (camelCased to entryType) so extensions read
	// `entry.type` as pi does.
	type: string;
	customType: string;
	// pi extensions read `entry.data`; the shim exposes the parsed payload
	// under that name.
	data: unknown;
	parentId: string;
	timestamp: string;
	label: string;
}

export interface PiModel {
	id?: string;
	provider?: string;
	api?: string;
	[key: string]: unknown;
}

export interface ExtensionContext {
	ui: PiUI;
	hasUI: boolean;
	cwd: string;
	sessionManager: SessionManagerLike;
	readonly model: PiModel | undefined;
	isIdle: () => boolean;
	readonly signal: AbortSignal | undefined;
	abort: () => void;
	hasPendingMessages: () => boolean;
	shutdown: () => void;
	getContextUsage: () => undefined;
	compact: () => void;
	getSystemPrompt: () => string;
}

// ---------------------------------------------------------------------------
// pi (ExtensionAPI subset)
// ---------------------------------------------------------------------------

export interface PiFlagOptions {
	description?: string;
	type: "boolean" | "string";
	default?: boolean | string;
}

export interface PiAPI {
	on: (event: string, handler: PiEventHandler) => () => void;
	registerTool: (tool: ToolDefinition) => void;
	registerCommand: (name: string, options: Omit<RegisteredCommand, "name">) => void;
	registerShortcut: (shortcut: string, options: { description?: string; handler: () => void }) => void;
	registerFlag: (name: string, options: PiFlagOptions) => void;
	getFlag: (name: string) => string | boolean | undefined;
	registerMessageRenderer: (customType: string, renderer: unknown) => void;
	sendMessage: (message: Record<string, unknown>, options?: { triggerTurn?: boolean }) => void;
	sendUserMessage: (content: string, options?: { deliverAs?: "steer" | "followUp" }) => Promise<void>;
	appendEntry: (customType: string, data?: unknown) => Promise<void>;
	setSessionName: (name: string) => void;
	getSessionName: () => string | undefined;
	setLabel: (entryId: string, label: string | undefined) => Promise<void>;
	exec: (command: string, args: string[], options?: ExecOptions) => Promise<ExecResult>;
	getActiveTools: () => string[];
	getAllTools: () => Array<Record<string, unknown>>;
	setActiveTools: (toolNames: string[]) => void;
	getCommands: () => Array<{ name: string; description: string; source: string }>;
	setModel: (model: unknown) => Promise<boolean>;
	getThinkingLevel: () => undefined;
	setThinkingLevel: (level: unknown) => void;
	registerProvider: (name: string, config: Record<string, unknown>) => void;
	unregisterProvider: (name: string) => void;
	readonly events: NodeJS.EventEmitter;
}

/** Extension factory: default export of an extension module. */
export type ExtensionFactory = (pi: PiAPI) => void | Promise<void>;

export const API_VERSION = "1";
