package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/google/uuid"
)

// GetOrCreateDeviceID lazily persists a global installation ID under the settings lock.
func GetOrCreateDeviceID(ctx context.Context, settingsPath string) (string, error) {
	var id string
	err := withSettingsLock(ctx, settingsPath, func(ctx context.Context) error {
		data, err := os.ReadFile(settingsPath)
		if errors.Is(err, os.ErrNotExist) {
			data = []byte("{}")
		} else if err != nil {
			return fmt.Errorf("read device settings: %w", err)
		}
		settings, err := ParseSettings(data)
		if err != nil {
			return err
		}
		if settings.DeviceID != "" {
			if err := uuid.Validate(settings.DeviceID); err != nil {
				return fmt.Errorf("invalid deviceId: %w", err)
			}
			if len(settings.DeviceID) != 36 {
				return fmt.Errorf("invalid deviceId: expected canonical UUID")
			}
			id = settings.DeviceID
			return context.Cause(ctx)
		}
		generated, err := uuid.NewRandom()
		if err != nil {
			return fmt.Errorf("generate device ID: %w", err)
		}
		id = generated.String()
		patch, err := json.Marshal(map[string]string{"deviceId": id})
		if err != nil {
			return fmt.Errorf("encode device ID: %w", err)
		}
		merged, err := mergeSettingsJSON(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")), patch)
		if err != nil {
			return err
		}
		pretty, err := prettyJSON(merged)
		if err != nil {
			return err
		}
		return replaceSettings(ctx, settingsPath, pretty)
	})
	if err != nil {
		return "", err
	}
	return id, nil
}
