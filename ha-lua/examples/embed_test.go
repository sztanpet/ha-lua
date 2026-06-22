package bundled

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// TestMaterializeWritesEmbeddedFiles checks every embedded file lands on disk
// byte-for-byte, the lib/ subdir is recreated, and the generated README exists.
func TestMaterializeWritesEmbeddedFiles(t *testing.T) {
	dir := t.TempDir()
	if err := Materialize(dir); err != nil {
		t.Fatal(err)
	}

	var found int
	err := fs.WalkDir(FS, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		found++
		want, err := FS.ReadFile(path)
		if err != nil {
			return err
		}
		got, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(path)))
		if err != nil {
			t.Errorf("%s: not materialized: %v", path, err)
			return nil
		}
		if string(got) != string(want) {
			t.Errorf("%s: content mismatch", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if found == 0 {
		t.Fatal("no embedded files found")
	}
	if _, err := os.Stat(filepath.Join(dir, "README.txt")); err != nil {
		t.Errorf("README.txt not written: %v", err)
	}
}

// TestMaterializeOverwrites confirms a locally-modified example is restored to
// the embedded content on the next boot — the dir is a read-only reference.
func TestMaterializeOverwrites(t *testing.T) {
	dir := t.TempDir()
	if err := Materialize(dir); err != nil {
		t.Fatal(err)
	}

	const name = "thermostat.lua"
	target := filepath.Join(dir, name)
	if err := os.WriteFile(target, []byte("-- tampered\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := Materialize(dir); err != nil {
		t.Fatal(err)
	}

	want, err := FS.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Errorf("%s not overwritten back to embedded content", name)
	}
}
