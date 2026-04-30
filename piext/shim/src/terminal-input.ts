export interface TerminalInput {
	key: string;
	text: string;
	mod: string;
}

export type TerminalInputHandler = (input: TerminalInput | string) => void | { consume?: boolean };

const handlers = new Set<TerminalInputHandler>();

export function subscribeTerminalInput(handler: TerminalInputHandler): () => void {
	handlers.add(handler);
	return () => {
		handlers.delete(handler);
	};
}

export function dispatchTerminalInput(input: TerminalInput): boolean {
	let consume = false;
	for (const handler of Array.from(handlers)) {
		const result = handler(input);
		if (result && typeof result === "object" && result.consume === true) consume = true;
	}
	return consume;
}
