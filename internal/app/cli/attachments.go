package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/losinggeneration/tau/ai"
	"github.com/losinggeneration/tau/sdk"
)

func buildCLIMessage(ctx context.Context, sess sdk.Session, prompt string, first bool, fileImages []ai.ContentBlock) (sdk.Message, error) {
	processedPrompt, promptImages, err := processFileArguments(prompt)
	if err != nil {
		return sdk.Message{}, fmt.Errorf("process file arguments: %w", err)
	}

	msg := sdk.Message{Role: "user", Text: processedPrompt}
	mgr, ok := sess.(sdk.AttachmentManager)
	if !ok {
		msg.Images = promptImages
		if first {
			msg.Images = append(append([]ai.ContentBlock{}, fileImages...), msg.Images...)
			if err := appendImageFlagBlocks(ctx, &msg); err != nil {
				return sdk.Message{}, err
			}
		}
		return msg, nil
	}

	if first {
		for _, block := range fileImages {
			if block.Name == "" {
				msg.Images = append(msg.Images, block)
				continue
			}
			id, err := importCLIAttachment(mgr, block.Name)
			if err != nil {
				return sdk.Message{}, err
			}
			msg.AttachmentIDs = append(msg.AttachmentIDs, id)
		}
		if err := appendImageFlagAttachments(ctx, mgr, &msg); err != nil {
			return sdk.Message{}, err
		}
	}
	for _, block := range promptImages {
		if block.Name == "" {
			msg.Images = append(msg.Images, block)
			continue
		}
		id, err := importCLIAttachment(mgr, block.Name)
		if err != nil {
			return sdk.Message{}, err
		}
		msg.AttachmentIDs = append(msg.AttachmentIDs, id)
	}
	return msg, nil
}

func appendImageFlagBlocks(ctx context.Context, msg *sdk.Message) error {
	cmd := rootCmdFromContext(ctx)
	if cmd == nil {
		return nil
	}
	for _, path := range cmd.StringSlice("image") {
		block, err := ai.LoadImageContentBlock(path)
		if err != nil {
			return fmt.Errorf("load image %s: %w", path, err)
		}
		msg.Images = append(msg.Images, block)
	}
	return nil
}

func appendImageFlagAttachments(ctx context.Context, mgr sdk.AttachmentManager, msg *sdk.Message) error {
	cmd := rootCmdFromContext(ctx)
	if cmd == nil {
		return nil
	}
	for _, path := range cmd.StringSlice("image") {
		id, err := importCLIAttachment(mgr, path)
		if err != nil {
			return err
		}
		msg.AttachmentIDs = append(msg.AttachmentIDs, id)
	}
	return nil
}

func importCLIAttachment(mgr sdk.AttachmentManager, path string) (string, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve attachment %s: %w", path, err)
	}
	if _, err := os.Stat(absPath); err != nil {
		return "", fmt.Errorf("stat attachment %s: %w", absPath, err)
	}
	record, err := mgr.ImportAttachmentFile(absPath)
	if err != nil {
		return "", fmt.Errorf("import attachment %s: %w", absPath, err)
	}
	return record.ID, nil
}
