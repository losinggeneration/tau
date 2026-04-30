// Read-only SessionManager facade for pi extensions.
//
// Session entries are pre-fetched on session_start so pi's synchronous
// SessionManager facade can serve extensions that restore state from their
// custom entries without awaiting.
//
// Unsupported write APIs throw informative errors so extensions fail loud.

import type { SessionEntryLike } from "./pi-types.ts";
import type { HostClient } from "./host.ts";
import type { SessionEntryP } from "./proto-types.ts";
import { shimState } from "./state.ts";

function normalize(e: SessionEntryP): SessionEntryLike {
	let data: unknown = e.payload;
	if (typeof data === "string" && data.length > 0) {
		try {
			data = JSON.parse(data);
		} catch {
			// leave as string
		}
	}
	return {
		id: e.id ?? "",
		type: e.entryType ?? "",
		customType: e.customType ?? "",
		data,
		parentId: e.parentId ?? "",
		timestamp: e.timestamp ?? "",
		label: e.label ?? "",
	};
}

/**
 * Pre-fetch session entries via Host.SessionScan into the sync cache. Called
 * when session_start arrives, before handlers run. Failures (e.g. a lazy
 * session whose file isn't materialized yet) leave the cache empty.
 */
export async function refreshSessionEntries(host: HostClient): Promise<void> {
	const resp = await host.sessionScan({}).catch(() => undefined);
	const entries = resp && Array.isArray(resp.entries) ? resp.entries : [];
	shimState.setSessionEntries(entries.map(normalize));
}

export class SessionManager {
	getEntries(): SessionEntryLike[] {
		return shimState.sessionEntries;
	}

	// Unsupported write APIs — fail loud.
	async appendEntry(): Promise<never> {
		throw new Error("pi-ext-shim: ctx.sessionManager.appendEntry not supported; use pi.appendEntry");
	}
	async fork(): Promise<never> {
		throw new Error("pi-ext-shim: ctx.sessionManager.fork not supported; use pi.sendUserMessage");
	}
	async navigateTree(): Promise<never> {
		throw new Error("pi-ext-shim: ctx.sessionManager.navigateTree not supported");
	}
	async switchSession(): Promise<never> {
		throw new Error("pi-ext-shim: ctx.sessionManager.switchSession not supported");
	}
}
