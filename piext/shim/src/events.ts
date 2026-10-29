// pi event name → shim handler signature translation.
//
// tau dispatches events to plugins with names from a small set (see
// docs/pi-ext.md §7). pi extensions subscribe using pi's full event surface.
// We translate tau's JSON payloads into pi-shaped event objects and collect
// handler results for the OnEvent RPC return value.

import { EventEmitter } from "node:events";
import type { ExtensionContext, PiEvent, PiEventHandler, PiEventHandlerResult } from "./pi-types.ts";

const HANDLED_EVENTS = new Set<string>([
	"resources_discover",
	"session_start",
	"session_before_switch",
	"session_before_fork",
	"session_before_compact",
	"session_compact",
	"session_shutdown",
	"session_before_tree",
	"session_tree",
	"context",
	"before_provider_request",
	"after_provider_response",
	"before_agent_start",
	"agent_start",
	"agent_end",
	"agent_settled",
	"turn_start",
	"turn_end",
	"message_start",
	"message_update",
	"message_end",
	"message_user",
	"message_assistant",
	"message_system",
	"tool_execution_start",
	"tool_execution_update",
	"tool_execution_end",
	"model_select",
	"tool_call",
	"tool_result",
	"user_bash",
	"input",
	"project_trust",
]);

const CANCELLABLE = new Set<string>([
	"tool_call",
	"session_before_fork",
	"session_before_compact",
	"session_before_switch",
	"session_before_tree",
]);

interface Payload {
	[key: string]: unknown;
}

function parsePayload(payloadJson: string): Payload {
	if (typeof payloadJson !== "string" || payloadJson.length === 0) return {};
	try {
		const parsed: unknown = JSON.parse(payloadJson);
		if (parsed && typeof parsed === "object" && !Array.isArray(parsed)) {
			return parsed as Payload;
		}
	} catch {
		// fall through
	}
	return {};
}

function asString(v: unknown): string | undefined {
	return typeof v === "string" ? v : undefined;
}

function asRecord(v: unknown): Record<string, unknown> | undefined {
	if (v && typeof v === "object" && !Array.isArray(v)) {
		return v as Record<string, unknown>;
	}
	return undefined;
}

function parseArguments(raw: unknown): Record<string, unknown> {
	const s = asString(raw);
	if (s && s.length > 0) {
		try {
			const parsed: unknown = JSON.parse(s);
			return asRecord(parsed) ?? {};
		} catch {
			return {};
		}
	}
	return {};
}

/** Project a tau payload into the pi event shape. */
export function buildEvent(name: string, raw: Payload): PiEvent {
	const event: PiEvent = { type: name };
	switch (name) {
		case "before_agent_start":
			event.systemPrompt = asString(raw["systemPrompt"]) ?? "";
			break;
		case "input":
			// tau dispatches input with the message payload shape ({role, content});
			// pi expects {text, source}. Fall back to content (string when the
			// message is a single text block, i.e. normal typed input).
			event.text = asString(raw["text"]) ?? asString(raw["content"]) ?? "";
			event.source = asString(raw["source"]) ?? "user";
			break;
		case "tool_call":
		case "user_bash":
			event.toolName = asString(raw["toolName"]) ?? (name === "user_bash" ? "bash" : "");
			event.toolCallId = asString(raw["toolCallId"]) ?? "";
			event.input = parseArguments(raw["arguments"]);
			break;
		case "tool_result":
			event.toolName = asString(raw["toolName"]) ?? "";
			event.toolCallId = asString(raw["toolCallId"]) ?? "";
			event.result = asString(raw["result"]) ?? "";
			event.isError = raw["isError"] === true;
			break;
		case "message_start":
		case "message_update":
		case "message_end":
			event.message = asRecord(raw["message"]) ?? {};
			break;
		case "message_user":
		case "message_assistant":
		case "message_system":
			event.message = raw;
			break;
		case "model_select":
		case "before_provider_request":
			event.model = asRecord(raw) ?? {};
			break;
		case "after_provider_response":
			event.message = asRecord(raw) ?? {};
			break;
		case "session_start":
			event.id = asString(raw["id"]) ?? "";
			event.cwd = asString(raw["cwd"]) ?? "";
			event.model = asRecord({ id: asString(raw["model"]) ?? "" }) ?? {};
			event.reason = asString(raw["reason"]) ?? "new";
			event.previousSessionFile = asString(raw["previousSessionFile"]) ?? "";
			break;
		case "session_shutdown":
			event.sessionId = asString(raw["session_id"]) ?? "";
			break;
		default:
			// tool_execution_* and anything else: pass payload fields through.
			for (const [k, v] of Object.entries(raw)) event[k] = v;
			break;
	}
	return event;
}

/** Narrow an unknown handler return into PiEventHandlerResult (null if not object). */
function asHandlerResult(v: unknown): PiEventHandlerResult | null {
	if (v === null || v === undefined) return null;
	if (typeof v !== "object" || Array.isArray(v)) return null;
	return v as PiEventHandlerResult;
}

interface Collected {
	systemPrompt?: string;
	toolCallId?: string;
	input?: Record<string, unknown>;
	content?: PiEventHandlerResult["content"];
	isError?: boolean;
	text?: string;
	block?: boolean;
	trusted?: "yes" | "no";
	remember?: boolean;
}

function serializeResponseData(name: string, collected: Collected[]): string {
	if (collected.length === 0) return "";
	const last = collected[collected.length - 1]!;
	switch (name) {
		case "before_agent_start":
			return JSON.stringify({ systemPrompt: last.systemPrompt ?? "" });
		case "tool_call":
			return JSON.stringify({ arguments: last.input ?? {} });
		case "tool_result":
			return JSON.stringify({ content: last.content, is_error: Boolean(last.isError) });
		case "input":
			return JSON.stringify({ text: last.text, block: Boolean(last.block) });
		case "project_trust":
			return JSON.stringify({ trusted: last.trusted, remember: Boolean(last.remember) });
		default:
			return "";
	}
}

export interface DispatchResult {
	block: boolean;
	blockReason: string;
	responseData: string;
}

type InternalHandler = (payload: Payload) => void;

/**
 * EventRouter: pi.on() handler registry + dispatcher.
 *
 * - on(name, fn): register a handler. fn may be sync or async; both are
 *   awaited sequentially in registration order.
 * - subscribeInternal(name, fn): shim-internal subscription (isIdle, model).
 * - dispatch(name, payloadJson): runs all matching handlers (ext + internal)
 *   and returns { block, responseData }.
 */
export class EventRouter {
	private _handlers = new Map<string, PiEventHandler[]>();
	private _internal = new Map<string, InternalHandler[]>();
	readonly eventBus = new EventEmitter();

	on(eventName: string, fn: PiEventHandler): () => void {
		if (typeof fn !== "function") return () => {};
		const list = this._handlers.get(eventName) ?? [];
		list.push(fn);
		this._handlers.set(eventName, list);
		return () => {
			const cur = this._handlers.get(eventName);
			if (!cur) return;
			const idx = cur.indexOf(fn);
			if (idx >= 0) cur.splice(idx, 1);
		};
	}

	subscribeInternal(eventName: string, fn: InternalHandler): () => void {
		const list = this._internal.get(eventName) ?? [];
		list.push(fn);
		this._internal.set(eventName, list);
		return () => {
			const cur = this._internal.get(eventName);
			if (!cur) return;
			const idx = cur.indexOf(fn);
			if (idx >= 0) cur.splice(idx, 1);
		};
	}

	/** Names the extension subscribed to (for ListRegistrations). */
	subscribedEvents(): string[] {
		return Array.from(this._handlers.keys());
	}

	/**
	 * dispatch(name, payloadJson, makeCtx?): runs all matching handlers
	 * (ext + internal). pi handlers receive (event, ctx); makeCtx builds the
	 * ExtensionContext for this dispatch (server.ts wires it to buildContext).
	 */
	async dispatch(name: string, payloadJson: string, makeCtx?: () => ExtensionContext): Promise<DispatchResult> {
		const payload = parsePayload(payloadJson);

		// Internal subscribers first.
		for (const fn of this._internal.get(name) ?? []) {
			try {
				fn(payload);
			} catch {
				// never crash the router from an internal bug
			}
		}

		if (!HANDLED_EVENTS.has(name)) {
			return { block: false, blockReason: "", responseData: "" };
		}

		const event = buildEvent(name, payload);
		const handlers = this._handlers.get(name) ?? [];
		const ctx = makeCtx ? makeCtx() : undefined;
		let block = false;
		let blockReason = "";
		const collected: Collected[] = [];

		for (const fn of handlers) {
			let rawResult: unknown;
			try {
				rawResult = fn(event, ctx as ExtensionContext);
			} catch (e) {
				stderr(`handler for "${name}" threw: ${errorMessage(e)}`);
				continue;
			}
			// Await async handlers; sync returns resolve immediately.
			let resolved: unknown = rawResult;
			if (rawResult && typeof (rawResult as { then?: unknown }).then === "function") {
				try {
					resolved = await rawResult;
				} catch (e) {
					stderr(`async handler for "${name}" threw: ${errorMessage(e)}`);
					continue;
				}
			}

			const result = asHandlerResult(resolved);
			if (result) {
				if (CANCELLABLE.has(name) && result["block"] === true) {
					block = true;
					blockReason = asString(result["reason"]) ?? asString(result["blockReason"]) ?? "";
				}
				if (name === "before_agent_start" && typeof result["systemPrompt"] === "string") {
					const sp = result["systemPrompt"];
					collected.push({ systemPrompt: sp });
					// pi chaining: each handler's return replaces
					// event.systemPrompt for the next handler.
					event.systemPrompt = sp;
				}
				if (name === "tool_result") {
					if (result["content"] !== undefined || result["isError"] !== undefined) {
						collected.push({
							toolCallId: event.toolCallId,
							content: result["content"],
							isError: result["isError"] === true,
						});
					}
				}
				if (name === "input" && (result["text"] !== undefined || result["block"] === true)) {
					collected.push({ text: asString(result["text"]), block: result["block"] === true });
				}
				// The first project_trust handler answering yes/no wins; undecided falls through.
				if (name === "project_trust" && (result["trusted"] === "yes" || result["trusted"] === "no")) {
					collected.push({ trusted: result["trusted"], remember: result["remember"] === true });
					break;
				}
			}
			if (name === "tool_call") {
				// Always collect so input-mutation patterns (pi) are visible.
				collected.push({ toolCallId: event.toolCallId, input: event.input ?? {} });
			}
		}

		return {
			block,
			blockReason,
			responseData: serializeResponseData(name, collected),
		};
	}
}

function stderr(msg: string): void {
	process.stderr.write(`pi-ext-shim: ${msg}\n`);
}

function errorMessage(e: unknown): string {
	if (e instanceof Error) return e.message;
	return String(e);
}

export { HANDLED_EVENTS, CANCELLABLE };
