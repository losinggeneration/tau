package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/losinggeneration/tau/ai"
	"github.com/losinggeneration/tau/sdk"
)

func messageContentBlocks(sess sdk.Session, images, attachFiles, attachmentIDs []string) ([]ai.ContentBlock, []string, error) {
	content := rpcImageBlocks(images)

	ids := append([]string(nil), attachmentIDs...)
	if len(attachFiles) == 0 {
		return content, ids, nil
	}
	mgr, err := attachmentManager(sess)
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

// rpcImageBlocks accepts the original base64 strings without requiring a
// wire change. Sniff only the prefix; the full payload is decoded downstream.
func rpcImageBlocks(images []string) []ai.ContentBlock {
	content := make([]ai.ContentBlock, 0, len(images))
	for _, data := range images {
		if header, body, ok := strings.Cut(data, ","); ok && strings.HasPrefix(header, "data:image/") && strings.HasSuffix(header, ";base64") {
			data = body
		}
		var prefix [512]byte
		n, err := io.ReadFull(base64.NewDecoder(base64.StdEncoding, strings.NewReader(data)), prefix[:])
		mimeType := "image/png" // Preserve the original default for unrecognized data.
		if err == nil || err == io.ErrUnexpectedEOF {
			if detected := http.DetectContentType(prefix[:n]); strings.HasPrefix(detected, "image/") {
				mimeType = detected
			}
		}
		content = append(content, ai.ContentBlock{Type: ai.ContentBlockImage, Data: data, MimeType: mimeType})
	}
	return content
}

func (s *Server) attachmentManager() (sdk.AttachmentManager, error) {
	s.sessionMu.Lock()
	current := s.session
	s.sessionMu.Unlock()
	return attachmentManager(current)
}

func attachmentManager(current sdk.Session) (sdk.AttachmentManager, error) {
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
	var args struct {
		ExpectedSessionID string `json:"expected_session_id,omitempty"`
		Filename          string `json:"filename"`
		MimeType          string `json:"mime_type"`
		Data              string `json:"data"`
	}
	if err := json.Unmarshal(cmd.Args, &args); err != nil {
		return s.sendErrorResult(cmd.ID, err)
	}
	var mgr sdk.AttachmentManager
	var err error
	if args.ExpectedSessionID == "" {
		mgr, err = s.attachmentManager()
	} else {
		var sess sdk.Session
		sess, err = s.draftSession(ctx, args.ExpectedSessionID)
		if err == nil {
			mgr, err = attachmentManager(sess)
		}
	}
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
