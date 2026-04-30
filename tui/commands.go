package tui

import (
	"fmt"
	"strings"
)

type SlashCommand struct {
	Name         string
	Description  string
	ArgumentHint string
}

type CommandRegistry struct {
	commands  map[string]SlashCommand
	conflicts map[string]int
}

func NewCommandRegistry() *CommandRegistry {
	return &CommandRegistry{commands: map[string]SlashCommand{}, conflicts: map[string]int{}}
}

func (r *CommandRegistry) Register(command SlashCommand) {
	if r == nil || command.Name == "" {
		return
	}
	if _, exists := r.commands[command.Name]; exists {
		r.conflicts[command.Name]++
	}
	r.commands[command.Name] = command
}

func (r *CommandRegistry) Items(query string) []SelectorItem {
	if r == nil {
		return nil
	}
	var out []SelectorItem
	for _, command := range r.commands {
		if query != "" && !strings.Contains(strings.ToLower(command.Name), strings.ToLower(query)) {
			continue
		}
		desc := command.Description
		if command.ArgumentHint != "" {
			desc = strings.TrimSpace(command.ArgumentHint + " " + desc)
		}
		out = append(out, SelectorItem{Name: "/" + command.Name, Description: desc})
	}
	return out
}

func (r *CommandRegistry) ConflictDiagnostics() []string {
	if r == nil {
		return nil
	}
	var out []string
	for name, count := range r.conflicts {
		out = append(out, fmt.Sprintf("/%s registered %d times", name, count+1))
	}
	return out
}
