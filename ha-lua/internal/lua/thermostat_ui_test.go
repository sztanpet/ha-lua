package lua

import (
	"context"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/go-json-experiment/json/jsontext"

	"github.com/sztanpet/ha-lua/internal/ha"
	"github.com/sztanpet/ha-lua/internal/scheduler"
	"github.com/sztanpet/ha-lua/internal/state"
	"github.com/sztanpet/ha-lua/internal/store"
	"github.com/sztanpet/ha-lua/internal/testutil"
)

// serveThermostatUI boots the real thermostat.lua (libs, scheduler, router) the
// same way TestThermostatAPI does, seeds three heating zones so cards render,
// and exposes it over a real listening server for a browser to drive.
func serveThermostatUI(t *testing.T) *httptest.Server {
	t.Helper()
	dir := t.TempDir()
	libDir := filepath.Join(dir, "lib")
	if err := os.MkdirAll(libDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestZones(t, libDir)
	copyRepoFile(t, filepath.Join(repoScriptsDir, "lib", "schedule.lua"), filepath.Join(libDir, "schedule.lua"))
	copyRepoFile(t, filepath.Join(repoScriptsDir, "thermostat.lua"), filepath.Join(dir, "thermostat.lua"))
	copyRepoFile(t, filepath.Join(repoScriptsDir, "thermostat.html"), filepath.Join(dir, "thermostat.html"))

	writeDB, readDB := testutil.NewTestDB(t, nil)
	if err := state.Migrate(writeDB); err != nil {
		t.Fatal(err)
	}
	tracker := state.New(writeDB, readDB)
	kv := store.New(writeDB, readDB, "thermostat")
	global := store.NewGlobal(writeDB, readDB)
	reg := NewRegistry()
	router := NewRouter(reg)
	sched := scheduler.New(writeDB, time.UTC, reg.DispatchToTimer)

	if err := tracker.Seed(context.Background(), []ha.StateData{
		{EntityID: "climate.bedroom", State: "heat", Attributes: jsontext.Value(`{"current_temperature":19.5,"temperature":18}`)},
		{EntityID: "climate.livingroom", State: "heat", Attributes: jsontext.Value(`{"current_temperature":21.0,"temperature":20}`)},
		{EntityID: "climate.childrens_room", State: "heat", Attributes: jsontext.Value(`{"current_temperature":20.0,"temperature":19}`)},
	}); err != nil {
		t.Fatal(err)
	}

	r := NewRunner("thermostat", dir, openTestRoot(t, dir), tracker, sched, kv, global)
	r.SetCallService(func(context.Context, string, string, jsontext.Value) error { return nil })
	reg.Add(r)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); r.Start(ctx, filepath.Join(dir, "thermostat.lua")) }()
	t.Cleanup(func() { cancel(); <-done })

	select {
	case <-r.LoadedCh:
	case <-time.After(3 * time.Second):
		t.Fatal("thermostat.lua did not finish loading")
	}
	router.Register("thermostat", r.Routes())

	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)
	return srv
}

// chromeNames mirrors the binaries chromedp's exec allocator searches for. We
// probe them up front so the browser tests skip cleanly on hosts and CI images
// without a browser installed, rather than failing make test/make check.
var chromeNames = []string{
	"google-chrome-stable", "google-chrome", "chromium-browser", "chromium",
	"headless-shell", "chrome",
}

func findChrome() string {
	if p := os.Getenv("CHROMEDP_BROWSER"); p != "" {
		return p
	}
	for _, name := range chromeNames {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	if runtime.GOOS == "darwin" {
		const macChrome = "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
		if _, err := os.Stat(macChrome); err == nil {
			return macChrome
		}
	}
	return ""
}

// newBrowserCtx skips the test when no browser is found, otherwise returns a
// headless chromedp context bounded by a timeout so a wedged browser can't hang
// the suite. NoSandbox keeps it working as root inside CI containers.
func newBrowserCtx(t *testing.T) context.Context {
	t.Helper()
	browser := findChrome()
	if browser == "" {
		t.Skip("no Chrome/Chromium found (set CHROMEDP_BROWSER to override); skipping browser UI test")
	}
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.ExecPath(browser),
		chromedp.NoSandbox,
	)
	allocCtx, cancelAlloc := chromedp.NewExecAllocator(context.Background(), opts...)
	t.Cleanup(cancelAlloc)
	browserCtx, cancelBrowser := chromedp.NewContext(allocCtx)
	t.Cleanup(cancelBrowser)
	boundedCtx, cancelTimeout := context.WithTimeout(browserCtx, 30*time.Second)
	t.Cleanup(cancelTimeout)
	return boundedCtx
}

// TestThermostatUIRendersZones drives the actual served page in a browser and
// checks every seeded zone renders with its translated display name.
func TestThermostatUIRendersZones(t *testing.T) {
	ctx := newBrowserCtx(t)
	srv := serveThermostatUI(t)

	var zoneNames []string
	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/?lang=en"),
		chromedp.WaitVisible(".card .zone", chromedp.ByQuery),
		chromedp.Evaluate(`Array.from(document.querySelectorAll(".card .zone")).map(node => node.textContent)`, &zoneNames),
	); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(zoneNames, ", ")
	for _, want := range []string{"Bedroom", "Living room", "Children's"} {
		if !strings.Contains(got, want) {
			t.Errorf("zone %q not rendered; got %q", want, got)
		}
	}
}

// editorAnimationsDone is a predicate that is truthy once the editor exists and
// has no running animations. While the open animation runs the editor is
// max-height clipped, so its cancel button is visually clipped and a click on
// it lands on empty space; waiting for the animation to settle makes the click
// hit the button.
const editorAnimationsDone = `document.querySelector(".card .editor") !== null && ` +
	`document.querySelector(".card .editor").getAnimations().length === 0`

// TestThermostatUIEditorToggle exercises the open/close state machine end to
// end: clicking edit fetches the schedule and reveals the editor, and cancel
// runs the close animation to completion and removes it.
func TestThermostatUIEditorToggle(t *testing.T) {
	ctx := newBrowserCtx(t)
	srv := serveThermostatUI(t)

	var openedEditor, closedEditor bool
	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/?lang=en"),
		chromedp.WaitVisible(".card .edit-btn", chromedp.ByQuery),
		chromedp.Click(".card .edit-btn", chromedp.ByQuery),
		chromedp.WaitVisible(".card .editor", chromedp.ByQuery),
		chromedp.Evaluate(`!!document.querySelector(".card .editor")`, &openedEditor),
		chromedp.Poll(editorAnimationsDone, nil, chromedp.WithPollingTimeout(5*time.Second)),
		chromedp.Click(".card .editor .cancel", chromedp.ByQuery),
		chromedp.WaitNotPresent(".card .editor", chromedp.ByQuery),
		chromedp.Evaluate(`!!document.querySelector(".card .editor")`, &closedEditor),
	); err != nil {
		t.Fatal(err)
	}
	if !openedEditor {
		t.Error("editor did not open after clicking edit")
	}
	if closedEditor {
		t.Error("editor still present after cancel")
	}
}

// TestThermostatUIScheduleGrouping checks the schedule round-trip logic in the
// real page context: a transition shared by all seven days collapses to one
// "everyday" editor entry, and expands back to seven per-day entries.
func TestThermostatUIScheduleGrouping(t *testing.T) {
	ctx := newBrowserCtx(t)
	srv := serveThermostatUI(t)

	const script = `(() => {
		const days = {};
		for (let day = 0; day < 7; day++) days[String(day)] = [{ time: "07:00", temp: 21 }];
		const entries = entriesFromDays(normalizeDays(days));
		const back = daysFromEntries(entries);
		return {
			entryCount: entries.length,
			group: entries[0] && entries[0].group,
			expandedDays: Object.keys(back).filter(day => back[day].length === 1).length,
		};
	})()`
	var res struct {
		EntryCount   int    `json:"entryCount"`
		Group        string `json:"group"`
		ExpandedDays int    `json:"expandedDays"`
	}
	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/?lang=en"),
		chromedp.WaitVisible(".card", chromedp.ByQuery),
		chromedp.Evaluate(script, &res),
	); err != nil {
		t.Fatal(err)
	}
	if res.EntryCount != 1 || res.Group != "everyday" {
		t.Errorf("grouping: got %d entries group=%q, want 1 everyday", res.EntryCount, res.Group)
	}
	if res.ExpandedDays != 7 {
		t.Errorf("round-trip expanded to %d days, want 7", res.ExpandedDays)
	}
}
