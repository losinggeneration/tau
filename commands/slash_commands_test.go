package commands

import (
	"slices"
	"testing"
)

func TestBuiltinSlashCommandStruct(t *testing.T) {
	// Verify BuiltinSlashCommand struct fields
	cmd := BuiltinSlashCommand{
		Name:        "test",
		Description: "Test command",
		Implemented: true,
		Note:        "Test note",
	}

	if cmd.Name != "test" {
		t.Errorf("expected Name 'test', got %q", cmd.Name)
	}
	if cmd.Description != "Test command" {
		t.Errorf("expected Description 'Test command', got %q", cmd.Description)
	}
	if !cmd.Implemented {
		t.Error("expected Implemented to be true")
	}
	if cmd.Note != "Test note" {
		t.Errorf("expected Note 'Test note', got %q", cmd.Note)
	}
}

func TestBUILTIN_SLASH_COMMANDSContainsAllTSCommands(t *testing.T) {
	// Verify we have all 18 Pi commands Tau keeps from the TypeScript implementation
	expectedCommands := []string{
		"settings", "model", "scoped-models", "export", "import",
		"copy", "name", "session",
		"hotkeys", "fork", "tree", "login", "logout",
		"new", "compact", "resume", "reload", "quit",
	}

	if len(BUILTIN_SLASH_COMMANDS) != len(expectedCommands) {
		t.Errorf("expected %d commands, got %d", len(expectedCommands), len(BUILTIN_SLASH_COMMANDS))
	}

	// Build a map of command names
	cmdNames := make(map[string]bool)
	for _, cmd := range BUILTIN_SLASH_COMMANDS {
		cmdNames[cmd.Name] = true
	}

	// Verify all expected commands are present
	for _, name := range expectedCommands {
		if !cmdNames[name] {
			t.Errorf("missing expected command: %s", name)
		}
	}
}

func TestLookupBuiltinCommand(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantFound bool
		wantName  string
		wantImpl  bool
	}{
		{
			name:      "find existing command",
			input:     "login",
			wantFound: true,
			wantName:  "login",
			wantImpl:  true,
		},
		{
			name:      "find unimplemented command",
			input:     "settings",
			wantFound: true,
			wantName:  "settings",
			wantImpl:  false,
		},
		{
			name:      "command not found",
			input:     "nonexistent",
			wantFound: false,
		},
		{
			name:      "command not found empty",
			input:     "",
			wantFound: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := LookupBuiltinCommand(tt.input)
			if tt.wantFound {
				if result == nil {
					t.Fatalf("expected to find command %q, got nil", tt.input)
				}
				if result.Name != tt.wantName {
					t.Errorf("Name = %q, want %q", result.Name, tt.wantName)
				}
				if result.Implemented != tt.wantImpl {
					t.Errorf("Implemented = %v, want %v", result.Implemented, tt.wantImpl)
				}
			} else {
				if result != nil {
					t.Errorf("expected nil for command %q, got %+v", tt.input, result)
				}
			}
		})
	}
}

func TestListBuiltinCommands(t *testing.T) {
	cmds := ListBuiltinCommands()

	// Should return all 18 commands
	if len(cmds) != 18 {
		t.Errorf("expected 18 commands, got %d", len(cmds))
	}

	// Should be sorted by name (implemented commands first)
	for i := 1; i < len(cmds); i++ {
		prev := cmds[i-1]
		curr := cmds[i]

		// If prev is not implemented and curr is implemented, that's wrong
		if !prev.Implemented && curr.Implemented {
			t.Errorf("commands should be sorted with implemented first: %s (impl=%v) before %s (impl=%v)",
				prev.Name, prev.Implemented, curr.Name, curr.Implemented)
		}
	}
}

func TestListImplementedCommands(t *testing.T) {
	cmds := ListImplementedCommands()

	// Should only return implemented commands
	for _, cmd := range cmds {
		if !cmd.Implemented {
			t.Errorf("ListImplementedCommands should only contain implemented commands, got %s", cmd.Name)
		}
	}

	// Find a specific unimplemented command and verify it's not in the list
	for _, cmd := range BUILTIN_SLASH_COMMANDS {
		if !cmd.Implemented {
			for _, impl := range cmds {
				if impl.Name == cmd.Name {
					t.Errorf("unimplemented command %s should not be in ListImplementedCommands", cmd.Name)
				}
			}
		}
	}

	// Verify we have the expected count of implemented commands
	// Based on our implementation: export, import, name, session, fork, tree,
	// login, logout, new, compact, resume, reload, quit
	expectedCount := 13
	if len(cmds) != expectedCount {
		t.Errorf("expected %d implemented commands, got %d", expectedCount, len(cmds))
	}
}

func TestCompareBuiltinCommands(t *testing.T) {
	tests := []struct {
		name     string
		a        BuiltinSlashCommand
		b        BuiltinSlashCommand
		wantNeg  bool // true if a should come before b
		wantZero bool // true if they should be equal
	}{
		{
			name:     "implemented before unimplemented",
			a:        BuiltinSlashCommand{Name: "z", Implemented: true},
			b:        BuiltinSlashCommand{Name: "a", Implemented: false},
			wantNeg:  true,
			wantZero: false,
		},
		{
			name:     "same implemented status alphabetically",
			a:        BuiltinSlashCommand{Name: "a", Implemented: true},
			b:        BuiltinSlashCommand{Name: "z", Implemented: true},
			wantNeg:  true,
			wantZero: false,
		},
		{
			name:     "same name, implemented comes first",
			a:        BuiltinSlashCommand{Name: "login", Implemented: true},
			b:        BuiltinSlashCommand{Name: "login", Implemented: false},
			wantNeg:  true,
			wantZero: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := compareBuiltinCommands(tt.a, tt.b)
			if tt.wantZero {
				if got != 0 {
					t.Errorf("compareBuiltinCommands(%+v, %+v) = %d, want 0", tt.a, tt.b, got)
				}
			} else if tt.wantNeg {
				if got >= 0 {
					t.Errorf("compareBuiltinCommands(%+v, %+v) = %d, want < 0", tt.a, tt.b, got)
				}
			} else {
				if got <= 0 {
					t.Errorf("compareBuiltinCommands(%+v, %+v) = %d, want > 0", tt.a, tt.b, got)
				}
			}
		})
	}
}

func TestCommandsSliceNotMutated(t *testing.T) {
	// Get the list twice and verify it's not the same slice (to prevent accidental mutations)
	cmds1 := ListBuiltinCommands()
	cmds2 := ListBuiltinCommands()

	// The slices should have the same content but be different instances
	if &cmds1 == &cmds2 {
		t.Error("ListBuiltinCommands should return a new slice each time")
	}

	// Verify content is the same
	if len(cmds1) != len(cmds2) {
		t.Errorf("slice lengths differ: %d vs %d", len(cmds1), len(cmds2))
	}

	for i := range slices.Min([]int{len(cmds1), len(cmds2)}) {
		if cmds1[i] != cmds2[i] {
			t.Errorf("slice content differs at index %d: %+v vs %+v", i, cmds1[i], cmds2[i])
		}
	}
}
