package run

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const (
	// DefaultStatePath is where the Okta cursor is persisted between runs.
	DefaultStatePath = "state/okta_cursor.json"
)

type cursorState struct {
	Next string `json:"next"`
}

// LoadCursor loads the persisted Okta cursor from disk.
func LoadCursor(path string) (string, error) {
	payload, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("read state file: %w", err)
	}

	if len(payload) == 0 {
		return "", nil
	}

	var state cursorState
	if err := json.Unmarshal(payload, &state); err != nil {
		return "", fmt.Errorf("parse state file: %w", err)
	}

	return state.Next, nil
}

// SaveCursor persists the Okta cursor to disk.
func SaveCursor(path, cursor string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("ensure state directory: %w", err)
	}

	payload, err := json.MarshalIndent(cursorState{Next: cursor}, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}

	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, payload, 0o600); err != nil {
		return fmt.Errorf("write temp state file: %w", err)
	}

	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replace state file: %w", err)
	}

	return nil
}
