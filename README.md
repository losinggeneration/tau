# tau

tau is a terminal-based AI coding agent written in Go. It supports most major
model providers, keeps persistent resumable sessions, and you can extend it
with packages, plugins, and MCP servers.

## This is a personal project

tau is something I built for myself. I'm sharing it because it might be useful
to someone else, not because I want to run a project. Anyone is free to fork
and modify it as they want, and the license doesn't need my permission for
anything. This has no desire to be a community project, so issues and pull
requests are disabled on GitHub and I'm not looking for contributors. Don't
expect this repo to merge your work or take your direction. Features exist
because I want them, and I fix bugs when they annoy me. If you need something
stable and supported, fork and maintain your own line.

If that works for you, read on.

## Why tau exists

tau started as a port of [pi](https://github.com/earendil-works/pi) to Go. It
had two purposes at first: to see if the models at the start of 2026 could do
it, and to test out [rovel](https://github.com/losinggeneration/rovel), my
terminal UI library. It has since become my main agent and harness, but that
wasn't an initial goal.

Since it's a port, if you just want a coding agent to use, you're probably
better off with pi itself.

## Features

- Terminal UI for interactive coding sessions, plus one-shot prompts from the
  command line
- Providers: Anthropic (including OAuth), OpenAI, OpenAI Codex, Google Gemini,
  Vertex AI, Azure OpenAI, Amazon Bedrock, GitHub Copilot, Mistral, Z.ai,
  OpenRouter, local models via llama.cpp, and Claude models through a Claude
  Agent SDK backend
- Sessions are stored as JSONL, with list/resume/fork/compact/export under
  `tau sessions`
- Extensible through packages (skills, prompts, themes, extensions), subprocess
  plugins over gRPC, and MCP servers
- A coordinator daemon (`tau daemon`) that runs agents as separate processes,
  so closing a client doesn't stop a running agent
- Clients for the daemon: the terminal UI and a Go client library. Remote
  access goes over SSH with per-device tokens
- An embeddable SDK for driving tau from Go code
- Built-in tools: read, write, edit, bash, grep, find, ls

## Building

Requires Go 1.26+.

```
go build -o tau ./cmd/tau
```

## Quick start

```
tau login anthropic            # or: tau login anthropic --oauth
tau set-model anthropic/claude-sonnet-4
tau "what does this repo do?"
```

Without arguments, tau starts an interactive session in the current directory.
`tau --help` lists all commands; `--standalone` runs the UI without
connecting to a running instance.

## Packages

tau can install skills, prompts, themes, and extensions:

```
tau install github.com/user/some-package
tau packages list
```

## License

[The Universal Permissive License (UPL), Version 1.0](LICENSE).
