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
	mu      sync.Mutex
	path    string
	segMax  int64
	file    *os.File
	size    int64
	openErr error
}

// RotateIfLarge bounds an append-per-write log (one that is opened, written,
// and closed on each record, like ha.exceptions.log_file) without holding a
// handle. When path is at or over maxTotalBytes/2 it is renamed over a single
// backup ("<path>.1"); the caller's next O_APPEND|O_CREATE open then starts a
// fresh file, so the active file plus the backup stay under maxTotalBytes.
// Best-effort: any error leaves the file as-is.
func RotateIfLarge(path string, maxTotalBytes int64) {
	segMax := maxTotalBytes / 2
	if segMax < 1 {
		segMax = 1
	}
	fi, err := os.Stat(path)
	if err != nil || fi.Size() < segMax {
		return
	}
	_ = os.Rename(path, path+".1")
}

// New opens (or creates, appending to) path and returns a writer bounded to
// maxTotalBytes across the active file plus one rotated backup.
func New(path string, maxTotalBytes int64) (*Rotating, error) {
	segMax := maxTotalBytes / 2
	if segMax < 1 {
		segMax = 1
	}
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

// rotate closes the active file, renames it over the single backup, and starts
// a fresh active file. Best-effort: on any error it tries to keep writing to
// the existing file rather than losing the writer entirely.
func (w *Rotating) rotate() {
	if err := w.file.Close(); err != nil {
		w.openErr = err
	}
	// Rename is atomic on the same filesystem; a stale backup is replaced.
	_ = os.Rename(w.path, w.path+".1")
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		// Could not start fresh; fall back to re-appending to whatever exists.
		w.openErr = err
		if reopened := w.open(); reopened != nil {
			w.file = nil
		}
		return
	}
	w.file = f
	w.size = 0
	w.openErr = nil
}

func (w *Rotating) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		if err := w.open(); err != nil {
			return 0, err
		}
	}
	// Rotate before writing when this write would push past the segment cap, so
	// a single record is never split across files. An oversized lone record
	// still goes to a freshly rotated file.
	if w.size > 0 && w.size+int64(len(p)) > w.segMax {
		w.rotate()
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
