package cli

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
)

//go:embed compose.yaml
var embeddedCompose []byte

func materializeCompose(stateDir string) (string, error) {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return "", fmt.Errorf("create local state directory: %w", err)
	}
	path := filepath.Join(stateDir, "compose.yaml")
	if err := os.WriteFile(path, embeddedCompose, 0o600); err != nil {
		return "", fmt.Errorf("write local Compose definition: %w", err)
	}
	return path, nil
}
