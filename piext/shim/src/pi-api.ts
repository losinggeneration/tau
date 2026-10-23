// The `pi` ExtensionAPI facade handed to extension factories.
//
// Tier-1 implementation for the common pi extension surface. Tier-2/3
// methods log a one-time warning and
// return safe defaults — see docs/pi-ext.md §6 for the support matrix.

import { execFile as cpExecFile } from "node:child_process";
import { promisify } from "node:util";
import type { EventRouter } from "./events.ts";
import type { HostClient } from "./host.ts";
import type {
	ExecOptions,
	ExecResult,
	ExtensionFactory,
	PiAPI,
	PiFlagOptions,
	PiVirtualModel,
	RegisteredCommand,
	ToolDefinition,
	ToolParameters,
} from "./pi-types.ts";

const execFileAsync = promisify(cpExecFile);

const warned = new Set<string>();
function warnOnce(key: string, msg: string): void {
	if (warned.has(key)) return;
	warned.add(key);
	process.stderr.write(`pi-ext-shim: ${key}: ${msg}\n`);
}

function logFailure(op: string, e: unknown): void {
	const msg = e instanceof Error ? e.message : String(e);
	process.stderr.write(`pi-ext-shim: ${op} failed: ${msg}\n`);
}

/** Registrations collected while extension factories run. */
export interface PiRegistrations {
	commands: Map<string, RegisteredCommand>;
	tools: Map<string, ToolDefinition>;
	flags: Map<string, PiFlagOptions & { name: string }>;
	/** Virtual models keyed by provider and id. */
	virtualModels: Map<string, PiVirtualModel>;
}

/** Convert a pi JSON-schema fragment to tau's map<string,string> parameters. */
export function toolParametersToMap(parameters: ToolParameters | undefined): Record<string, string> {
	const out: Record<string, string> = {};
	if (!parameters || typeof parameters !== "object") {
		out["type"] = "object";
		out["properties"] = "{}";
		return out;
	}
	for (const [key, value] of Object.entries(parameters)) {
		const encoded = JSON.stringify(value);
		if (encoded !== undefined) out[key] = encoded;
	}
	out["type"] ??= JSON.stringify("object");
	out["properties"] ??= "{}";
	return out;
}

export interface BuildPiOptions {
	host: HostClient;
	router: EventRouter;
	pluginName: string;
}

export function buildPi({ host, router, pluginName }: BuildPiOptions): PiAPI {
	const registrations: PiRegistrations = {
		commands: new Map(),
		tools: new Map(),
		flags: new Map(),
		virtualModels: new Map(),
	};

	const loadout = {
		ready: false,
		staged: [] as string[][],
		host,
		active: [] as string[],
		registered: [] as Array<Record<string, unknown>>,
		pending: Promise.resolve(),
		failure: undefined as unknown,
	};
	const api: PiAPI = {
		// ---- Events ---------------------------------------------------------
		on(event: string, handler) {
			return router.on(event, handler);
		},

		// ---- Tools ----------------------------------------------------------
		registerTool(tool: ToolDefinition): void {
			if (!tool || typeof tool.name !== "string") {
				throw new Error("registerTool: tool.name is required");
			}
			registrations.tools.set(tool.name, tool);
		},

		// ---- Commands -------------------------------------------------------
		registerCommand(name: string, options: { description?: string; handler: RegisteredCommand["handler"] }): void {
			if (typeof name !== "string") {
				throw new Error("registerCommand: name is required");
			}
			if (!options || typeof options.handler !== "function") {
				throw new Error(`registerCommand(${name}): handler is required`);
			}
			registrations.commands.set(name, {
				name,
				description: options.description ?? "",
				handler: options.handler,
			});
		},

		// ---- Shortcuts (TUI-only) -------------------------------------------
		registerShortcut(_shortcut: string, _options: { description?: string; handler: () => void }): void {
			warnOnce("pi.registerShortcut", "no-op in pi-ext-shim (TUI-only)");
		},

		// ---- CLI flags ------------------------------------------------------
		registerFlag(name: string, options: PiFlagOptions): void {
			if (typeof name !== "string") {
				throw new Error("registerFlag: name is required");
			}
			registrations.flags.set(name, {
				name,
				description: options?.description ?? "",
				type: options?.type ?? "string",
				default: options?.default ?? "",
			});
		},

		getFlag(_name: string): string | boolean | undefined {
			warnOnce("pi.getFlag", "values not yet wired (Phase 1.1)");
			return undefined;
		},

		// ---- Message rendering (TUI-only) -----------------------------------
		registerMessageRenderer(_customType: string, _renderer: unknown): void {
			warnOnce("pi.registerMessageRenderer", "no-op in pi-ext-shim (TUI-only)");
		},

		// ---- Actions --------------------------------------------------------
		// Extensions call sendMessage and sendUserMessage without awaiting, so
		// failures are logged rather than rejected.
		async sendMessage(
			message: Record<string, unknown>,
			options?: { triggerTurn?: boolean; deliverAs?: "steer" | "followUp" | "nextTurn" },
		): Promise<void> {
			try {
				const response = await host.sessionSendCustomMessage(message, options);
				if (!response.ok) throw new Error(response.error || "send failed");
			} catch (e) {
				logFailure("sendMessage", e);
			}
		},

		async sendUserMessage(content: string, options?: { deliverAs?: "steer" | "followUp" }): Promise<void> {
			const text = typeof content === "string" ? content : "";
			if (!text) return;
			const deliverAs = options?.deliverAs ?? "";
			try {
				const r = await host.sessionSendUserMessage(text, deliverAs);
				if (r && r.ok === false) {
					throw new Error(r.error || "send failed");
				}
			} catch (e) {
				logFailure("sendUserMessage", e);
			}
		},

		appendEntry(customType: string, data?: unknown): Promise<void> {
			// tau's session accessor pulls `custom_type` off the top of the
			// payload JSON; pi's surface treats it as a separate argument
			// (pi.appendEntry(data, { customType })). The shim bridges that
			// mismatch by merging the data's fields with `custom_type` at
			// the top level — pi extensions read `entry.data.<field>` and
			// tau stores both at the entry's data level.
			const obj = (
				data && typeof data === "object" && !Array.isArray(data) ? Object.assign({}, data as object) : { data }
			) as Record<string, unknown>;
			obj.custom_type = customType;
			const payload = JSON.stringify(obj);
			return host.sessionAppend("custom", payload, "").then(
				() => undefined,
				(e: unknown) => {
					logFailure("appendEntry", e);
				},
			);
		},

		// ---- Session metadata -----------------------------------------------
		setSessionName(_name: string): void {
			warnOnce("pi.setSessionName", "not yet implemented (Phase 1.1)");
		},
		getSessionName(): string | undefined {
			warnOnce("pi.getSessionName", "not yet implemented (Phase 1.1)");
			return undefined;
		},
		setLabel(entryId: string, label: string | undefined): Promise<void> {
			return host.sessionSetLabel(String(entryId), label).then(
				() => undefined,
				(e: unknown) => {
					logFailure("setLabel", e);
				},
			);
		},

		// ---- exec (local Node child_process) ---------------------------------
		async exec(command: string, args: string[], options?: ExecOptions): Promise<ExecResult> {
			const argv = Array.isArray(args) ? args : [];
			const opts = {
				cwd: options?.cwd,
				env: options?.env ? { ...process.env, ...options.env } : process.env,
				timeout: options?.timeout ?? 60_000,
				maxBuffer: 10 * 1024 * 1024,
			};
			try {
				const { stdout, stderr } = await execFileAsync(command, argv, opts);
				return { stdout, stderr, exitCode: 0, killed: false };
			} catch (e) {
				const err = e as NodeJS.ErrnoException & {
					stdout?: string;
					stderr?: string;
					code?: number | string;
					killed?: boolean;
				};
				return {
					stdout: err.stdout ?? "",
					stderr: err.stderr ?? "",
					exitCode: typeof err.code === "number" ? err.code : 1,
					killed: Boolean(err.killed),
				};
			}
		},

		// ---- Tool registry ---------------------------------------------------
		searchTools(query, options) {
			return host.toolDiscovery("search", { query, namespace: options?.namespace, limit: options?.limit ?? 8 });
		},
		describeTool(name) {
			return host.toolDiscovery("tool", { query: name });
		},
		describeNamespace(name) {
			return host.toolDiscovery("namespace", { query: name });
		},
		getActiveTools() {
			if (!loadout.ready)
				throw new Error("Extension runtime not initialized. Tool snapshots are available after session binding.");
			return [...loadout.active];
		},
		getAllTools() {
			if (!loadout.ready)
				throw new Error("Extension runtime not initialized. Tool snapshots are available after session binding.");
			return structuredClone(loadout.registered);
		},
		setActiveTools(names) {
			const selected = [...names];
			loadout.active = selected;
			if (!loadout.ready) {
				loadout.staged.push(selected);
				return;
			}
			loadout.pending = loadout.pending
				.then(async () => {
					await host.toolDiscovery("activate", { names: selected });
				})
				.catch((error) => {
					loadout.failure = error;
				});
		},
		getCommands(): Array<{ name: string; description: string; source: string }> {
			warnOnce("pi.getCommands", "partial: only returns this extension's commands");
			return Array.from(registrations.commands.values()).map((c) => ({
				name: c.name,
				description: c.description ?? "",
				source: pluginName,
			}));
		},

		// ---- Model + thinking ------------------------------------------------
		async setModel(_model: unknown): Promise<boolean> {
			warnOnce("pi.setModel", "not yet implemented (Phase 1.1)");
			return false;
		},
		getThinkingLevel(): undefined {
			warnOnce("pi.getThinkingLevel", "not yet implemented (Phase 1.1)");
			return undefined;
		},
		setThinkingLevel(_level: unknown): void {
			warnOnce("pi.setThinkingLevel", "not yet implemented (Phase 1.1)");
		},

		// ---- Providers --------------------------------------------------------
		registerProvider(_name: string, _config: Record<string, unknown>): void {
			warnOnce("pi.registerProvider", "not yet implemented (Phase 1.1)");
		},
		unregisterProvider(_name: string): void {
			warnOnce("pi.unregisterProvider", "not yet implemented (Phase 1.1)");
		},

		// ---- Virtual models ---------------------------------------------------
		// Registrations are read at load and reload; later changes apply on reload.
		registerVirtualModel(model: PiVirtualModel): void {
			if (!model || !model.provider?.trim() || !model.id?.trim()) {
				throw new Error("Virtual model provider and id must not be empty.");
			}
			if (typeof model.route !== "function") {
				throw new Error(`registerVirtualModel(${model.provider}/${model.id}): route is required`);
			}
			registrations.virtualModels.set(virtualModelKey(model.provider, model.id), model);
		},
		unregisterVirtualModel(provider: string, id: string): void {
			registrations.virtualModels.delete(virtualModelKey(provider, id));
		},

		// ---- EventBus (in-process) -------------------------------------------
		events: router.eventBus,
	};

	// Stash for server.ts (ListRegistrations) via a symbol-free side channel.
	registrationMap.set(api, registrations);
	loadoutMap.set(api, loadout);

	return api;
}

// Side channel from PiAPI → its registrations. The pi object handed to
// extensions stays clean; server.ts reads the collected state through here.
const registrationMap = new WeakMap<PiAPI, PiRegistrations>();

export function getRegistrations(api: PiAPI): PiRegistrations {
	return (
		registrationMap.get(api) ?? { commands: new Map(), tools: new Map(), flags: new Map(), virtualModels: new Map() }
	);
}

export function virtualModelKey(provider: string, id: string): string {
	return `${provider}/${id}`;
}

export type { ExtensionFactory };

const loadoutMap = new WeakMap<
	PiAPI,
	{
		ready: boolean;
		staged: string[][];
		host: HostClient;
		active: string[];
		registered: Array<Record<string, unknown>>;
		pending: Promise<void>;
		failure: unknown;
	}
>();

export async function flushToolLoadout(api: PiAPI): Promise<void> {
	const state = loadoutMap.get(api);
	if (!state) return;
	await state.pending;
	if (state.failure !== undefined) {
		const error = state.failure;
		state.failure = undefined;
		throw error;
	}
}

export async function refreshToolLoadout(api: PiAPI): Promise<void> {
	const state = loadoutMap.get(api);
	if (!state) return;
	for (const names of state.staged.splice(0)) {
		state.pending = state.pending
			.then(async () => {
				await state.host.toolDiscovery("activate", { names });
			})
			.catch((error) => {
				state.failure = error;
			});
	}
	await flushToolLoadout(api);
	const [registered, active] = await Promise.all([
		state.host.toolDiscovery("registered"),
		state.host.toolDiscovery("active"),
	]);
	state.registered = registered as Array<Record<string, unknown>>;
	state.active = active as string[];
	state.ready = true;
}
