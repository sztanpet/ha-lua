package web

import (
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-json-experiment/json"

	"github.com/sztanpet/ha-lua/internal/logbuf"
)

func decodeInfo(t *testing.T, h http.Handler) debugInfo {
	t.Helper()
	rec := get(t, h, "/debug/api/info")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var info debugInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &info); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return info
}

func TestDebugPageServed(t *testing.T) {
	h := DebugHandler(DebugDeps{})

	rec := get(t, h, "/debug/")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `src="../ui/debug.js"`) {
		t.Errorf("debug page does not load its asset relatively:\n%s", body)
	}
	if strings.Contains(body, `"/debug/`) || strings.Contains(body, `"/ui/`) {
		t.Error("debug page contains an absolute URL, which breaks under ingress")
	}
}

// Nil sources must degrade, not fail: a daemon with no HA link still has a
// debug page, and that is exactly when someone opens it.
func TestDebugInfoWithNoSources(t *testing.T) {
	info := decodeInfo(t, DebugHandler(DebugDeps{}))

	if info.HA != nil {
		t.Errorf("ha = %+v, want omitted", info.HA)
	}
	if info.Scripts == nil {
		t.Error("scripts is null, want an empty list")
	}
	if info.Runtime.Go == "" || info.Runtime.Goroutines == 0 {
		t.Errorf("runtime = %+v, want the process's own numbers", info.Runtime)
	}
}

func TestDebugInfoReportsRuntimeAndStorage(t *testing.T) {
	deps := DebugDeps{
		Version:       "4.0.0",
		Started:       time.Now().Add(-90 * time.Second),
		PprofAddr:     "127.0.0.1:6060",
		RetentionDays: 2,
		PurgeInterval: time.Hour,
	}
	info := decodeInfo(t, DebugHandler(deps))

	if info.Runtime.Version != "4.0.0" || info.Runtime.PprofAddr != "127.0.0.1:6060" {
		t.Errorf("runtime = %+v", info.Runtime)
	}
	if info.Runtime.Uptime != "1m30s" {
		t.Errorf("uptime = %q, want 1m30s", info.Runtime.Uptime)
	}
	if info.Storage.RetentionDays != 2 || info.Storage.PurgeInterval != "1h0m0s" {
		t.Errorf("storage = %+v", info.Storage)
	}
}

func TestDebugLogsPollIncrementally(t *testing.T) {
	buf := logbuf.New(10)
	log := slog.New(logbuf.NewHandler(slog.NewTextHandler(discard{}, nil), buf))
	h := DebugHandler(DebugDeps{Logs: buf})

	log.Info("first")
	log.Warn("second")

	var first logsReply
	rec := get(t, h, "/debug/api/logs")
	if err := json.Unmarshal(rec.Body.Bytes(), &first); err != nil {
		t.Fatal(err)
	}
	if len(first.Records) != 2 || first.Newest != 2 {
		t.Fatalf("first poll = %+v", first)
	}

	log.Error("third")

	var second logsReply
	rec = get(t, h, "/debug/api/logs?since=2")
	if err := json.Unmarshal(rec.Body.Bytes(), &second); err != nil {
		t.Fatal(err)
	}
	if len(second.Records) != 1 || second.Records[0].Msg != "third" {
		t.Fatalf("incremental poll = %+v", second)
	}
}

func TestDebugLogsLevelFilter(t *testing.T) {
	buf := logbuf.New(10)
	log := slog.New(logbuf.NewHandler(slog.NewTextHandler(discard{}, &slog.HandlerOptions{Level: slog.LevelDebug}), buf))
	h := DebugHandler(DebugDeps{Logs: buf})

	log.Info("quiet")
	log.Error("loud")

	var reply logsReply
	rec := get(t, h, "/debug/api/logs?level=ERROR")
	if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil {
		t.Fatal(err)
	}
	if len(reply.Records) != 1 || reply.Records[0].Msg != "loud" {
		t.Fatalf("filtered = %+v", reply.Records)
	}

	if rec := get(t, h, "/debug/api/logs?level=nonsense"); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad level status = %d, want 400", rec.Code)
	}
}

func TestDebugLogsScriptFilter(t *testing.T) {
	buf := logbuf.New(10)
	log := slog.New(logbuf.NewHandler(slog.NewTextHandler(discard{}, nil), buf))
	h := DebugHandler(DebugDeps{Logs: buf})

	log.Info("daemon start")
	log.Info("from alpha", "script", "alpha")
	log.Info("from beta", "script", "beta")

	for _, tc := range []struct {
		query string
		want  []string
	}{
		{"", []string{"daemon start", "from alpha", "from beta"}},
		{"&script=*", []string{"from alpha", "from beta"}},
		{"&script=alpha", []string{"from alpha"}},
		{"&script=missing", nil},
	} {
		var reply logsReply
		rec := get(t, h, "/debug/api/logs?since=0"+tc.query)
		if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil {
			t.Fatal(err)
		}
		got := make([]string, len(reply.Records))
		for i, r := range reply.Records {
			got[i] = r.Msg
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("logs%q = %v, want %v", tc.query, got, tc.want)
		}
		if reply.Newest != 3 {
			t.Errorf("logs%q newest = %d, want 3", tc.query, reply.Newest)
		}
	}
}

func TestDebugGoroutineDump(t *testing.T) {
	rec := get(t, DebugHandler(DebugDeps{}), "/debug/api/goroutines")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("content-type = %q", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "goroutine ") || !strings.Contains(body, "web.TestDebugGoroutineDump") {
		t.Errorf("dump lacks stacks:\n%s", body)
	}
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }
