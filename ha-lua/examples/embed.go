// Package bundled embeds the shipped reference example scripts so the daemon
// can materialize them, read-only, into the user's config dir on boot. The
// examples are never executed; they are documentation the user copies into
// their own scripts dir and edits.
package bundled

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// FS holds the example scripts and assets. The patterns deliberately exclude
// this Go file, so embed.go is never written out by Materialize.
//
//go:embed *.lua lib/*.lua *.html
var FS embed.FS

// readme marks the materialized directory as generated. It is written fresh
// each time rather than embedded, so it never travels when a user copies an
// example into their own scripts dir.
const readme = `This directory is auto-generated on every boot from the installed ha-lua
add-on version. Do NOT edit these files — your changes will be overwritten.

To use an example, copy it into ../scripts/ and edit it there:

    cp thermostat.lua ../scripts/
    cp -r lib ../scripts/

Only ../scripts/ is loaded and hot-reloaded; this directory is reference only.
`

// Materialize writes every embedded file under destDir, overwriting existing
// files and creating parent directories, then writes a README.txt. It is
// best-effort from the caller's view: any error is returned for logging, but
// the daemon does not depend on the examples existing.
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
		return fmt.Errorf("materialize examples into %q: %w", destDir, err)
	}
	if err := os.WriteFile(filepath.Join(destDir, "README.txt"), []byte(readme), 0o644); err != nil {
		return fmt.Errorf("write examples readme: %w", err)
	}
	return nil
}
