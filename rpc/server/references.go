package server

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/losinggeneration/tau/ai"
	"github.com/losinggeneration/tau/internal/atref"
	"github.com/losinggeneration/tau/sdk"
	"github.com/losinggeneration/tau/tui/autocomplete"
)

// completePathTimeout bounds the recursive file walk so a huge tree can't
// stall the worker's read lane.
const completePathTimeout = 2 * time.Second

type completePathItem struct {
	Value       string `json:"value"`
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

type completePathResult struct {
	Prefix string             `json:"prefix"`
	Items  []completePathItem `json:"items"`
}

// expandReferences resolves @path tokens against the session's working
// directory and imports the files they name. It runs in the process that owns
// the session, so paths always mean files on the instance's machine.
func expandReferences(sess sdk.Session, text string) (string, []string, error) {
	if !strings.Contains(text, "@") {
		return text, nil, nil
	}
	mgr, err := attachmentManager(sess)
	if err != nil {
		return "", nil, err
	}
	root := cmp.Or(sess.State().CWD(), ".")
	resolve := func(path string) string { return atref.Resolve(path, root) }
	expanded, records, err := atref.Attach(text, resolve, mgr.ImportAttachmentFile)
	if err != nil {
		return "", nil, err
	}
	ids := make([]string, 0, len(records))
	for _, record := range records {
		ids = append(ids, record.ID)
	}
	return expanded, ids, nil
}

// messageContent builds a user message's text, images, and attachments,
// expanding @path tokens first when asked.
func messageContent(sess sdk.Session, text string, expandRefs bool, images, attachFiles, attachmentIDs []string) (string, []ai.ContentBlock, []string, error) {
	var refIDs []string
	if expandRefs {
		var err error
		if text, refIDs, err = expandReferences(sess, text); err != nil {
			return "", nil, nil, err
		}
	}
	content, ids, err := messageContentBlocks(sess, images, attachFiles, attachmentIDs)
	if err != nil {
		return "", nil, nil, err
	}
	return text, content, append(ids, refIDs...), nil
}

// handleCompletePath suggests files for the @path token before the cursor.
// cursor is a byte offset into text and defaults to the end of it.
func (s *Server) handleCompletePath(ctx context.Context, cmd *Command) error {
	var args struct {
		Text   string `json:"text"`
		Cursor *int   `json:"cursor,omitempty"`
	}
	if err := json.Unmarshal(cmd.Args, &args); err != nil {
		return s.sendErrorResult(cmd.ID, err)
	}
	cursor := len(args.Text)
	if args.Cursor != nil {
		cursor = min(max(*args.Cursor, 0), len(args.Text))
	}

	s.sessionMu.Lock()
	current := s.session
	s.sessionMu.Unlock()
	var root string
	if current != nil {
		root = current.State().CWD()
	}
	root = cmp.Or(root, s.projectCWD, ".")

	provider, err := autocomplete.NewCombinedProvider(autocomplete.WithBasePath(root))
	if err != nil {
		return s.sendErrorResult(cmd.ID, err)
	}
	ctx, cancel := context.WithTimeout(ctx, completePathTimeout)
	defer cancel()
	before := args.Text[:cursor]
	lineStart := strings.LastIndexByte(before, '\n') + 1
	result := completePathResult{Items: []completePathItem{}}
	suggestions, err := provider.GetSuggestions(ctx, strings.Split(args.Text, "\n"), strings.Count(before, "\n"), cursor-lineStart, false)
	if err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return s.sendErrorResult(cmd.ID, err)
	}
	if suggestions != nil && strings.HasPrefix(suggestions.Prefix, "@") {
		result.Prefix = suggestions.Prefix
		for _, item := range suggestions.Items {
			result.Items = append(result.Items, completePathItem{Value: item.Value, Label: item.Label, Description: item.Description})
		}
	}
	return s.sendResult(cmd.ID, result)
}
