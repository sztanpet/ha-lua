package lua

import (
	"bytes"
	"context"
	"log/slog"
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
	return newUIRunners(t, map[string]string{scriptID: src})
}

// newUIRunners is newUIRunner for several scripts sharing one Registry and
// Router — what namespacing has to keep apart.
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

// doReq issues a request against script "ui"'s namespace; target is the path
// the script itself registered.
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
	for i := 0; i < 400; i++ {
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

// TestServeTwoScriptsBothServeRoot is the whole point of the mount: before
// v4.0.0 these two scripts shared one flat table and load order decided which
// of them owned "/".
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

// TestServeStripsMountFromPath: a script must never see /s/<id>.
func TestServeStripsMountFromPath(t *testing.T) {
	router := newUIRunner(t, "ui", `ha.serve("GET", "/", function(req) return 200, req.path end)`)
	if got := doReq(router, "GET", "/api/deep/thing", "").Body.String(); got != "/api/deep/thing" {
		t.Fatalf("req.path = %q", got)
	}
}

// TestServeRedirectsMissingTrailingSlash: relative fetches inside a page resolve
// one segment too high without it. The Location must stay relative so HA
// ingress's /api/hassio_ingress/<token>/ prefix survives.
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

// TestUITitle covers ha.ui: the title is cached for the shell's tab list, and a
// script that never calls it stays out of the tab bar.
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

// TestUITitleWithoutRootRouteWarns: the tab would open onto a 404, so the
// script must not be left to discover that in the browser.
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
