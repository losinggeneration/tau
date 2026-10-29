// Package session provides the tau sessions CLI commands.
package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/losinggeneration/tau/agent/compaction"
	"github.com/losinggeneration/tau/ai"
	"github.com/losinggeneration/tau/ai/store"
	"github.com/losinggeneration/tau/paths"
)

// CLI provides the `tau sessions` subcommand implementation.
// It wraps the session storage layer with the CLI surface defined in the
// session-management spec.
type CLI struct {
	Storage   *Storage
	Store     *store.FileBackend
	Settings  *ai.Settings
	Resolver  *ai.ModelResolver
	model     *ai.Model
	modelSpec ai.ModelSpec
}

// NewCLI creates a CLI backed by the default session storage and credential store.
// If sessionsDir is empty, it resolves TAU_SESSION_DIR, then sessionDir from
// settings.json, then <configDir>/sessions.
func NewCLI(sessionDir string) (*CLI, error) {
	cfgDir := configDir()
	userSettings, _ := ai.LoadSettings(filepath.Join(cfgDir, "settings.json"))
	if sessionDir == "" {
		var configured string
		if userSettings != nil {
			configured = userSettings.SessionDir
		}
		sessionDir = paths.Dir("TAU_SESSION_DIR", configured, filepath.Join(cfgDir, "sessions"))
	}
	storage, err := NewStorageAt(sessionDir)
	if err != nil {
		return nil, fmt.Errorf("session storage: %w", err)
	}

	authPath := filepath.Join(cfgDir, "auth.json")
	fb := store.NewFileBackend(authPath, store.Options{})
	if err := fb.EnsureDir(); err != nil {
		return nil, fmt.Errorf("ensure auth dir: %w", err)
	}

	cli := &CLI{
		Storage:  storage,
		Store:    fb,
		Settings: userSettings,
	}

	// Set up model resolver for model-driven compaction
	if userSettings != nil {
		cli.Resolver = ai.NewModelResolver(userSettings)
		if spec := cli.Resolver.ResolveDefaultModel(); spec.Provider != "" && spec.Model != "" {
			model, err := ai.ResolveModelID(spec.Provider + "/" + spec.Model)
			if err == nil {
				cli.model = model
				cli.modelSpec = spec
			}
		}
	}

	return cli, nil
}

// configDir returns the tau config directory, defaulting to ~/.tau/agent.
func configDir() string {
	return paths.ConfigDir()
}

// ListOptions controls what sessions List returns.
type ListOptions struct {
	Limit int
	Since string
	CWD   string
}

// List returns summaries of sessions matching the options.
func (c *CLI) List(ctx context.Context, opts ListOptions) ([]SessionSummary, error) {
	summaries, err := c.Storage.List()
	if opts.CWD != "" {
		summaries, err = c.Storage.ListCWD(opts.CWD)
	}
	if err != nil {
		return nil, err
	}

	var result []SessionSummary
	for _, s := range summaries {
		if opts.Since != "" {
			since, err := time.Parse("2006-01-02", opts.Since)
			if err == nil {
				created, err := time.Parse("2006-01-02", s.Created[:10])
				if err == nil && created.Before(since) {
					continue
				}
			}
		}
		if opts.CWD != "" && s.CWD != opts.CWD {
			continue
		}
		result = append(result, s)
		if opts.Limit > 0 && len(result) >= opts.Limit {
			break
		}
	}

	return result, nil
}

// Show prints a session to w in human-readable (ANSI) format.
func (c *CLI) Show(ctx context.Context, id string, w io.Writer) error {
	r, err := c.Storage.Open(id)
	if err != nil {
		return err
	}
	out := &checkedWriter{w: w}

	out.Printf("Session: %s\n", r.DisplayName())
	out.Printf("ID:      %s\n", r.Header().ID)
	out.Printf("Created: %s\n", r.Header().Timestamp)
	out.Printf("CWD:     %s\n", r.Header().CWD)
	if r.Header().ParentSession != nil {
		out.Printf("Parent:  %s\n", *r.Header().ParentSession)
	}
	out.Printf("Tokens:  in=%d out=%d\n", r.TotalTokens().Input, r.TotalTokens().Output)
	out.Printf("Messages: %d\n", r.MessageCount())
	out.Println()
	out.Println("--- Entries ---")

	for _, e := range r.Entries() {
		ts := FormatTime(e.Timestamp)
		switch e.Type {
		case EntryTypeMessage:
			if e.Message == nil {
				continue
			}
			var msg map[string]any
			if err := json.Unmarshal(e.Message, &msg); err != nil {
				continue
			}
			role, _ := msg["role"].(string)
			content := formatContent(msg["content"])
			out.Printf("[%s] %s (%s)\n", ts, role, e.ID[:8])
			out.Printf("%s\n", content)
			if e.Tokens != nil {
				out.Printf("  tokens: in=%d out=%d\n", e.Tokens.Input, e.Tokens.Output)
			}
		case EntryTypePositionChange:
			if e.NewPosition != nil {
				out.Printf("[%s] position -> %s\n", ts, (*e.NewPosition)[:8])
			}
		case EntryTypeCompaction:
			if e.Summary != nil {
				out.Printf("[%s] compaction (hook=%v)\n", ts, e.FromHook)
				if e.Fallback {
					out.Printf("  fallback: local summary (model call failed)\n")
					if e.FallbackError != "" {
						out.Printf("  error: %s\n", e.FallbackError)
					}
				}
				out.Printf("  kept from: %s\n", func() string {
					if e.FirstKeptID != nil {
						return (*e.FirstKeptID)[:8]
					}
					return "?"
				}())
				out.Printf("  before=%d after=%d\n", derefInt(e.TokensBefore), derefInt(e.TokensAfter))
				out.Printf("  summary: %s\n", *e.Summary)
			}
		case EntryTypeBranchSummary:
			if e.Summary != nil {
				out.Printf("[%s] branch_summary from=%s\n", ts, func() string {
					if e.FromID != nil {
						return (*e.FromID)[:8]
					}
					return "?"
				}())
				out.Printf("  %s\n", *e.Summary)
			}
		case EntryTypeSessionInfo:
			if e.Name != nil {
				out.Printf("[%s] renamed to %q\n", ts, *e.Name)
			}
		case EntryTypeModelChange:
			if e.Provider != nil {
				out.Printf("[%s] model_change: %s/%s\n", ts, *e.Provider, derefStr(e.Model))
			}
		case EntryTypeCustom:
			ct := "unknown"
			if e.CustomType != nil {
				ct = *e.CustomType
			}
			out.Printf("[%s] custom [%s]\n", ts, ct)
		case EntryTypeCustomMessage:
			ct := "unknown"
			if e.CustomType != nil {
				ct = *e.CustomType
			}
			if e.Message != nil {
				var msg map[string]any
				_ = json.Unmarshal(e.Message, &msg)
				content := formatContent(msg["content"])
				out.Printf("[%s] custom_message [%s]: %s\n", ts, ct, content)
			}
		default:
			out.Printf("[%s] %s\n", ts, e.Type)
		}
	}

	if out.err != nil {
		return fmt.Errorf("write session: %w", out.err)
	}
	return nil
}

type checkedWriter struct {
	w   io.Writer
	err error
}

func (w *checkedWriter) Printf(format string, args ...any) {
	if w.err != nil {
		return
	}
	_, w.err = fmt.Fprintf(w.w, format, args...)
}

func (w *checkedWriter) Println(args ...any) {
	if w.err != nil {
		return
	}
	_, w.err = fmt.Fprintln(w.w, args...)
}

// Resume opens an existing session for appending, claiming ownership.
// The caller is responsible for releasing ownership when done.
func (c *CLI) Resume(ctx context.Context, id string) (*Writer, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("resume session: %w", err)
	}
	return c.Storage.Resume(id)
}

// Rename changes the display name of a session.
func (c *CLI) Rename(ctx context.Context, id, newName string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("rename session: %w", err)
	}
	return c.Storage.Rename(id, newName)
}

// MoveCWD rewrites a session's working directory and moves it into that CWD's scoped directory.
func (c *CLI) MoveCWD(ctx context.Context, id, newCWD string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("move cwd for session: %w", err)
	}
	return c.Storage.MoveCWD(id, newCWD)
}

// Delete moves a session to the trash directory.
func (c *CLI) Delete(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	return c.Storage.Delete(id)
}

// CleanupEmpty moves sessions without messages to the trash directory.
func (c *CLI) CleanupEmpty(ctx context.Context, opts CleanupEmptyOptions) (CleanupEmptyResult, error) {
	return c.Storage.CleanupEmpty(ctx, opts)
}

// Restore moves a session from trash back to the sessions directory.
func (c *CLI) Restore(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("restore session: %w", err)
	}
	return c.Storage.Restore(id)
}

// Compact triggers manual compaction for a session using model-driven summarization.
func (c *CLI) Compact(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("compact: %w", err)
	}
	// Get current context for compaction trigger
	r, err := c.Storage.Open(id)
	if err != nil {
		return err
	}

	// Use the session's context builder to get the transcript
	entries := r.ContextEntries()
	if len(entries) == 0 {
		return errors.New("session has no entries to compact")
	}

	transcriptStr := summaryTranscript(entries)
	var summary string

	// Try model-driven summarization if model is available
	if c.model != nil {
		var usage ai.Usage
		summary, usage, err = c.completeSummary(ctx, transcriptStr)
		if usageErr := c.recordSummaryUsage(id, usage); usageErr != nil {
			return errors.Join(err, usageErr)
		}
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return err
			}
			// Fallback to naive summarization on error
			summary = buildNaiveSummary(transcriptStr)
		}
	} else {
		// No model available, use naive summarization
		summary = buildNaiveSummary(transcriptStr)
	}

	// Find the first entry to keep (preserving last N entries)
	firstKeptID := ""
	if len(entries) > 10 {
		firstKeptID = entries[len(entries)-10].ID
	} else if len(entries) > 0 {
		firstKeptID = entries[0].ID
	}

	tokensBefore := r.TotalTokens().Input
	tokensAfter := tokensBefore / 2 // rough estimate

	if err := ctx.Err(); err != nil {
		return fmt.Errorf("compact: %w", err)
	}
	return c.Storage.CompactWithHook(id, summary, firstKeptID, tokensBefore, tokensAfter, false)
}

// completeSummary requests a summary and returns the billed usage even when
// the response is rejected.
func (c *CLI) completeSummary(ctx context.Context, content string) (string, ai.Usage, error) {
	if c.model == nil {
		return "", ai.Usage{}, errors.New("no model configured")
	}
	client, err := c.newClient(ctx)
	if err != nil {
		return "", ai.Usage{}, fmt.Errorf("create AI client: %w", err)
	}
	prompt := &ai.Context{
		SystemPrompt: compaction.BuildSummarizationPrompt(),
		Messages:     []ai.Message{{Role: "user", Content: content}},
	}
	msg, err := client.Complete(ctx, c.model, prompt)
	var usage ai.Usage
	if msg != nil {
		usage = msg.Usage
	}
	if err != nil {
		return "", usage, fmt.Errorf("model completion: %w", err)
	}
	text, err := summaryResponseText(msg)
	return text, usage, err
}

// recordSummaryUsage persists billed usage so session totals include it.
func (c *CLI) recordSummaryUsage(id string, usage ai.Usage) error {
	usage = NormalizeUsage(usage)
	if usage.TotalTokens == 0 && usage.Cost.Total == 0 {
		return nil
	}
	return c.Storage.AppendSummaryUsage(id, usage)
}

// buildNaiveSummary creates a fallback summary from transcript text.
func buildNaiveSummary(transcript string) string {
	lines := strings.Split(transcript, "\n")
	var summary strings.Builder
	summary.WriteString("Session summary:\n")

	count := 0
	for _, line := range lines {
		if line != "" && count < 20 {
			summary.WriteString(line + "\n")
			count++
		}
		if count >= 20 {
			break
		}
	}

	return summary.String()
}

// SummarizeBranch walks from branchTip back to divergence and persists a branch_summary entry.
func (c *CLI) SummarizeBranch(ctx context.Context, id, branchTip string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("summarize branch: %w", err)
	}
	if branchTip == "" {
		return errors.New("branch tip must not be empty")
	}

	r, err := c.Storage.Open(id)
	if err != nil {
		return err
	}

	// Build a map of entries by ID for parent traversal
	entriesByID := make(map[string]Entry)
	for _, e := range r.Entries() {
		entriesByID[e.ID] = e
	}

	// Find the branch tip entry
	var tipEntry *Entry
	if entry, ok := entriesByID[branchTip]; ok {
		tipEntry = &entry
	} else {
		for _, entry := range r.Entries() {
			if strings.HasPrefix(entry.ID, branchTip) {
				if tipEntry != nil {
					return fmt.Errorf("ambiguous branch tip %q matches multiple entries", branchTip)
				}
				tipEntry = &entry
			}
		}
	}
	if tipEntry == nil {
		return fmt.Errorf("branch tip %q not found in session", branchTip)
	}

	selected := make(map[string]bool)
	for _, entryID := range r.ActiveBranch() {
		selected[entryID] = true
	}

	// Stop at the common ancestor so shared history is not summarized twice.
	var branchEntries []Entry
	current := tipEntry
	visited := make(map[string]bool)
	for current != nil && !selected[current.ID] && !visited[current.ID] {
		visited[current.ID] = true
		branchEntries = append([]Entry{*current}, branchEntries...)
		if current.ParentID != nil {
			if parent, ok := entriesByID[*current.ParentID]; ok {
				current = &parent
			} else {
				current = nil
			}
		} else {
			current = nil
		}
	}

	if len(branchEntries) == 0 {
		return nil
	}

	transcriptStr := summaryTranscript(branchEntries)
	var summary string

	// Try model-driven summarization if model is available
	if strings.TrimSpace(transcriptStr) == "" {
		summary = "No content to summarize"
	} else if c.model != nil {
		var usage ai.Usage
		summary, usage, err = c.callModelForBranchSummary(ctx, transcriptStr)
		if usageErr := c.recordSummaryUsage(id, usage); usageErr != nil {
			return errors.Join(err, usageErr)
		}
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return err
			}
			// Fallback to naive summarization on error
			summary = buildBranchNaiveSummary(branchEntries)
		}
	} else {
		// No model available, use naive summarization
		summary = buildBranchNaiveSummary(branchEntries)
	}

	if err := ctx.Err(); err != nil {
		return fmt.Errorf("summarize branch: %w", err)
	}
	return c.Storage.SummarizeBranch(id, tipEntry.ID, summary)
}

// callModelForBranchSummary calls the configured model to generate a branch summary.
func (c *CLI) callModelForBranchSummary(ctx context.Context, transcript string) (string, ai.Usage, error) {
	return c.completeSummary(ctx, "Summarize this branch's conversation. Focus on the key changes and decisions made in this branch compared to the parent.\n\n"+transcript)
}

func summaryResponseText(msg *ai.AssistantMessage) (string, error) {
	var text strings.Builder
	if msg != nil {
		switch msg.StopReason {
		case ai.StopReasonAborted:
			return "", fmt.Errorf("summarization aborted: %s: %w", msg.ErrorMessage, context.Canceled)
		case ai.StopReasonError:
			return "", fmt.Errorf("summarization failed: %s", msg.ErrorMessage)
		case ai.StopReasonLength:
			return "", errors.New("summarization stopped at the output token limit; summary is incomplete")
		}
		for _, block := range msg.Content {
			if block.Type == ai.ContentBlockToolCall {
				return "", errors.New("summarization attempted to call a tool")
			}
			if block.Type == ai.ContentBlockText {
				text.WriteString(block.Text)
			}
		}
	}
	if strings.TrimSpace(text.String()) == "" {
		return "", errors.New("no text content in summarization response")
	}
	return text.String(), nil
}

func (c *CLI) newClient(ctx context.Context) (ai.Client, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("create client: %w", err)
	}
	if c.model == nil {
		return nil, errors.New("no model configured")
	}

	client, err := ai.GetClient(c.model)
	if err != nil {
		return nil, err
	}

	if c.Store == nil || c.modelSpec.Provider == "" {
		return client, nil
	}

	cred, err := c.Store.Get(ctx, c.modelSpec.Provider)
	if err != nil {
		return nil, fmt.Errorf("get credentials for %s: %w", c.modelSpec.Provider, err)
	}

	apiKey, err := store.APIKeyFromCredential(ctx, c.modelSpec.Provider, cred)
	if err != nil {
		return nil, err
	}

	if apiKey != "" {
		if setter, ok := client.(interface{ SetAPIKey(string) }); ok {
			setter.SetAPIKey(apiKey)
		}
	}

	if refreshable, ok := client.(interface {
		SetTransportOption(opts ai.TransportOption)
	}); ok {
		refreshable.SetTransportOption(ai.TransportOption{
			RefreshCreds: func(ctx context.Context, prov string) (string, error) {
				updatedCred, err := c.Store.Get(ctx, prov)
				if err != nil {
					return "", fmt.Errorf("refresh credentials: %w", err)
				}
				if updatedCred.NeedsReauth {
					return "", ai.ErrCredentialNeedsReauth{Provider: prov}
				}
				apiKey, err := store.APIKeyFromCredential(ctx, prov, updatedCred)
				if err != nil {
					return "", err
				}
				if apiKey == "" {
					return "", fmt.Errorf("no API key in refreshed credential")
				}
				return apiKey, nil
			},
			Provider: c.modelSpec.Provider,
		})
	}

	return client, nil
}

func summaryTranscript(entries []Entry) string {
	var transcript strings.Builder
	for _, entry := range entries {
		message := entry.ToAgentMessage()
		if message == nil || message.ExcludeFromContext {
			continue
		}
		for _, block := range message.Content {
			switch block.Type {
			case ai.ContentBlockText:
				if block.Text != "" {
					fmt.Fprintf(&transcript, "[%s]: %s\n", message.Role, block.Text)
				}
			case ai.ContentBlockToolCall:
				if block.Name != "" {
					fmt.Fprintf(&transcript, "[%s tool call]: %s(%s)\n", message.Role, block.Name, block.Arguments)
				}
			}
		}
	}
	return transcript.String()
}

// buildBranchNaiveSummary creates a fallback summary from branch entries.
func buildBranchNaiveSummary(entries []Entry) string {
	var summary strings.Builder
	summary.WriteString("Branch summary:\n")

	count := 0
	for _, e := range entries {
		content := summaryTranscript([]Entry{e})
		if content == "" {
			continue
		}
		if len(content) > 150 {
			content = content[:150] + "..."
		}
		summary.WriteString("- " + strings.TrimSuffix(content, "\n") + "\n")
		count++
		if count >= 10 {
			break
		}
	}

	return summary.String()
}

// Fork creates a new session forked from an existing one.
func (c *CLI) Fork(ctx context.Context, id string, atEntry *string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("fork session: %w", err)
	}
	w, err := c.Storage.Fork(id, atEntry)
	if err != nil {
		return "", err
	}
	if err := w.Close(); err != nil {
		return "", fmt.Errorf("close fork writer: %w", err)
	}
	// Get the new session header from the writer's file path
	h, err := ReadHeader(w.Path())
	if err != nil {
		return "", err
	}
	return h.ID, nil
}

// ExportOptions controls export format and output.
type ExportOptions struct {
	Format     string // "html", "json", or "md"
	Output     string // file path; empty = stdout
	NoCollapse bool   // Expand collapsible sections in HTML export
}

// Export writes a session in the requested format.
func (c *CLI) Export(ctx context.Context, id string, opts ExportOptions) error {
	var data []byte
	var err error

	switch opts.Format {
	case "json":
		data, err = c.Storage.ExportJSON(id)
	case "html":
		data, err = c.Storage.ExportHTMLOptions(id, Options{NoCollapse: opts.NoCollapse})
	case "md":
		data, err = c.Storage.ExportMarkdown(id)
	default:
		return fmt.Errorf("unknown format %q (use html, json, or md)", opts.Format)
	}
	if err != nil {
		return err
	}

	if opts.Output != "" {
		return os.WriteFile(opts.Output, data, 0o644)
	}

	_, err = os.Stdout.Write(data)
	return err
}

// Helpers

func formatContent(content any) string {
	switch c := content.(type) {
	case string:
		return c
	case []any:
		var parts []string
		for _, item := range c {
			if m, ok := item.(map[string]any); ok {
				if text, ok := m["text"].(string); ok {
					parts = append(parts, text)
				}
			}
		}
		return strings.Join(parts, "\n")
	default:
		return fmt.Sprintf("%v", content)
	}
}

func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func derefInt(i *int) int {
	if i == nil {
		return 0
	}
	return *i
}
