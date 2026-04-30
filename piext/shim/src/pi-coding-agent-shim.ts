import os from "node:os";
import path from "node:path";
import { theme } from "./theme.ts";

export const CONFIG_DIR_NAME = ".tau";
export const VERSION = "0.0.0-tau-shim";

export function getAgentDir(): string {
	return process.env.TAU_AGENT_DIR || path.join(os.homedir(), ".tau", "agent");
}

export function getConfigDirName(): string {
	return CONFIG_DIR_NAME;
}

export function getProjectConfigDir(cwd = process.cwd()): string {
	return path.join(cwd, ".tau");
}

export function resolveConfigDirName(): string {
	return CONFIG_DIR_NAME;
}

export function defineTool<T>(tool: T): T {
	return tool;
}

export function convertToLlm<T>(value: T): T {
	return value;
}

export function estimateTokens(value: unknown): number {
	const text = typeof value === "string" ? value : JSON.stringify(value ?? "");
	return Math.ceil(text.length / 4);
}

export function initTheme(): typeof theme {
	return theme;
}

export function getMarkdownTheme(): Record<string, unknown> {
	return {};
}

export function getSelectListTheme(): Record<string, unknown> {
	return {};
}

export function getSettingsListTheme(): Record<string, unknown> {
	return {};
}

export function getLanguageFromPath(file: string): string {
	return path.extname(file).replace(/^\./, "");
}

export function highlightCode(code: string): string[] {
	return String(code).split("\n");
}

export function keyText(keybinding: string): string {
	return keybinding;
}

export function keyHint(keybinding: string): string {
	return keybinding;
}

export function rawKeyHint(keybinding: string): string {
	return keybinding;
}

export class DynamicBorder {
	constructor(readonly format: (s: string) => string = (s) => s) {}
	render(text = ""): string {
		return this.format(text);
	}
}

export class Theme {}

export function createReadOnlyTools(): unknown[] {
	return [];
}

export const BorderedLoader = class {};
export const ArminComponent = class {};
export const AssistantMessageComponent = class {};
export const BashExecutionComponent = class {};
export const BranchSummaryMessageComponent = class {};
export const CompactionSummaryMessageComponent = class {};
export const CustomEditor = class {};
export const CustomMessageComponent = class {};
export const ExtensionEditorComponent = class {};
export const ExtensionInputComponent = class {};
export const ExtensionSelectorComponent = class {};
export const FooterComponent = class {};
export const LoginDialogComponent = class {};
export const ModelSelectorComponent = class {};
export const OAuthSelectorComponent = class {};
export const SessionSelectorComponent = class {};
export const SettingsSelectorComponent = class {};
export const ShowImagesSelectorComponent = class {};
export const SkillInvocationMessageComponent = class {};
export const ThemeSelectorComponent = class {};
export const ThinkingSelectorComponent = class {};
export const ToolExecutionComponent = class {};
export const TreeSelectorComponent = class {};
export const UserMessageComponent = class {};
export const UserMessageSelectorComponent = class {};
