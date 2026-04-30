package server

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/losinggeneration/tau/ai"
	"github.com/losinggeneration/tau/sdk"
)

func (s *Server) messageContentBlocks(images, attachFiles, attachmentIDs []string) ([]ai.ContentBlock, []string, error) {
	content := make([]ai.ContentBlock, 0, len(images))
	for _, img := range images {
		content = append(content, ai.ContentBlock{Type: ai.ContentBlockImage, Data: img, MimeType: "image/png"})
	}

	ids := append([]string(nil), attachmentIDs...)
	if len(attachFiles) == 0 {
		return content, ids, nil
	}
	mgr, err := s.attachmentManager()
	if err != nil {
		return nil, nil, err
	}
	for _, path := range attachFiles {
		record, err := mgr.ImportAttachmentFile(path)
		if err != nil {
			return nil, nil, err
		}
		ids = append(ids, record.ID)
	}
	return content, ids, nil
}

func (s *Server) attachmentManager() (sdk.AttachmentManager, error) {
	s.sessionMu.Lock()
	current := s.session
	s.sessionMu.Unlock()
	if current == nil {
		return nil, fmt.Errorf("no active session")
	}
	mgr, ok := current.(sdk.AttachmentManager)
	if !ok {
		return nil, fmt.Errorf("attachment store is not configured")
	}
	return mgr, nil
}

func (s *Server) handleAttachFile(ctx context.Context, cmd *Command) error {
	_ = ctx
	var args struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(cmd.Args, &args); err != nil {
		return s.sendErrorResult(cmd.ID, err)
	}
	mgr, err := s.attachmentManager()
	if err != nil {
		return s.sendErrorResult(cmd.ID, err)
	}
	record, err := mgr.ImportAttachmentFile(args.Path)
	if err != nil {
		return s.sendErrorResult(cmd.ID, err)
	}
	return s.sendResult(cmd.ID, map[string]any{"attachment": record})
}

func (s *Server) handleAttachData(ctx context.Context, cmd *Command) error {
	_ = ctx
	var args struct {
		Filename string `json:"filename"`
		MimeType string `json:"mime_type"`
		Data     string `json:"data"`
	}
	if err := json.Unmarshal(cmd.Args, &args); err != nil {
		return s.sendErrorResult(cmd.ID, err)
	}
	mgr, err := s.attachmentManager()
	if err != nil {
		return s.sendErrorResult(cmd.ID, err)
	}
	record, err := mgr.ImportAttachmentData(args.Filename, args.MimeType, args.Data)
	if err != nil {
		return s.sendErrorResult(cmd.ID, err)
	}
	return s.sendResult(cmd.ID, map[string]any{"attachment": record})
}

func (s *Server) handleListAttachments(ctx context.Context, cmd *Command) error {
	_ = ctx
	mgr, err := s.attachmentManager()
	if err != nil {
		return s.sendErrorResult(cmd.ID, err)
	}
	return s.sendResult(cmd.ID, map[string]any{"attachments": mgr.ListAttachments()})
}

func (s *Server) handleDeleteAttachment(ctx context.Context, cmd *Command) error {
	_ = ctx
	var args struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(cmd.Args, &args); err != nil {
		return s.sendErrorResult(cmd.ID, err)
	}
	mgr, err := s.attachmentManager()
	if err != nil {
		return s.sendErrorResult(cmd.ID, err)
	}
	if err := mgr.DeleteAttachment(args.ID); err != nil {
		return s.sendErrorResult(cmd.ID, err)
	}
	return s.sendResult(cmd.ID, map[string]any{"ok": true})
}
