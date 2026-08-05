// Package logbuf keeps the most recent log records in memory so the debug page
// can tail them. It wraps a slog.Handler rather than replacing one: stderr and
// the log file still receive every record, unchanged.
package logbuf

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// DefaultCapacity is how many records the ring keeps. Enough to see what led up
// to a problem, small enough that nobody has to think about the memory.
const DefaultCapacity = 500

// Record is one buffered log line, flattened for JSON.
type Record struct {
	Seq   uint64            `json:"seq"`
	Time  time.Time         `json:"time"`
	Level string            `json:"level"`
	Msg   string            `json:"msg"`
	Attrs map[string]string `json:"attrs,omitempty"`
}

// Buffer is a fixed-size ring of the most recent records. Safe for concurrent
// use: every handler in the tree shares one.
type Buffer struct {
	mu   sync.Mutex
	ring []Record
	next int    // where the next record goes
	seq  uint64 // records ever written; also the next record's seq
	full bool
}

// New returns a buffer holding the last capacity records.
func New(capacity int) *Buffer {
	if capacity <= 0 {
		capacity = DefaultCapacity
	}
	return &Buffer{ring: make([]Record, capacity)}
}

func (b *Buffer) append(rec Record) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.seq++
	rec.Seq = b.seq
	b.ring[b.next] = rec
	b.next = (b.next + 1) % len(b.ring)
	if b.next == 0 {
		b.full = true
	}
}

// Snapshot returns the buffered records newer than sinceSeq at or above
// minLevel, oldest first, plus the newest sequence number in the buffer. A
// caller polls with the seq it last saw; 0 asks for everything still held.
func (b *Buffer) Snapshot(sinceSeq uint64, minLevel slog.Level) ([]Record, uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()

	out := make([]Record, 0, len(b.ring))
	for _, rec := range b.ordered() {
		if rec.Seq <= sinceSeq {
			continue
		}
		if lvl, err := parseLevel(rec.Level); err == nil && lvl < minLevel {
			continue
		}
		out = append(out, rec)
	}
	return out, b.seq
}

// ordered returns the live records oldest first. Caller holds b.mu.
func (b *Buffer) ordered() []Record {
	if !b.full {
		return b.ring[:b.next]
	}
	return append(append(make([]Record, 0, len(b.ring)), b.ring[b.next:]...), b.ring[:b.next]...)
}

func parseLevel(s string) (slog.Level, error) {
	var lvl slog.Level
	err := lvl.UnmarshalText([]byte(s))
	return lvl, err
}

// Handler forwards every record to next and keeps a copy in the buffer.
type Handler struct {
	buf   *Buffer
	next  slog.Handler
	attrs []slog.Attr
	group string // dotted prefix from WithGroup, applied to attr keys
}

// NewHandler wraps next so records also land in buf.
func NewHandler(next slog.Handler, buf *Buffer) *Handler {
	return &Handler{buf: buf, next: next}
}

func (h *Handler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h *Handler) Handle(ctx context.Context, rec slog.Record) error {
	entry := Record{
		Time:  rec.Time,
		Level: rec.Level.String(),
		Msg:   rec.Message,
		Attrs: make(map[string]string, rec.NumAttrs()+len(h.attrs)),
	}
	for _, attr := range h.attrs {
		flatten(entry.Attrs, h.group, attr)
	}
	rec.Attrs(func(attr slog.Attr) bool {
		flatten(entry.Attrs, h.group, attr)
		return true
	})
	if len(entry.Attrs) == 0 {
		entry.Attrs = nil
	}
	h.buf.append(entry)

	return h.next.Handle(ctx, rec)
}

func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	clone := *h
	clone.attrs = append(append(make([]slog.Attr, 0, len(h.attrs)+len(attrs)), h.attrs...), attrs...)
	clone.next = h.next.WithAttrs(attrs)
	return &clone
}

func (h *Handler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	clone := *h
	clone.group = joinKey(h.group, name)
	clone.next = h.next.WithGroup(name)
	return &clone
}

// flatten writes attr into out as dotted key -> string. Groups recurse; the
// debug page wants flat lines, not a nested structure it has to render.
func flatten(out map[string]string, prefix string, attr slog.Attr) {
	value := attr.Value.Resolve()
	if value.Kind() == slog.KindGroup {
		group := value.Group()
		if attr.Key == "" {
			// An inline group: its members keep the current prefix.
			for _, member := range group {
				flatten(out, prefix, member)
			}
			return
		}
		for _, member := range group {
			flatten(out, joinKey(prefix, attr.Key), member)
		}
		return
	}
	if attr.Equal(slog.Attr{}) {
		return
	}
	out[joinKey(prefix, attr.Key)] = value.String()
}

func joinKey(prefix, key string) string {
	if prefix == "" {
		return key
	}
	var b strings.Builder
	b.Grow(len(prefix) + 1 + len(key))
	b.WriteString(prefix)
	b.WriteByte('.')
	b.WriteString(key)
	return b.String()
}
