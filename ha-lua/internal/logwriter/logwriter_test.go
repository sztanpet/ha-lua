package logwriter

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRotatingBoundsTotalSize(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ha-lua.log")
	// Budget 200 bytes -> 100-byte segments.
	w, err := New(path, 200)
	if err != nil {
		t.Fatal(err)
	}
	line := strings.Repeat("x", 40) + "\n" // 41 bytes
	for range 50 {                         // 2050 bytes total, far over budget
		if _, err := w.Write([]byte(line)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	active := statSize(t, path)
	backup := statSize(t, path+".1")
	if active > 100 {
		t.Errorf("active file = %d bytes, want <= 100 (segment cap)", active)
	}
	if backup > 100 {
		t.Errorf("backup file = %d bytes, want <= 100 (segment cap)", backup)
	}
	if active+backup > 200 {
		t.Errorf("total = %d bytes, want <= 200 (budget)", active+backup)
	}
	// The most recent writes must survive in the active file.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "x") {
		t.Error("active file lost recent content")
	}
}

func TestRotatingAppendsToExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ha-lua.log")
	if err := os.WriteFile(path, []byte("prior\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	w, err := New(path, 10_000)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("more\n")); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	data, _ := os.ReadFile(path)
	if got := string(data); got != "prior\nmore\n" {
		t.Errorf("content = %q, want appended", got)
	}
}

func statSize(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatal(err)
	}
	return fi.Size()
}
