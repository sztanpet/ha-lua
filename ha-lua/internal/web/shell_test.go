package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-json-experiment/json"

	"github.com/sztanpet/ha-lua/internal/lua"
)

type fakeScript struct{ id, title string }

func (f fakeScript) ScriptID() string { return f.id }
func (f fakeScript) UITitle() string  { return f.title }

type fakeScripts []fakeScript

func (f fakeScripts) list() []Script {
	out := make([]Script, len(f))
	for i, s := range f {
		out[i] = s
	}
	return out
}

func get(t *testing.T, h http.Handler, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), "GET", target, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestShellServedAtRoot(t *testing.T) {
	h := Handler(Deps{})

	rec := get(t, h, "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("content-type = %q", ct)
	}
	body := rec.Body.String()
	// The shell must reach its own asset relatively: an absolute /ui/shell.js
	// escapes HA ingress's /api/hassio_ingress/<token>/ prefix.
	if !strings.Contains(body, `src="ui/shell.js"`) {
		t.Errorf("shell does not load ui/shell.js relatively:\n%s", body)
	}
	if strings.Contains(body, `"/ui/`) || strings.Contains(body, `"/api/`) {
		t.Errorf("shell contains an absolute URL, which breaks under ingress:\n%s", body)
	}
}

func TestShellAssetsServed(t *testing.T) {
	h := Handler(Deps{})

	rec := get(t, h, "/ui/shell.js")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "api/tabs") {
		t.Fatal("shell.js does not fetch the tab list")
	}
	if rec := get(t, h, "/ui/nosuch.js"); rec.Code != http.StatusNotFound {
		t.Fatalf("missing asset status = %d, want 404", rec.Code)
	}
}

func decodeTabs(t *testing.T, rec *httptest.ResponseRecorder) []tab {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var tabs []tab
	if err := json.Unmarshal(rec.Body.Bytes(), &tabs); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return tabs
}

func TestTabsEmptyWithoutRegistry(t *testing.T) {
	// An empty list, never a JSON null: shell.js branches on tabs.length.
	rec := get(t, Handler(Deps{}), "/api/tabs")
	if body := strings.TrimSpace(rec.Body.String()); body != "[]" {
		t.Fatalf("body = %q, want []", body)
	}
	if tabs := decodeTabs(t, rec); len(tabs) != 0 {
		t.Fatalf("tabs = %v, want none", tabs)
	}
}

func TestTabsListsOptedInScriptsSorted(t *testing.T) {
	scripts := fakeScripts{
		{id: "zulu", title: "Alpha"},
		{id: "alpha", title: "Zulu"},
		{id: "silent"}, // no ha.ui: not a tab
	}

	tabs := decodeTabs(t, get(t, Handler(Deps{Scripts: scripts.list}), "/api/tabs"))
	want := []tab{
		{ID: "zulu", Title: "Alpha", Path: "s/zulu/"},
		{ID: "alpha", Title: "Zulu", Path: "s/alpha/"},
	}
	if len(tabs) != len(want) {
		t.Fatalf("tabs = %+v, want %+v", tabs, want)
	}
	for i := range want {
		if tabs[i] != want[i] {
			t.Errorf("tab %d = %+v, want %+v", i, tabs[i], want[i])
		}
	}
}

func TestTabsAppendsDebugLast(t *testing.T) {
	scripts := fakeScripts{{id: "thermostat", title: "Heating"}}

	// No debug handler mounted: no Debug tab pointing at a 404.
	if tabs := decodeTabs(t, get(t, Handler(Deps{Scripts: scripts.list}), "/api/tabs")); len(tabs) != 1 {
		t.Fatalf("tabs without a debug handler = %+v, want just the script", tabs)
	}

	deps := Deps{Scripts: scripts.list, Debug: http.NotFoundHandler()}
	tabs := decodeTabs(t, get(t, Handler(deps), "/api/tabs"))
	if len(tabs) != 2 {
		t.Fatalf("tabs = %+v, want script + debug", tabs)
	}
	if last := tabs[len(tabs)-1]; last.ID != "debug" || last.Path != "debug/" {
		t.Fatalf("last tab = %+v, want the debug tab", last)
	}
}

func TestScriptRoutesMountedUnderS(t *testing.T) {
	router := lua.NewRouter(lua.NewRegistry())
	h := Handler(Deps{Router: router})

	// Unknown script: the router answers, not the shell's catch-all.
	if rec := get(t, h, "/s/nosuch/"); rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	// The shell only owns the bare root; it must not swallow other paths.
	if rec := get(t, h, "/nosuch"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown path status = %d, want 404", rec.Code)
	}
}
