// Package xdg implements the one piece of the XDG Base Directory
// Specification jev-mcp needs: resolving the user's data directory. The Go
// standard library has os.UserConfigDir() but no equivalent for
// $XDG_DATA_HOME, so that fallback is implemented once here and shared by
// every package that needs a data-directory path (internal/audit's audit
// log, internal/credentials' opencode auth store lookup) rather than being
// duplicated per call site.
package xdg

import (
	"fmt"
	"os"
	"path/filepath"
)

// DataHome returns $XDG_DATA_HOME, or $HOME/.local/share when that's unset
// or empty, per the XDG Base Directory Specification's fallback rule for
// $XDG_DATA_HOME.
func DataHome() (string, error) {
	if dataHome := os.Getenv("XDG_DATA_HOME"); dataHome != "" {
		return dataHome, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolving home directory: %w", err)
	}
	return filepath.Join(home, ".local", "share"), nil
}
