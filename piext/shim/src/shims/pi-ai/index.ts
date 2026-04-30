export const Type = {
	Object(properties: Record<string, unknown> = {}, options: Record<string, unknown> = {}): Record<string, unknown> {
		return { type: "object", properties, ...options };
	},
	String(options: Record<string, unknown> = {}): Record<string, unknown> {
		return { type: "string", ...options };
	},
	Number(options: Record<string, unknown> = {}): Record<string, unknown> {
		return { type: "number", ...options };
	},
	Boolean(options: Record<string, unknown> = {}): Record<string, unknown> {
		return { type: "boolean", ...options };
	},
	Array(items: unknown, options: Record<string, unknown> = {}): Record<string, unknown> {
		return { type: "array", items, ...options };
	},
	Enum(values: readonly unknown[], options: Record<string, unknown> = {}): Record<string, unknown> {
		return { enum: [...values], ...options };
	},
	Optional(schema: Record<string, unknown>): Record<string, unknown> {
		return { ...schema, optional: true };
	},
};

export function StringEnum(values: readonly string[], options: Record<string, unknown> = {}): Record<string, unknown> {
	return { type: "string", enum: [...values], ...options };
}

export function clampThinkingLevel(level: string | undefined): string {
	return level ?? "medium";
}

export function getSupportedThinkingLevels(): string[] {
	return ["low", "medium", "high"];
}

export async function streamSimple(): Promise<string> {
	throw new Error("pi-ai streamSimple is not supported by pi-ext-shim");
}
