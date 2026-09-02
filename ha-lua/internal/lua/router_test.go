package lua

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
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
	return newUIRunners(t, map[string]string{scriptID: src})
}

func newUIRunners(t *testing.T, scripts map[string]string) *Router {
	t.Helper()
	return newUIRunnersIn(t, NewRegistry(), scripts)
}

func newUIRunnersIn(t *testing.T, reg *Registry, scripts map[string]string) *Router {
	t.Helper()
	writeDB, readDB := testutil.NewTestDB(t, nil)
	if err := state.Migrate(writeDB); err != nil {
		t.Fatal(err)
	}
	tracker := state.New(writeDB, readDB)
	global := store.NewGlobal(writeDB, readDB)

	dir := t.TempDir()
	router := NewRouter(reg)
	ctx, cancel := context.WithCancel(context.Background())

	for scriptID, src := range scripts {
		path := filepath.Join(dir, scriptID+".lua")
		writeScript(t, dir, scriptID+".lua", src)

		r := NewRunner(scriptID, dir, nil, nil, tracker, nil, store.New(writeDB, readDB, scriptID), global)
		reg.Add(r)
		done := make(chan struct{})
		go func() { defer close(done); r.Start(ctx, path) }()
		t.Cleanup(func() { <-done })

		select {
		case <-r.LoadedCh:
		case <-time.After(2 * time.Second):
			t.Fatalf("script %s did not finish loading", scriptID)
		}
		router.Register(scriptID, r.Routes())
	}
	t.Cleanup(cancel)
	return router
}

// doReq targets script "ui"'s namespace.
func doReq(router *Router, method, target, body string) *httptest.ResponseRecorder {
	return doReqID(router, "ui", method, target, body)
}

func doReqID(router *Router, scriptID, method, target, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), method, Mount+scriptID+target, strings.NewReader(body))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func waitRoute(t *testing.T, router *Router, method, path string) {
	t.Helper()
	waitRouteID(t, router, "ui", method, path)
}

func waitRouteID(t *testing.T, router *Router, scriptID, method, path string) {
	t.Helper()
	for range 400 {
		if router.match(scriptID, method, path) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("route %s %s never registered for %s", method, path, scriptID)
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

func TestServeTwoScriptsBothServeRoot(t *testing.T) {
	router := newUIRunners(t, map[string]string{
		"alpha": `
ha.serve("GET", "/", function(req) return 200, "alpha root" end)
ha.serve("GET", "/api/x", function(req) return 200, "alpha x" end)
`,
		"beta": `
ha.serve("GET", "/", function(req) return 200, "beta root" end)
ha.serve("GET", "/api/x", function(req) return 200, "beta x" end)
`,
	})

	for _, tc := range []struct{ id, path, want string }{
		{"alpha", "/", "alpha root"},
		{"beta", "/", "beta root"},
		{"alpha", "/api/x", "alpha x"},
		{"beta", "/api/x", "beta x"},
	} {
		if got := doReqID(router, tc.id, "GET", tc.path, "").Body.String(); got != tc.want {
			t.Errorf("GET /s/%s%s = %q, want %q", tc.id, tc.path, got, tc.want)
		}
	}
}

func TestServeStripsMountFromPath(t *testing.T) {
	router := newUIRunner(t, "ui", `ha.serve("GET", "/", function(req) return 200, req.path end)`)
	if got := doReq(router, "GET", "/api/deep/thing", "").Body.String(); got != "/api/deep/thing" {
		t.Fatalf("req.path = %q", got)
	}
}

func TestServeRedirectsMissingTrailingSlash(t *testing.T) {
	router := newUIRunner(t, "ui", `ha.serve("GET", "/", function(req) return 200, "root" end)`)

	req := httptest.NewRequestWithContext(context.Background(), "GET", "/s/ui?lang=en", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusPermanentRedirect {
		t.Fatalf("status = %d, want 308", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "ui/?lang=en" {
		t.Fatalf("Location = %q, want relative %q", loc, "ui/?lang=en")
	}
}

func TestServeUnknownScript404(t *testing.T) {
	router := newUIRunner(t, "ui", `ha.serve("GET", "/", function(req) return 200, "root" end)`)

	for _, target := range []string{"/s/nosuch/", "/s/", "/", "/debug/", "/api/tabs"} {
		req := httptest.NewRequestWithContext(context.Background(), "GET", target, nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s status = %d, want 404", target, rec.Code)
		}
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
	ctx := t.Context()

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

func TestUITitle(t *testing.T) {
	for _, tc := range []struct {
		name, src, want string
	}{
		{"opted in", `ha.ui("Heating")
ha.serve("GET", "/", function(req) return 200, "x" end)`, "Heating"},
		{"not opted in", `ha.serve("GET", "/", function(req) return 200, "x" end)`, ""},
		{"last call wins", `ha.ui("First")
ha.ui("Second")
ha.serve("GET", "/", function(req) return 200, "x" end)`, "Second"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg := NewRegistry()
			newUIRunnersIn(t, reg, map[string]string{"ui": tc.src})
			if got := reg.Get("ui").UITitle(); got != tc.want {
				t.Fatalf("UITitle() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestUITitleWithoutRootRouteWarns(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	newUIRunner(t, "ui", `
ha.ui("Heating")
ha.serve("GET", "/api/x", function(req) return 200, "x" end)
`)
	if !strings.Contains(buf.String(), "UI tab") {
		t.Fatalf("no warning logged, got:\n%s", buf.String())
	}
}

func TestRunnerStatsReportsLoadTimeShape(t *testing.T) {
	reg := NewRegistry()
	newUIRunnersIn(t, reg, map[string]string{"ui": `
ha.ui("Panel")
ha.immediate_events()
ha.serve("GET", "/", function(req) return 200, "x" end)
ha.serve("POST", "/api/x", function(req) return 200, "x" end)
ha.on_state_change("light.*", function() end)
ha.on_state_change("switch.*", function() end)
ha.on_event("custom_event", function() end)
`})

	st := reg.Get("ui").Stats()
	if st.ScriptID != "ui" || st.UITitle != "Panel" {
		t.Errorf("identity = %+v", st)
	}
	if len(st.Routes) != 2 {
		t.Errorf("routes = %+v, want 2", st.Routes)
	}
	if st.StateHandlers != 2 || st.EventHandlers != 1 {
		t.Errorf("handlers: state=%d event=%d, want 2/1", st.StateHandlers, st.EventHandlers)
	}
	if !st.Immediate {
		t.Error("immediate_events not reported")
	}
	if st.QueueCap == 0 {
		t.Error("queue cap not reported")
	}
	if st.Dropped != 0 || st.LastError != nil {
		t.Errorf("fresh script has dropped=%d lastError=%+v", st.Dropped, st.LastError)
	}
}

func TestRunnerStatsRecordsLastError(t *testing.T) {
	reg := NewRegistry()
	router := newUIRunnersIn(t, reg, map[string]string{
		"ui": `ha.serve("GET", "/boom", function(req) error("kaboom") end)`,
	})
	doReq(router, "GET", "/boom", "")

	st := reg.Get("ui").Stats()
	if st.LastError == nil {
		t.Fatal("no last error recorded")
	}
	if !strings.Contains(st.LastError.Error, "kaboom") {
		t.Errorf("error = %q", st.LastError.Error)
	}
	if st.LastError.Callback != "GET /boom" {
		t.Errorf("callback = %q", st.LastError.Callback)
	}
	if st.LastError.Time.IsZero() {
		t.Error("error has no timestamp")
	}
}

func TestRunnerStatsCountsDroppedEvents(t *testing.T) {
	r := &Runner{scriptID: "ui", ch: make(chan Event, 1)}
	for range 5 {
		r.Send(Event{})
	}
	if got := r.Stats().Dropped; got != 4 {
		t.Fatalf("dropped = %d, want 4", got)
	}
	if got := r.Stats().QueueLen; got != 1 {
		t.Fatalf("queue len = %d, want 1", got)
	}
}

func TestRegistryAllIsOrderedByScriptID(t *testing.T) {
	reg := NewRegistry()
	for _, id := range []string{"zulu", "alpha", "mike", "bravo"} {
		reg.Add(&Runner{scriptID: id})
	}

	want := []string{"alpha", "bravo", "mike", "zulu"}
	// Repeated because the bug it guards against is map iteration order: a
	// single pass can match by luck.
	for range 20 {
		var got []string
		for _, r := range reg.All() {
			got = append(got, r.ScriptID())
		}
		if !slices.Equal(got, want) {
			t.Fatalf("All() = %v, want %v", got, want)
		}
	}
}
