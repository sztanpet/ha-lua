// Package cards embeds the bundled Lovelace card assets so the daemon can
// materialize them into the user's /config/www on boot, where HA serves them
// at /local/ha-lua/…. The assets are plain vanilla JS with no build step; a
// user adds one dashboard resource pointing at the served URL.
package cards

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// FS holds the card assets. The pattern excludes this Go file.
//
//go:embed *.js
var FS embed.FS

// Materialize writes every embedded asset under destDir, overwriting existing
// files and creating parent directories. Best-effort from the caller's view:
// any error is returned for logging, but the daemon does not depend on the
// assets existing.
func Materialize(destDir string) error {
	err := fs.WalkDir(FS, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		data, err := FS.ReadFile(path)
		if err != nil {
			return err
		}
		dest := filepath.Join(destDir, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return err
		}
		return os.WriteFile(dest, data, 0o644)
	})
	if err != nil {
		return fmt.Errorf("materialize cards into %q: %w", destDir, err)
	}
	return nil
}
