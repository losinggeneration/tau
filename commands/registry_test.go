package commands

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubUI implements CommandUI for testing.
type stubUI struct{}

func (stubUI) Confirm(_ context.Context, _, _ string) (bool, error) { return true, nil }
func (stubUI) Select(_ context.Context, _ string, _ []string) (int, error) {
	return 0, nil
}
func (stubUI) Input(_ context.Context, _, _ string) (string, error) { return "test", nil }

// stubSession implements CommandSession for testing.
type stubSession struct {
	id    string
	model string
	cwd   string
}

func (s stubSession) GetLastAssistantText() string { return "" }
func (s stubSession) GetSessionName() string       { return "" }
func (s stubSession) Compact(context.Context, ...string) error {
	return nil
}

func TestInMemoryRegistry_Register(t *testing.T) {
	r := NewInMemoryRegistry()

	handler := func(_ context.Context, _ string, _ CommandUI, _ CommandSession) (Result, error) {
		return Result{Output: "ok"}, nil
	}

	err := r.Register(Info{Name: "review", Description: "review code", Source: Builtin}, handler)
	require.NoError(t, err)

	gotHandler, gotInfo, ok := r.Lookup("review")
	assert.True(t, ok)
	assert.NotNil(t, gotHandler)
	assert.Equal(t, "review", gotInfo.Name)
	assert.Equal(t, Builtin, gotInfo.Source)
}

func TestInMemoryRegistry_RegisterConflict(t *testing.T) {
	r := NewInMemoryRegistry()

	handler := func(_ context.Context, _ string, _ CommandUI, _ CommandSession) (Result, error) {
		return Result{Output: "ok"}, nil
	}

	err := r.Register(Info{Name: "review", Source: Builtin}, handler)
	require.NoError(t, err)

	err = r.Register(Info{Name: "review", Source: Plugin}, handler)
	assert.True(t, errors.Is(err, ErrCommandExists{Name: "review", Source: Builtin}))
}

func TestInMemoryRegistry_LookupNotFound(t *testing.T) {
	r := NewInMemoryRegistry()

	handler, info, ok := r.Lookup("nonexistent")
	assert.False(t, ok)
	assert.Nil(t, handler)
	assert.Equal(t, Info{}, info)
}

func TestInMemoryRegistry_List(t *testing.T) {
	r := NewInMemoryRegistry()

	handler := func(_ context.Context, _ string, _ CommandUI, _ CommandSession) (Result, error) {
		return Result{Output: "ok"}, nil
	}

	require.NoError(t, r.Register(Info{Name: "zebra", Source: Builtin}, handler))
	require.NoError(t, r.Register(Info{Name: "alpha", Source: Builtin}, handler))
	require.NoError(t, r.Register(Info{Name: "middle", Source: Builtin}, handler))

	list := r.List()
	require.Len(t, list, 3)
	assert.Equal(t, "alpha", list[0].Name)
	assert.Equal(t, "middle", list[1].Name)
	assert.Equal(t, "zebra", list[2].Name)
}

func TestInMemoryRegistry_ParseAndExecute(t *testing.T) {
	r := NewInMemoryRegistry()

	handler := func(_ context.Context, args string, _ CommandUI, _ CommandSession) (Result, error) {
		return Result{Output: "echo: " + args}, nil
	}

	require.NoError(t, r.Register(Info{Name: "echo", Source: Builtin}, handler))

	result, err := r.ParseAndExecute(context.Background(), "/echo hello world", stubUI{}, stubSession{})
	require.NoError(t, err)
	assert.Equal(t, "echo: hello world", result.Output)
}

func TestInMemoryRegistry_ParseAndExecuteNoArgs(t *testing.T) {
	r := NewInMemoryRegistry()

	handler := func(_ context.Context, args string, _ CommandUI, _ CommandSession) (Result, error) {
		return Result{Output: "args: '" + args + "'"}, nil
	}

	require.NoError(t, r.Register(Info{Name: "test", Source: Builtin}, handler))

	result, err := r.ParseAndExecute(context.Background(), "/test", stubUI{}, stubSession{})
	require.NoError(t, err)
	assert.Equal(t, "args: ''", result.Output)
}

func TestInMemoryRegistry_ParseAndExecuteNotFound(t *testing.T) {
	r := NewInMemoryRegistry()

	_, err := r.ParseAndExecute(context.Background(), "/nonexistent", stubUI{}, stubSession{})
	assert.True(t, errors.Is(err, ErrCommandNotFound{Name: "nonexistent"}))
}

func TestParseCommand(t *testing.T) {
	tests := []struct {
		input    string
		wantName string
		wantArgs string
	}{
		{"/review HEAD~1", "review", "HEAD~1"},
		{"/test", "test", ""},
		{"/test   ", "test", ""},
		{"/echo  hello   world  ", "echo", "hello   world"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			name, args := parseCommand(tt.input)
			assert.Equal(t, tt.wantName, name)
			assert.Equal(t, tt.wantArgs, args)
		})
	}
}

func TestSource_String(t *testing.T) {
	tests := []struct {
		source Source
		want   string
	}{
		{Builtin, "builtin"},
		{Prompt, "prompt"},
		{Skill, "skill"},
		{Plugin, "plugin"},
		{Source(99), "unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.source.String())
		})
	}
}

func TestInMemoryRegistry_MultipleSources(t *testing.T) {
	r := NewInMemoryRegistry()

	handler := func(_ context.Context, _ string, _ CommandUI, _ CommandSession) (Result, error) {
		return Result{Output: "ok"}, nil
	}

	require.NoError(t, r.Register(Info{Name: "built", Source: Builtin, SourcePath: "builtin.go"}, handler))
	require.NoError(t, r.Register(Info{Name: "sk", Source: Skill, SourcePath: "skill.md"}, handler))
	require.NoError(t, r.Register(Info{Name: "plg", Source: Plugin, SourcePath: "plugin.so"}, handler))
	require.NoError(t, r.Register(Info{Name: "pr", Source: Prompt, SourcePath: "prompt.md"}, handler))

	list := r.List()
	require.Len(t, list, 4)

	sources := map[string]Source{}
	for _, info := range list {
		sources[info.Name] = info.Source
	}
	assert.Equal(t, Builtin, sources["built"])
	assert.Equal(t, Skill, sources["sk"])
	assert.Equal(t, Plugin, sources["plg"])
	assert.Equal(t, Prompt, sources["pr"])
}
