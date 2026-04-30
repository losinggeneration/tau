// ANSI-styled theme approximation for pi's ctx.ui.theme. Extensions use
// theme.fg("accent"|"dim"|"muted"|"text", s) to style status text and
// status text and toasts. pi's real theme integrates with the TUI's color
// system; under tau we approximate with raw ANSI codes.
//
// The returned text is always a string — unknown colors fall through to the
// input text. Extensions commonly guard on theme?.fg, so the contract is
// "fg exists and returns a string."

import type { PiTheme } from "./pi-types.ts";

const escape = (code: string): string => `\x1b[${code}m`;
const reset = escape("0");

// Color name → ANSI SGR foreground code. 256-color codes approximate pi's
// palette slots well enough for status bars.
const fgMap: Record<string, string> = {
	accent: escape("38;5;39"), // bright cyan
	dim: escape("38;5;245"), // gray
	muted: escape("38;5;243"), // dimmer gray
	text: "", // default
	default: "",
	warning: escape("33"), // yellow
	error: escape("31"), // red
	success: escape("32"), // green
	info: escape("36"), // cyan
};

export const theme: PiTheme = {
	fg(color: string, text: string): string {
		const code = fgMap[color];
		if (code === undefined || code === "") return String(text);
		return `${code}${text}${reset}`;
	},
	bg(color: string, text: string): string {
		// Reuse fg codes for bg too — good enough for status bars.
		const code = fgMap[color];
		if (code === undefined || code === "") return String(text);
		return `${code}${text}${reset}`;
	},
};
