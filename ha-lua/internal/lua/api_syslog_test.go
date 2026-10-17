package lua

import (
	"context"
	"database/sql"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sztanpet/ha-lua/internal/ha"
	"github.com/sztanpet/ha-lua/internal/state"
	"github.com/sztanpet/ha-lua/internal/store"
	"github.com/sztanpet/ha-lua/internal/syslog"
	"github.com/sztanpet/ha-lua/internal/testutil"
)

// startSyslogScripts loads each id→source script into one registry.
func startSyslogScripts(t *testing.T, enabled bool, scripts map[string]string) *Registry {
	t.Helper()
	dir := t.TempDir()
	writeDB, readDB := testutil.NewTestDB(t, func(writeDB, _ *sql.DB) error {
		return state.Migrate(writeDB)
	})
	global := store.NewGlobal(writeDB, readDB)
	reg := NewRegistry()
	root := openTestRoot(t, dir)
	for id, src := range scripts {
		writeScript(t, dir, id+".lua", src)
		r := NewRunner(id, dir, root, nil, nil, nil, store.New(writeDB, readDB, id), global)
		r.SetSyslog(enabled)
		reg.Add(r)
		done := make(chan struct{})
		go func() { defer close(done); r.Start(t.Context(), filepath.Join(dir, id+".lua")) }()
		t.Cleanup(func() { <-done })
		select {
		case <-r.LoadedCh:
		case <-time.After(3 * time.Second):
			t.Fatalf("%s did not load", id)
		}
	}
	return reg
}

const syslogRecorder = `
syslog.on(function(msg)
  store.set("got", table.concat({msg.from, tostring(msg.host), tostring(msg.facility),
    tostring(msg.severity), msg.message, msg.raw}, "|"))
end)
syslog.on(function(msg) store.set("second", msg.message) end)
`

func TestSyslogDelivers(t *testing.T) {
	reg := startSyslogScripts(t, true, map[string]string{"s": syslogRecorder})
	raw := "<30>Oct 17 19:14:53 AmonHen interface,info ether3 link down"
	msg := syslog.Parse([]byte(raw))
	msg.From = "192.168.1.1"
	reg.DispatchSyslog(msg)

	kv := reg.Get("s").kv
	waitFor(t, "both handlers to run", func() bool { v, _ := kv.Get(t.Context(), "second"); return v != nil })
	got, _ := kv.Get(t.Context(), "got")
	want := "192.168.1.1|AmonHen|3|6|interface,info ether3 link down|" + raw
	if got != want {
		t.Errorf("msg = %v, want %v", got, want)
	}
}

func TestSyslogNoPriorityFieldsAreNil(t *testing.T) {
	reg := startSyslogScripts(t, true, map[string]string{"s": syslogRecorder})
	reg.DispatchSyslog(syslog.Message{From: "10.0.0.1", Priority: -1, Text: "x", Raw: "x"})
	kv := reg.Get("s").kv
	waitFor(t, "the handler to run", func() bool { v, _ := kv.Get(t.Context(), "got"); return v != nil })
	if got, _ := kv.Get(t.Context(), "got"); got != "10.0.0.1|nil|nil|nil|x|x" {
		t.Errorf("msg = %v", got)
	}
}

// TestSyslogSkipsScriptsWithoutHandler: a datagram must not wake, or fill
// the queue of, a script that never asked for syslog.
func TestSyslogSkipsScriptsWithoutHandler(t *testing.T) {
	reg := startSyslogScripts(t, true, map[string]string{
		"s":     syslogRecorder,
		"other": `ha.immediate_events()`,
	})
	other := reg.Get("other")
	if other.WantsSyslog() {
		t.Fatal("a script without syslog.on wants syslog")
	}
	reg.DispatchSyslog(syslog.Message{Priority: -1, Text: "x"})
	if n := len(other.ch); n != 0 {
		t.Errorf("other's queue holds %d events, want 0", n)
	}
	kv := reg.Get("s").kv
	waitFor(t, "s to handle it", func() bool { v, _ := kv.Get(t.Context(), "second"); return v != nil })
}

func TestSyslogRegisterFromCallback(t *testing.T) {
	reg := startSyslogScripts(t, true, map[string]string{"s": `
ha.on_event("arm", function()
  syslog.on(function(msg) store.set("late", msg.message) end)
end)
`})
	r := reg.Get("s")
	if r.WantsSyslog() {
		t.Fatal("wants syslog before any syslog.on")
	}
	reg.Dispatch(ha.Event{Type: "arm"})
	waitFor(t, "the callback to register", r.WantsSyslog)
	reg.DispatchSyslog(syslog.Message{Priority: -1, Text: "late line"})
	waitFor(t, "the late handler to run", func() bool { v, _ := r.kv.Get(t.Context(), "late"); return v == "late line" })
}

func TestSyslogDisabledRaises(t *testing.T) {
	reg := startSyslogScripts(t, false, map[string]string{"s": syslogRecorder})
	r := reg.Get("s")
	err := r.Stats().LastError
	if err == nil || !strings.Contains(err.Error, "syslog_port") {
		t.Fatalf("syslog.on with the input disabled: last error %v, want a syslog_port hint", err)
	}
	if r.WantsSyslog() {
		t.Error("a raising syslog.on still registered")
	}
}

// TestSyslogOverUDP is the whole path: socket, parser, registry, script.
func TestSyslogOverUDP(t *testing.T) {
	reg := startSyslogScripts(t, true, map[string]string{"s": syslogRecorder})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	l, err := syslog.Listen(ctx, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go l.Serve(ctx, reg.DispatchSyslog)

	var d net.Dialer
	conn, err := d.DialContext(ctx, "udp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("script,info ha-lua test")); err != nil {
		t.Fatal(err)
	}
	kv := reg.Get("s").kv
	waitFor(t, "the datagram to reach the script", func() bool { v, _ := kv.Get(t.Context(), "second"); return v != nil })
	if got, _ := kv.Get(t.Context(), "got"); got != "127.0.0.1|nil|nil|nil|script,info ha-lua test|script,info ha-lua test" {
		t.Errorf("msg = %v", got)
	}
}

func TestSyslogDropCounted(t *testing.T) {
	reg := NewRegistry()
	r := NewRunner("stuck", t.TempDir(), nil, nil, nil, nil, nil, nil)
	r.wantsSyslog.Store(true)
	reg.Add(r)
	for range cap(r.ch) + 2 {
		reg.DispatchSyslog(syslog.Message{Priority: -1})
	}
	if got := reg.SyslogDropped(); got != 2 {
		t.Errorf("SyslogDropped = %d, want 2", got)
	}
}
