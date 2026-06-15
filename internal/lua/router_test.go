package lua

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sztanpet/ha-lua/internal/scheduler"
	"github.com/sztanpet/ha-lua/internal/state"
	"github.com/sztanpet/ha-lua/internal/store"
	"github.com/sztanpet/ha-lua/internal/testutil"
)

// newUIRunner starts a single script with its run loop and registers its routes
// on a fresh Router, returning the router ready to serve.
func newUIRunner(t *testing.T, scriptID, src string) *Router {
	t.Helper()
	writeDB, readDB := testutil.NewTestDB(t, nil)
	if err := state.Migrate(writeDB); err != nil {
		t.Fatal(err)
	}
	tracker := state.New(writeDB, readDB)
	kv := store.New(writeDB, readDB, scriptID)
	global := store.NewGlobal(writeDB, readDB)

	dir := t.TempDir()
	path := filepath.Join(dir, scriptID+".lua")
	writeScript(t, dir, scriptID+".lua", src)

	reg := NewRegistry()
	router := NewRouter(reg)

	r := NewRunner(scriptID, dir, tracker, nil, kv, global)
	reg.Add(r)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); r.Start(ctx, path) }()
	t.Cleanup(func() { cancel(); <-done })

	select {
	case <-r.LoadedCh:
	case <-time.After(2 * time.Second):
		t.Fatal("script did not finish loading")
	}
	router.Register(scriptID, r.Routes())
	return router
}

func doReq(router *Router, method, target, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), method, target, strings.NewReader(body))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func waitRoute(t *testing.T, router *Router, method, path string) {
	t.Helper()
	for i := 0; i < 400; i++ {
		if _, ok := router.match(method, path); ok {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("route %s %s never registered", method, path)
}

func TestServeRoundTrip(t *testing.T) {
	router := newUIRunner(t, "ui", `
ha.serve("GET", "/api/state", function(req)
  return 200, '{"ok":true}', {["Content-Type"]="application/json"}
end)
`)
	rec := doReq(router, "GET", "/api/state", "")
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	if rec.Body.String() != `{"ok":true}` {
		t.Fatalf("body = %q", rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type = %q", ct)
	}
}

func TestServeEchoesRequestFields(t *testing.T) {
	router := newUIRunner(t, "ui", `
ha.serve("POST", "/echo", function(req)
  return 200, req.method .. " " .. req.path .. " x=" .. (req.query.x or "") .. " body=" .. req.body
end)
`)
	rec := doReq(router, "POST", "/echo?x=1", "hello")
	want := "POST /echo x=1 body=hello"
	if rec.Body.String() != want {
		t.Fatalf("body = %q, want %q", rec.Body.String(), want)
	}
}

func TestServeLongestPrefixWins(t *testing.T) {
	router := newUIRunner(t, "ui", `
ha.serve("GET", "/api", function(req) return 200, "api" end)
ha.serve("GET", "/api/state", function(req) return 200, "state" end)
`)
	if got := doReq(router, "GET", "/api/state", "").Body.String(); got != "state" {
		t.Fatalf("got %q, want state", got)
	}
	if got := doReq(router, "GET", "/api/other", "").Body.String(); got != "api" {
		t.Fatalf("got %q, want api", got)
	}
}

func TestServeUnknownRoute404(t *testing.T) {
	router := newUIRunner(t, "ui", `ha.serve("GET", "/known", function(req) return 200, "ok" end)`)
	if rec := doReq(router, "GET", "/unknown", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	// Wrong method is also a miss.
	if rec := doReq(router, "POST", "/known", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("wrong-method status = %d, want 404", rec.Code)
	}
}

func TestServeHandlerError500(t *testing.T) {
	router := newUIRunner(t, "ui", `ha.serve("GET", "/boom", function(req) error("kaboom") end)`)
	if rec := doReq(router, "GET", "/boom", ""); rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

func TestServeGarbageReturnsDefaultsTo200(t *testing.T) {
	// Returns a non-number status and nothing else: must not panic, defaults 200.
	router := newUIRunner(t, "ui", `ha.serve("GET", "/g", function(req) return "notanumber" end)`)
	rec := doReq(router, "GET", "/g", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rec.Body.String() != "" {
		t.Fatalf("body = %q, want empty", rec.Body.String())
	}
}

func TestServeNoReturnDefaultsTo200(t *testing.T) {
	router := newUIRunner(t, "ui", `ha.serve("GET", "/n", function(req) end)`)
	if rec := doReq(router, "GET", "/n", ""); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

// TestServeBusy503 covers the send-timeout path: a registered script whose run
// loop is not consuming reqCh yields a 503, not a hang.
func TestServeBusy503(t *testing.T) {
	reg := NewRegistry()
	router := NewRouter(reg)
	router.timeout = 50 * time.Millisecond

	r := &Runner{scriptID: "ui", reqCh: make(chan *request)}
	reg.Add(r)
	router.Register("ui", []RouteSpec{{Method: "GET", Prefix: "/x"}})

	if rec := doReq(router, "GET", "/x", ""); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

// TestRouterReloadReRegisters proves the §3.1a lifecycle: after a reload, the
// old route is gone and the new one serves — no stale mapping to a dead runner.
func TestRouterReloadReRegisters(t *testing.T) {
	writeDB, readDB := testutil.NewTestDB(t, nil)
	if err := state.Migrate(writeDB); err != nil {
		t.Fatal(err)
	}
	reg := NewRegistry()
	router := NewRouter(reg)
	dir := t.TempDir()
	sup := NewSupervisor(reg, dir, Deps{
		Tracker:   state.New(writeDB, readDB),
		Scheduler: scheduler.New(writeDB, time.UTC, reg.DispatchToTimer),
		Global:    store.NewGlobal(writeDB, readDB),
		Router:    router,
		NewKV:     func(id string) *store.Store { return store.New(writeDB, readDB, id) },
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	writeScript(t, dir, "ui.lua", `ha.serve("GET", "/a", function(req) return 200, "A" end)`)
	sup.StartScript(ctx, "ui")
	waitRoute(t, router, "GET", "/a")
	if rec := doReq(router, "GET", "/a", ""); rec.Code != 200 || rec.Body.String() != "A" {
		t.Fatalf("before reload: status=%d body=%q", rec.Code, rec.Body.String())
	}

	writeScript(t, dir, "ui.lua", `ha.serve("GET", "/b", function(req) return 200, "B" end)`)
	sup.Reload(ctx, "ui")
	waitRoute(t, router, "GET", "/b")

	if rec := doReq(router, "GET", "/a", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("old route still served after reload: status=%d", rec.Code)
	}
	if rec := doReq(router, "GET", "/b", ""); rec.Code != 200 || rec.Body.String() != "B" {
		t.Fatalf("new route after reload: status=%d body=%q", rec.Code, rec.Body.String())
	}

	sup.StopScript("ui")
	// After stop the route is unregistered.
	if rec := doReq(router, "GET", "/b", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("route served after stop: status=%d", rec.Code)
	}
}
