package agent

import "errors"

// Static errors for the agent package.
// These are defined at package level to satisfy err113 linter rule.
var (
	ErrAlreadyProcessingPrompt     = errors.New("agent is already processing a prompt. use steer() or followUp() to queue messages, or wait for completion")
	ErrAlreadyProcessing           = errors.New("agent is already processing. wait for completion before continuing")
	ErrNoMessagesToContinue        = errors.New("no messages to continue from")
	ErrCannotContinueAssistant     = errors.New("cannot continue from message role: assistant")
	ErrCannotContinueNoMessages    = errors.New("cannot continue: no messages in context")
	ErrCannotContinueFromAssistant = errors.New("cannot continue from assistant message")
)
