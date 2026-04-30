// Tau is the bare executable entry point for the tau coding agent.
// It supports subcommands for auth management and model resolution,
// and defaults to running a single-shot conversation with the faux provider
// when given positional arguments.
package main

import "github.com/losinggeneration/tau/internal/app/cli"

func main() {
	cli.CLI()
}
