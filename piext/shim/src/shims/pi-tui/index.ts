export const CURSOR_MARKER = "";

export const Key = {
	ctrlAlt(key: string): string {
		return `ctrl+alt+${key}`;
	},
	ctrl(key: string): string {
		return `ctrl+${key}`;
	},
	alt(key: string): string {
		return `alt+${key}`;
	},
};

export function matchesKey(actual: unknown, expected: unknown): boolean {
	return actual === expected;
}

export function visibleWidth(value: unknown): number {
	return String(value ?? "").replace(/\x1b\[[0-9;]*m/g, "").length;
}

export function truncateToWidth(value: unknown, width: number): string {
	return String(value ?? "").slice(0, Math.max(0, width));
}

export function wrapTextWithAnsi(value: unknown): string[] {
	return String(value ?? "").split("\n");
}

// ponytail: regex covers SGR + CSI cursor sequences pi emits; widen if a caller hits an unhandled escape.
export function stripTerminalSequences(value: unknown): string {
	return String(value ?? "").replace(/\x1b\[[0-9;?]*[ -/]*[@-~]/g, "");
}

export function fuzzyFilter<T>(items: T[]): T[] {
	return items;
}

export class Renderable {
	render(): unknown {
		return "";
	}
}

export class Box extends Renderable {}
export class Container extends Renderable {}
export class Editor extends Renderable {}
export class Input extends Renderable {}
export class Loader extends Renderable {}
export class Markdown extends Renderable {}
export class Spacer extends Renderable {}
export class Text extends Renderable {}
export class TuiAltScreen extends Renderable {}
export class VStack extends Renderable {}

export class ScrollView {
	isFollowingEnd = true;
	scrollTop = 0;
	updateLayout(_contentHeight: number, _viewportHeight: number, _requestRender: () => void): void {}
	scrollTo(scrollTop: number, _options?: Record<string, unknown>): void {
		this.scrollTop = scrollTop;
	}
}
