// Host service gRPC client.
//
// Tau serves the Host service on a plaintext unix socket whose path is in
// the TAU_HOST_SOCKET env var (set by agent/plugins/plugins.go during plugin
// launch). This client dials it once and exposes async method stubs that
// proxy plugin intent back to tau (UI prompts, session state, message
// sending, etc.).
//
// Every call sets the `tau-plugin-name` gRPC metadata so the Host handlers
// can namespace state and config per plugin.

import { Client, Metadata, credentials, type ServiceError } from "@grpc/grpc-js";
import { callUnary, loadProto, type HostMethod } from "./grpc.ts";
import type {
	GetConfigResponseP,
	SessionAppendResponseP,
	SessionGetLabelResponseP,
	SessionScanResponseP,
	SessionSendUserMessageResponseP,
	SessionSetLabelResponseP,
	UIConfirmResponseP,
	UIInputResponseP,
	UISelectResponseP,
} from "./proto-types.ts";
import type { NotifyLevel, NestedToolCall } from "./pi-types.ts";
import fs from "node:fs";
import path from "node:path";

// Per-call deadline (under tau's 5s on_event budget; leave headroom).
const DEFAULT_DEADLINE_MS = 4000;
// UI prompts block on the user; use a generous deadline.
const UI_DEADLINE_MS = 30_000;

const NOTIFY_LEVELS: ReadonlyArray<NotifyLevel> = ["info", "success", "warning", "error"];

export function resolvePluginName(cwd: string): string {
	return process.env["TAU_PLUGIN_NAME"] || path.basename(cwd);
}

export class HostClient {
	private readonly pluginName: string;
	private _client: Client | null = null;

	constructor(pluginName: string) {
		this.pluginName = pluginName;
	}

	socketPath(): string {
		return process.env["TAU_HOST_SOCKET"] ?? "";
	}

	private ensure(): Client {
		if (this._client) return this._client;
		const sock = this.socketPath();
		if (!sock) {
			throw new Error("TAU_HOST_SOCKET not set; cannot reach Host service");
		}
		if (!fs.existsSync(sock)) {
			throw new Error(`TAU_HOST_SOCKET ${sock} does not exist`);
		}
		// grpc-js unix socket target: "unix:/abs/path" (single slash).
		const { HostClient: HostClientCtor } = loadProto();
		this._client = new HostClientCtor(`unix:${sock}`, credentials.createInsecure()) as Client;
		return this._client;
	}

	private metadata(): Metadata {
		const md = new Metadata();
		md.add("tau-plugin-name", this.pluginName);
		return md;
	}

	private call<TReq, TRes>(method: HostMethod, request: TReq, deadlineMs = DEFAULT_DEADLINE_MS): Promise<TRes> {
		let client: Client;
		try {
			client = this.ensure();
		} catch (e) {
			return Promise.reject(e);
		}
		return callUnary<TReq, TRes>(client, method, request, this.metadata(), {
			deadline: Date.now() + deadlineMs,
		});
	}

	async toolDiscovery(
		operation: string,
		options: { names?: string[]; query?: string; namespace?: string; limit?: number } = {},
	): Promise<unknown> {
		const response = await this.call<unknown, { resultJson?: string }>("toolDiscovery", { operation, ...options });
		return JSON.parse(response.resultJson ?? "null");
	}

	async toolProgress(update: {
		toolCallId: string;
		content: string;
		isError?: boolean;
		nestedCalls?: NestedToolCall[];
	}): Promise<void> {
		await this.call("toolProgress", {
			...update,
			nestedCalls: update.nestedCalls === undefined ? "" : JSON.stringify(update.nestedCalls.slice(-128)),
		});
	}

	// --- UI methods ---------------------------------------------------------

	uiConfirm(title: string, message: string): Promise<UIConfirmResponseP> {
		return this.call("uiConfirm", { title, message }, UI_DEADLINE_MS);
	}

	uiSelect(title: string, options: string[]): Promise<UISelectResponseP> {
		return this.call("uiSelect", { title, options }, UI_DEADLINE_MS);
	}

	uiInput(title: string, placeholder: string): Promise<UIInputResponseP> {
		return this.call("uiInput", { title, placeholder }, UI_DEADLINE_MS);
	}

	uiNotify(message: string, level: NotifyLevel = "info"): Promise<void> {
		const idx = NOTIFY_LEVELS.indexOf(level);
		return this.call("uiNotify", { message, level: idx < 0 ? 0 : idx }).then(() => undefined);
	}

	uiSetStatus(key: string, message: string): Promise<void> {
		return this.call("uiSetStatus", { key, message }).then(() => undefined);
	}

	uiSetWidget(key: string, lines: string[]): Promise<void> {
		return this.call("uiSetWidget", { key, lines }).then(() => undefined);
	}

	uiSetTitle(title: string): Promise<void> {
		return this.call("uiSetTitle", { title }).then(() => undefined);
	}

	uiSetEditorText(text: string): Promise<void> {
		return this.call("uiSetEditorText", { text }).then(() => undefined);
	}

	uiSetToolsExpanded(expanded: boolean): Promise<void> {
		return this.call("uiSetToolsExpanded", { expanded }).then(() => undefined);
	}

	// --- Logging ------------------------------------------------------------

	log(message: string, level = 1): Promise<void> {
		return this.call("log", { message, level }).then(() => undefined);
	}

	// --- Config -------------------------------------------------------------

	getConfig(key: string): Promise<GetConfigResponseP> {
		return this.call("getConfig", { key });
	}

	// --- State --------------------------------------------------------------

	stateGet(key: string): Promise<{ value: string; exists: boolean }> {
		return this.call("stateGet", { key });
	}

	stateSet(key: string, value: string): Promise<void> {
		return this.call("stateSet", { key, value }).then(() => undefined);
	}

	stateAppend(key: string, value: string): Promise<void> {
		return this.call("stateAppend", { key, value }).then(() => undefined);
	}

	stateDelete(key: string): Promise<void> {
		return this.call("stateDelete", { key }).then(() => undefined);
	}

	// --- Session ------------------------------------------------------------

	sessionAppend(entryType: string, payloadJson: string, parentId = ""): Promise<SessionAppendResponseP> {
		return this.call("sessionAppend", { entryType, payload: payloadJson, parentId });
	}

	sessionScan(filters?: { entryTypeFilter?: string; customTypeFilter?: string }): Promise<SessionScanResponseP> {
		return this.call("sessionScan", {
			entryTypeFilter: filters?.entryTypeFilter ?? "",
			customTypeFilter: filters?.customTypeFilter ?? "",
		});
	}

	sessionSetLabel(entryId: string, label: string | undefined): Promise<SessionSetLabelResponseP> {
		return this.call("sessionSetLabel", { entryId, label: label ?? "" });
	}

	sessionGetLabel(entryId: string): Promise<SessionGetLabelResponseP> {
		return this.call("sessionGetLabel", { entryId });
	}

	// --- Message sending ----------------------------------------------------

	sessionSendUserMessage(
		text: string,
		deliverAs: "" | "steer" | "followUp" = "",
	): Promise<SessionSendUserMessageResponseP> {
		return this.call("sessionSendUserMessage", { text, deliverAs });
	}

	sessionSendCustomMessage(
		message: Record<string, unknown>,
		options?: { triggerTurn?: boolean; deliverAs?: "steer" | "followUp" | "nextTurn" },
	): Promise<SessionSendUserMessageResponseP> {
		return this.call("sessionSendCustomMessage", {
			messageJson: JSON.stringify(message),
			triggerTurn: options?.triggerTurn,
			deliverAs: options?.deliverAs ?? "",
		});
	}

	/** Close the underlying gRPC client (tests). */
	close(): void {
		if (this._client) {
			const c = this._client as Client & { close?: () => void };
			c.close?.();
			this._client = null;
		}
	}
}

export type { ServiceError };
