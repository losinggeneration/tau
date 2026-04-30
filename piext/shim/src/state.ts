// Derived shim state: isIdle(), current model. The shim subscribes to
// agent_start/agent_end/model_select internally regardless of extension
// subscriptions, so the values stay current even if the extension ignores
// those events.

import type { PiModel, SessionEntryLike } from "./pi-types.ts";

type StateListener = (value: unknown) => void;
interface ListenerEntry {
	event: string;
	fn: StateListener;
}

class ShimState {
	private _isIdle = true;
	private _model: PiModel | undefined;
	private _sessionEntries: SessionEntryLike[] = [];
	private _listeners: Set<ListenerEntry> = new Set();

	markAgentStart(): void {
		this._isIdle = false;
		this.emit("isIdle", false);
	}

	markAgentEnd(): void {
		this._isIdle = true;
		this.emit("isIdle", true);
	}

	setModel(model: PiModel): void {
		this._model = model;
		this.emit("model", model);
	}

	setSessionEntries(entries: SessionEntryLike[]): void {
		this._sessionEntries = entries;
	}

	get sessionEntries(): SessionEntryLike[] {
		return this._sessionEntries;
	}

	get isIdle(): boolean {
		return this._isIdle;
	}

	get model(): PiModel | undefined {
		return this._model;
	}

	on(event: string, fn: StateListener): () => void {
		const entry = { event, fn };
		this._listeners.add(entry);
		return () => {
			this._listeners.delete(entry);
		};
	}

	private emit(event: string, value: unknown): void {
		for (const l of this._listeners) {
			if (l.event === event) {
				try {
					l.fn(value);
				} catch {
					// best-effort
				}
			}
		}
	}
}

export const shimState = new ShimState();
