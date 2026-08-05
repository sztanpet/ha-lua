package logbuf

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

func newTestLogger(capacity int) (*slog.Logger, *Buffer, *bytes.Buffer) {
	var sink bytes.Buffer
	next := slog.NewTextHandler(&sink, &slog.HandlerOptions{Level: slog.LevelDebug})
	buf := New(capacity)
	return slog.New(NewHandler(next, buf)), buf, &sink
}

func msgs(recs []Record) []string {
	out := make([]string, len(recs))
	for i, r := range recs {
		out[i] = r.Msg
	}
	return out
}

func TestHandlerStillWritesThrough(t *testing.T) {
	log, buf, sink := newTestLogger(10)
	log.Info("hello", "k", "v")

	if !strings.Contains(sink.String(), "hello") || !strings.Contains(sink.String(), "k=v") {
		t.Fatalf("wrapped handler lost the record: %q", sink.String())
	}
	recs, _ := buf.Snapshot(0, slog.LevelDebug)
	if len(recs) != 1 || recs[0].Msg != "hello" || recs[0].Attrs["k"] != "v" {
		t.Fatalf("buffered = %+v", recs)
	}
}

func TestRingWrapsAndKeepsNewest(t *testing.T) {
	log, buf, _ := newTestLogger(3)
	for _, m := range []string{"one", "two", "three", "four", "five"} {
		log.Info(m)
	}

	recs, newest := buf.Snapshot(0, slog.LevelDebug)
	if got := msgs(recs); len(got) != 3 || got[0] != "three" || got[2] != "five" {
		t.Fatalf("snapshot = %v, want the last three oldest-first", got)
	}
	if newest != 5 {
		t.Fatalf("newest seq = %d, want 5", newest)
	}
	if recs[0].Seq != 3 || recs[2].Seq != 5 {
		t.Fatalf("seqs = %d..%d, want 3..5", recs[0].Seq, recs[2].Seq)
	}
}

func TestSnapshotSinceSeqIsIncremental(t *testing.T) {
	log, buf, _ := newTestLogger(10)
	log.Info("one")
	log.Info("two")

	_, seen := buf.Snapshot(0, slog.LevelDebug)
	log.Info("three")

	recs, _ := buf.Snapshot(seen, slog.LevelDebug)
	if got := msgs(recs); len(got) != 1 || got[0] != "three" {
		t.Fatalf("incremental snapshot = %v, want [three]", got)
	}
	if recs, _ := buf.Snapshot(3, slog.LevelDebug); len(recs) != 0 {
		t.Fatalf("idle poll returned %v", msgs(recs))
	}
}

func TestSnapshotFiltersByLevel(t *testing.T) {
	log, buf, _ := newTestLogger(10)
	log.Debug("noise")
	log.Info("normal")
	log.Warn("careful")
	log.Error("broken")

	recs, _ := buf.Snapshot(0, slog.LevelWarn)
	if got := msgs(recs); len(got) != 2 || got[0] != "careful" || got[1] != "broken" {
		t.Fatalf("warn+ snapshot = %v", got)
	}
}

func TestAttrsFromWithAndGroups(t *testing.T) {
	log, buf, _ := newTestLogger(10)

	log.With("script", "thermostat").Info("loaded", "routes", 3)
	log.WithGroup("ha").With("url", "ws://x").Info("connected", "retries", 2)
	log.Info("grouped", slog.Group("db", "path", "/data/x.db", "size", 12))

	recs, _ := buf.Snapshot(0, slog.LevelDebug)
	if len(recs) != 3 {
		t.Fatalf("got %d records", len(recs))
	}
	if recs[0].Attrs["script"] != "thermostat" || recs[0].Attrs["routes"] != "3" {
		t.Errorf("With attrs = %v", recs[0].Attrs)
	}
	if recs[1].Attrs["ha.url"] != "ws://x" || recs[1].Attrs["ha.retries"] != "2" {
		t.Errorf("WithGroup attrs = %v", recs[1].Attrs)
	}
	if recs[2].Attrs["db.path"] != "/data/x.db" || recs[2].Attrs["db.size"] != "12" {
		t.Errorf("inline group attrs = %v", recs[2].Attrs)
	}
}

func TestConcurrentWriters(t *testing.T) {
	log, buf, _ := newTestLogger(64)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				log.Info("spam", "j", j)
			}
		}()
	}
	go func() {
		for i := 0; i < 100; i++ {
			buf.Snapshot(0, slog.LevelDebug)
		}
	}()
	wg.Wait()

	if _, newest := buf.Snapshot(0, slog.LevelDebug); newest != 400 {
		t.Fatalf("newest seq = %d, want 400", newest)
	}
}

func TestEnabledDelegates(t *testing.T) {
	var sink bytes.Buffer
	next := slog.NewTextHandler(&sink, &slog.HandlerOptions{Level: slog.LevelWarn})
	h := NewHandler(next, New(4))

	if h.Enabled(context.Background(), slog.LevelInfo) {
		t.Error("info enabled despite a warn-level next handler")
	}
	if !h.Enabled(context.Background(), slog.LevelError) {
		t.Error("error not enabled")
	}
}
