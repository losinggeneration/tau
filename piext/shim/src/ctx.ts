// ExtensionContext construction.
//
// pi handlers receive (event, ctx) where ctx exposes ui, cwd, sessionManager,
// model, isIdle(), etc. The ctx is built fresh per-handler-invocation because
// pi extensions can capture it and use it later (e.g. in setTimeout).
//
// Tier-3 (unsupported) ctx APIs warn once and no-op so extensions degrade
// instead of crashing — matching how pi extensions guard optional ctx
// (`ctx?.ui?.notify?.()` is idiomatic in the corpus).

import { theme } from "./theme.ts";
import { shimState } from "./state.ts";
import { SessionManager } from "./session-manager.ts";
import type { HostClient } from "./host.ts";
import type { ExtensionContext, NotifyLevel, PiUI } from "./pi-types.ts";
import { subscribeTerminalInput } from "./terminal-input.ts";

const warned = new Set<string>();
function warnOnce(api: string, msg: string): void {
	const k = `${api}:${msg}`;
	if (warned.has(k)) return;
	warned.add(k);
	process.stderr.write(`pi-ext-shim: ${api}: ${msg}\n`);
}

function logFailure(op: string, e: unknown): void {
	const msg = e instanceof Error ? e.message : String(e);
	process.stderr.write(`pi-ext-shim: ${op} failed: ${msg}\n`);
}

function makeUI(host: HostClient): PiUI {
	return {
		notify(message: string, level: NotifyLevel = "info"): void {
			// Fire-and-forget; notify is informational.
			host.uiNotify(String(message), level).catch((e: unknown) => logFailure("uiNotify", e));
		},
		async confirm(title: string, message: string): Promise<boolean> {
			const r = await host.uiConfirm(String(title ?? ""), String(message ?? ""));
			return Boolean(r?.confirmed);
		},
		async select(title: string, options: string[]): Promise<string | undefined> {
			const opts = Array.isArray(options) ? options.map(String) : [];
			const r = await host.uiSelect(String(title ?? ""), opts);
			const idx = typeof r?.selectedIndex === "number" ? r.selectedIndex : -1;
			return idx >= 0 ? opts[idx] : undefined;
		},
		async input(title: string, placeholder: string): Promise<string> {
			const r = await host.uiInput(String(title ?? ""), String(placeholder ?? ""));
			return r?.value ?? "";
		},
		setStatus(key: string, message: string): void {
			host.uiSetStatus(String(key), String(message ?? "")).catch((e: unknown) => logFailure("uiSetStatus", e));
		},
		setWidget(key: string, lines: string[]): void {
			const ls = Array.isArray(lines) ? lines.map(String) : [];
			host.uiSetWidget(String(key), ls).catch((e: unknown) => logFailure("uiSetWidget", e));
		},
		setTitle(title: string): void {
			host.uiSetTitle(String(title ?? "")).catch((e: unknown) => logFailure("uiSetTitle", e));
		},
		setHeader(_text: string): void {
			warnOnce("ctx.ui.setHeader", "no-op in pi-ext-shim");
		},
		setFooter(_text: string): void {
			warnOnce("ctx.ui.setFooter", "no-op in pi-ext-shim");
		},
		setTheme(_name: string): void {
			warnOnce("ctx.ui.setTheme", "no-op in pi-ext-shim");
		},
		setEditorText(text: string): void {
			host.uiSetEditorText(String(text ?? "")).catch((e: unknown) => logFailure("uiSetEditorText", e));
		},
		setEditorComponent(_component: unknown): void {
			warnOnce("ctx.ui.setEditorComponent", "no-op in pi-ext-shim");
		},
		setHiddenThinkingLabel(_label: string): void {
			warnOnce("ctx.ui.setHiddenThinkingLabel", "no-op in pi-ext-shim");
		},
		setToolsExpanded(expanded: boolean): void {
			host.uiSetToolsExpanded(Boolean(expanded)).catch((e: unknown) => logFailure("uiSetToolsExpanded", e));
		},
		onTerminalInput(handler: (input: unknown) => void | { consume?: boolean }): () => void {
			return subscribeTerminalInput(handler);
		},
		get theme() {
			return theme;
		},
		get custom(): unknown {
			warnOnce("ctx.ui.custom", "TUI-only; not supported in pi-ext-shim");
			return undefined;
		},
		get editor(): unknown {
			warnOnce("ctx.ui.editor", "TUI-only; not supported in pi-ext-shim");
			return undefined;
		},
	};
}

export interface BuildContextOptions {
	host: HostClient;
	cwd: string;
	pluginName: string;
}

/**
 * Build a per-call ExtensionContext. cwd comes from the init request
 * (session-level). The SessionManager wraps Host.SessionScan.
 */
export function buildContext({ host, cwd }: BuildContextOptions): ExtensionContext {
	const ui = makeUI(host);
	const sessionManager = new SessionManager();

	return {
		ui,
		hasUI: true,
		cwd: cwd ?? "",
		sessionManager,
		get model() {
			return shimState.model;
		},
		isIdle(): boolean {
			return shimState.isIdle;
		},
		get signal(): AbortSignal | undefined {
			return undefined;
		},
		abort(): void {
			warnOnce("ctx.abort", "no-op in pi-ext-shim (no AbortController wiring yet)");
		},
		hasPendingMessages(): boolean {
			return false; // No Host RPC for queue sizes yet.
		},
		shutdown(): void {
			warnOnce("ctx.shutdown", "no-op in pi-ext-shim");
		},
		getContextUsage(): undefined {
			return undefined;
		},
		compact(): void {
			warnOnce("ctx.compact", "no-op in pi-ext-shim");
		},
		getSystemPrompt(): string {
			warnOnce("ctx.getSystemPrompt", "not yet wired");
			return "";
		},
	};
}
