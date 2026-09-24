// Package logwriter provides a size-bounded io.WriteCloser for the daemon log
// file, so the log can never grow without limit on the user's /config volume.
package logwriter

import (
	"os"
	"sync"
)

// Rotating writes to a file and keeps the total on-disk size under a fixed
// budget. The budget is split into two segments: the active file and one
// rotated backup ("<path>.1"). When the active file would exceed half the
// budget it is renamed over the backup and a fresh file is started, so the two
// files together never exceed the budget while at least the previous segment of
// history is retained.
type Rotating struct {
	mu     sync.Mutex
	path   string
	segMax int64
	file   *os.File
	size   int64
}

// RotateIfLarge bounds an append-per-write log (one that is opened, written,
// and closed on each record, like ha.exceptions.log_file) without holding a
// handle. path is relative to root, which confines both the stat and the
// rename. When path is at or over maxTotalBytes/2 it is renamed over a single
// backup ("<path>.1"); the caller's next O_APPEND|O_CREATE open then starts a
// fresh file, so the active file plus the backup stay under maxTotalBytes.
// Best-effort: any error leaves the file as-is.
func RotateIfLarge(root *os.Root, path string, maxTotalBytes int64) {
	segMax := max(maxTotalBytes/2, 1)
	fi, err := root.Stat(path)
	if err != nil || fi.Size() < segMax {
		return
	}
	_ = root.Rename(path, path+".1")
}

// New opens (or creates, appending to) path and returns a writer bounded to
// maxTotalBytes across the active file plus one rotated backup.
func New(path string, maxTotalBytes int64) (*Rotating, error) {
	segMax := max(maxTotalBytes/2, 1)
	w := &Rotating{path: path, segMax: segMax}
	if err := w.open(); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *Rotating) open() error {
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return err
	}
	w.file = f
	w.size = fi.Size()
	return nil
}

// rotate closes the active file and renames it over the single backup; the next
// write creates a fresh one. A rename that fails leaves nowhere to rotate into,
// so the file is truncated instead: the budget is the promise here, the tail of
// the log is not.
func (w *Rotating) rotate() {
	_ = w.file.Close()
	w.file, w.size = nil, 0
	// Rename is atomic on the same filesystem; a stale backup is replaced.
	if err := os.Rename(w.path, w.path+".1"); err != nil {
		_ = os.Truncate(w.path, 0)
	}
}

func (w *Rotating) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	// Rotate before writing when this write would push past the segment cap, so
	// a single record is never split across files. An oversized lone record
	// still goes to a freshly rotated file.
	if w.file != nil && w.size > 0 && w.size+int64(len(p)) > w.segMax {
		w.rotate()
	}
	if w.file == nil {
		if err := w.open(); err != nil {
			return 0, err
		}
	}
	n, err := w.file.Write(p)
	w.size += int64(n)
	return n, err
}

// Close closes the active file.
func (w *Rotating) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return nil
	}
	return w.file.Close()
}
