// Package session provides the tau sessions CLI commands.
package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/losinggeneration/tau/agent/compaction"
	"github.com/losinggeneration/tau/ai"
	"github.com/losinggeneration/tau/ai/store"
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
	// GistURL overrides the GitHub Gist API endpoint for tests.
	GistURL string
	// GistStatusCode overrides the expected HTTP status for gist creation.
	// Defaults to http.StatusCreated (201).
	GistStatusCode int
}

// NewCLI creates a CLI backed by the default session storage and credential store.
// If sessionsDir is empty, the default location (~/.tau/agent/sessions) is used.
func NewCLI(sessionsDir string) (*CLI, error) {
	cfgDir := configDir()
	if sessionsDir == "" {
		sessionsDir = filepath.Join(cfgDir, "sessions")
	}
	storage, err := NewStorageAt(sessionsDir)
	if err != nil {
		return nil, fmt.Errorf("session storage: %w", err)
	}

	authPath := filepath.Join(cfgDir, "auth.json")
	fb := store.NewFileBackend(authPath, store.Options{})
	if err := fb.EnsureDir(); err != nil {
		return nil, fmt.Errorf("ensure auth dir: %w", err)
	}

	userSettings, _ := ai.LoadSettings(filepath.Join(cfgDir, "settings.json"))

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
	if dir := os.Getenv("TAU_CONFIG_DIR"); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".tau"
	}
	return filepath.Join(home, ".tau", "agent")
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
	return c.Storage.Resume(id)
}

// Rename changes the display name of a session.
func (c *CLI) Rename(ctx context.Context, id, newName string) error {
	return c.Storage.Rename(id, newName)
}

// MoveCWD rewrites a session's working directory and moves it into that CWD's scoped directory.
func (c *CLI) MoveCWD(ctx context.Context, id, newCWD string) (string, error) {
	return c.Storage.MoveCWD(id, newCWD)
}

// Delete moves a session to the trash directory.
func (c *CLI) Delete(ctx context.Context, id string) error {
	return c.Storage.Delete(id)
}

// CleanupEmpty moves sessions without messages to the trash directory.
func (c *CLI) CleanupEmpty(ctx context.Context, opts CleanupEmptyOptions) (CleanupEmptyResult, error) {
	return c.Storage.CleanupEmpty(ctx, opts)
}

// Restore moves a session from trash back to the sessions directory.
func (c *CLI) Restore(ctx context.Context, id string) error {
	return c.Storage.Restore(id)
}

// Compact triggers manual compaction for a session using model-driven summarization.
func (c *CLI) Compact(ctx context.Context, id string) error {
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

	// Build transcript from entries
	var transcript strings.Builder
	for _, e := range entries {
		if e.Type == EntryTypeMessage && e.Message != nil {
			var msg map[string]any
			if err := json.Unmarshal(e.Message, &msg); err == nil {
				role, _ := msg["role"].(string)
				content := formatContent(msg["content"])
				fmt.Fprintf(&transcript, "[%s]: %s\n", role, content)
			}
		}
	}

	transcriptStr := transcript.String()
	var summary string

	// Try model-driven summarization if model is available
	if c.model != nil {
		summary, err = c.callModelForSummary(ctx, transcriptStr)
		if err != nil {
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

	return c.Storage.CompactWithHook(id, summary, firstKeptID, tokensBefore, tokensAfter, false)
}

// callModelForSummary calls the configured model to generate a summary.
func (c *CLI) callModelForSummary(ctx context.Context, transcript string) (string, error) {
	if c.model == nil {
		return "", errors.New("no model configured")
	}

	// Get AI client for the model
	client, err := c.newClient()
	if err != nil {
		return "", fmt.Errorf("create AI client: %w", err)
	}

	// Build the summarization prompt using the compaction template
	systemPrompt := compaction.BuildSummarizationPrompt(transcript)

	prompt := &ai.Context{
		SystemPrompt: systemPrompt,
		Messages: []ai.Message{
			{Role: "user", Content: "Generate a concise summary of the conversation transcript following the instructions above."},
		},
	}

	msg, err := client.Complete(ctx, c.model, prompt)
	if err != nil {
		return "", fmt.Errorf("model completion: %w", err)
	}

	// Extract text content from response
	for _, block := range msg.Content {
		if block.Type == ai.ContentBlockText && block.Text != "" {
			return block.Text, nil
		}
	}

	return "", errors.New("no text content in summarization response")
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
	for _, e := range r.Entries() {
		if e.ID == branchTip || strings.HasPrefix(e.ID, branchTip) {
			tipEntry = &e
			break
		}
	}
	if tipEntry == nil {
		return fmt.Errorf("branch tip %q not found in session", branchTip)
	}

	// Walk back through parent IDs to collect all entries in the branch
	var branchEntries []Entry
	current := tipEntry
	visited := make(map[string]bool)
	for current != nil && !visited[current.ID] {
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

	// Build transcript from branch entries
	var transcript strings.Builder
	for _, e := range branchEntries {
		if e.Type == EntryTypeMessage && e.Message != nil {
			var msg map[string]any
			if err := json.Unmarshal(e.Message, &msg); err == nil {
				role, _ := msg["role"].(string)
				content := formatContent(msg["content"])
				fmt.Fprintf(&transcript, "[%s]: %s\n", role, content)
			}
		}
	}

	transcriptStr := transcript.String()
	var summary string

	// Try model-driven summarization if model is available
	if c.model != nil {
		summary, err = c.callModelForBranchSummary(ctx, transcriptStr)
		if err != nil {
			// Fallback to naive summarization on error
			summary = buildBranchNaiveSummary(branchEntries)
		}
	} else {
		// No model available, use naive summarization
		summary = buildBranchNaiveSummary(branchEntries)
	}

	return c.Storage.SummarizeBranch(id, branchTip, summary)
}

// callModelForBranchSummary calls the configured model to generate a branch summary.
func (c *CLI) callModelForBranchSummary(ctx context.Context, transcript string) (string, error) {
	if c.model == nil {
		return "", errors.New("no model configured")
	}

	// Get AI client for the model
	client, err := c.newClient()
	if err != nil {
		return "", fmt.Errorf("create AI client: %w", err)
	}

	// Build the summarization prompt
	systemPrompt := compaction.BuildSummarizationPrompt(transcript)

	prompt := &ai.Context{
		SystemPrompt: systemPrompt,
		Messages: []ai.Message{
			{Role: "user", Content: "Generate a concise summary of this branch's conversation. Focus on the key changes and decisions made in this branch compared to the parent."},
		},
	}

	msg, err := client.Complete(ctx, c.model, prompt)
	if err != nil {
		return "", fmt.Errorf("model completion: %w", err)
	}

	// Extract text content from response
	for _, block := range msg.Content {
		if block.Type == ai.ContentBlockText && block.Text != "" {
			return block.Text, nil
		}
	}

	return "", errors.New("no text content in summarization response")
}

func (c *CLI) newClient() (ai.Client, error) {
	if c.model == nil {
		return nil, errors.New("no model configured")
	}

	p, ok := ai.Get(c.model.API)
	if !ok {
		return nil, fmt.Errorf("no provider for API: %s", c.model.API)
	}

	client, err := p.NewClient(c.model)
	if err != nil {
		return nil, err
	}

	if c.Store == nil || c.modelSpec.Provider == "" {
		return client, nil
	}

	cred, err := c.Store.Get(context.Background(), c.modelSpec.Provider)
	if err != nil {
		return nil, fmt.Errorf("get credentials for %s: %w", c.modelSpec.Provider, err)
	}

	apiKey, err := store.APIKeyFromCredential(c.modelSpec.Provider, cred)
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
				apiKey, err := store.APIKeyFromCredential(prov, updatedCred)
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

// buildBranchNaiveSummary creates a fallback summary from branch entries.
func buildBranchNaiveSummary(entries []Entry) string {
	var summary strings.Builder
	summary.WriteString("Branch summary:\n")

	count := 0
	for _, e := range entries {
		if e.Type == EntryTypeMessage && e.Message != nil {
			var msg map[string]any
			if err := json.Unmarshal(e.Message, &msg); err == nil {
				role, _ := msg["role"].(string)
				content := formatContent(msg["content"])
				if len(content) > 150 {
					content = content[:150] + "..."
				}
				fmt.Fprintf(&summary, "- [%s]: %s\n", role, content)
				count++
				if count >= 10 {
					break
				}
			}
		}
	}

	return summary.String()
}

// Fork creates a new session forked from an existing one.
func (c *CLI) Fork(ctx context.Context, id string, atEntry *string) (string, error) {
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

// Share posts a session to GitHub Gist and returns the URL.
// If TAU_SHARE_VIEWER_URL is set, the returned URL will use that as the base
// for viewing the shared session instead of the default https://pi.dev/session/.
func (c *CLI) Share(ctx context.Context, id string, isPublic bool) (url string, err error) {
	// Get GitHub credential
	cred, err := c.Store.Get(ctx, "github")
	if err != nil {
		return "", fmt.Errorf("run `tau login github` first: no credential found for github")
	}

	var apiKey string
	switch cred.Type {
	case ai.CredentialTypeAPIKey:
		apiKey = cred.APIKey
	case ai.CredentialTypeOAuth:
		// Get access token from OAuth credential
		apiKey = cred.AccessToken
	default:
		return "", errors.New("github credential is not an API key or OAuth token")
	}

	if apiKey == "" {
		return "", errors.New("github credential has no API key")
	}

	// Export to HTML for the gist
	data, err := c.Storage.ExportHTML(id)
	if err != nil {
		return "", fmt.Errorf("export session: %w", err)
	}

	// Post to GitHub Gist API
	reqBody := map[string]any{
		"description": fmt.Sprintf("Tau session %s", id),
		"public":      isPublic,
		"files": map[string]map[string]string{
			"session.html": {
				"content": string(data),
			},
		},
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("marshal gist request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", c.gistAPIURL(), bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("post gist: %w", err)
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil && err == nil {
			err = fmt.Errorf("close gist response body: %w", closeErr)
		}
	}()

	// GistStatusCode allows tests to use a non-201 status code.
	wantCode := http.StatusCreated
	if c.GistStatusCode != 0 {
		wantCode = c.GistStatusCode
	}
	if resp.StatusCode != wantCode {
		respBody, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("gist creation failed (status %d): %s", resp.StatusCode, string(respBody))
	}

	var gist struct {
		ID      string `json:"id"`
		HTMLURL string `json:"html_url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&gist); err != nil {
		return "", fmt.Errorf("parse gist response: %w", err)
	}

	// If TAU_SHARE_VIEWER_URL is set, return it with the gist ID appended
	// This allows using a custom viewer URL instead of the default pi.dev/session/
	if viewerURL := ai.ShareViewerURL(); viewerURL != "" {
		if gist.ID == "" {
			return "", errors.New("gist response missing id")
		}
		// Ensure viewer URL ends with / for clean concatenation
		viewerURL = strings.TrimSuffix(viewerURL, "/")
		return viewerURL + "/" + gist.ID, nil
	}

	return gist.HTMLURL, nil
}

// gistAPIURL returns the Gist API base URL. If CLI.GistURL is set it is used
// as-is (for tests). Otherwise defaults to the GitHub Gist API endpoint.
func (c *CLI) gistAPIURL() string {
	if c.GistURL != "" {
		return c.GistURL
	}
	return "https://api.github.com/gists"
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
