export class Agent {
	constructor(..._args: unknown[]) {}
	async run(): Promise<never> {
		throw new Error("pi-agent-core Agent is not supported by pi-ext-shim");
	}
}
