package lua

import (
	"context"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
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

// TestThermostatUIBoostFlow drives the boost mutate→render cycle through the
// real backend: clicking a preset duration POSTs /api/boost, and the returned
// state re-renders the card's boost-row into a live countdown with a cancel
// button; clicking cancel POSTs /api/boost/cancel and restores the preset row.
// The first sorted card is bedroom (cards render in zone-key order).
func TestThermostatUIBoostFlow(t *testing.T) {
	ctx := newBrowserCtx(t)
	srv := serveThermostatUI(t)

	var countdown string
	var rowBack bool
	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/?lang=en"),
		chromedp.WaitVisible(".card .boost-row button", chromedp.ByQuery),
		// First preset is "10m"; clicking it boosts bedroom.
		chromedp.Click(".card .boost-row button", chromedp.ByQuery),
		chromedp.WaitVisible(".card .boosting", chromedp.ByQuery),
		chromedp.Text(".card .boosting .cd", &countdown, chromedp.ByQuery),
		// Cancel is the only button inside .boosting; it restores the preset row.
		chromedp.Click(".card .boosting button", chromedp.ByQuery),
		chromedp.WaitVisible(".card .boost-row button", chromedp.ByQuery),
		chromedp.Evaluate(`!!document.querySelector(".card .boost-row")`, &rowBack),
	); err != nil {
		t.Fatal(err)
	}
	// The countdown started under the 10-minute cap and above zero.
	if !regexp.MustCompile(`^\d+:[0-5]\d$`).MatchString(countdown) {
		t.Errorf("countdown = %q, want mm:ss", countdown)
	}
	if !rowBack {
		t.Error("preset boost-row did not return after cancel")
	}
}

// TestThermostatUILocalizesHungarian loads the page with ?lang=hu and checks
// the whole localization path resolves in a real browser: the static document
// chrome the HTML ships (the <h1>/<title>) is rewritten after load, and the
// dynamically rendered cards translate both the zone names and the boost
// fieldset legend through t(). en is covered by RendersZones; this guards that
// a non-default locale actually takes effect rather than silently falling back.
func TestThermostatUILocalizesHungarian(t *testing.T) {
	ctx := newBrowserCtx(t)
	srv := serveThermostatUI(t)

	var heading, legend string
	var zoneNames []string
	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/?lang=hu"),
		chromedp.WaitVisible(".card .boost legend", chromedp.ByQuery),
		chromedp.Text("h1", &heading, chromedp.ByQuery),
		// textContent, not Text: the legend is CSS text-transform:uppercase, so
		// innerText would report the visually upper-cased form.
		chromedp.Evaluate(`document.querySelector(".card .boost legend").textContent`, &legend),
		chromedp.Evaluate(`Array.from(document.querySelectorAll(".card .zone")).map(node => node.textContent)`, &zoneNames),
	); err != nil {
		t.Fatal(err)
	}
	if heading != "Fűtés" {
		t.Errorf("h1 = %q, want Hungarian \"Fűtés\"", heading)
	}
	if legend != "Ideiglenes felülbírálás" {
		t.Errorf("boost legend = %q, want Hungarian", legend)
	}
	got := strings.Join(zoneNames, ", ")
	for _, want := range []string{"Hálószoba", "Nappali", "Gyerekszoba"} {
		if !strings.Contains(got, want) {
			t.Errorf("zone %q not rendered in Hungarian; got %q", want, got)
		}
	}
}

// TestThermostatUIScheduleSaveRoundTrip closes the editor loop through the real
// backend: the seeded zones ship no schedule, so the editor opens empty; adding
// one entry (defaulting to Mon–Fri 07:00 21°) and clicking save PUTs
// /api/schedule, and re-opening the editor must GET the persisted schedule back
// and regroup it to the single Mon–Fri row. This is the only test that drives
// the DOM editor through both PUT and GET /api/schedule.
func TestThermostatUIScheduleSaveRoundTrip(t *testing.T) {
	ctx := newBrowserCtx(t)
	srv := serveThermostatUI(t)

	const rowCount = `document.querySelectorAll(".card .editor .row").length`
	var initialRows, savedRows int
	var group, hhmm, temp string
	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/?lang=en"),
		chromedp.WaitVisible(".card .edit-btn", chromedp.ByQuery),
		// Open the editor; with no stored schedule it has no rows.
		chromedp.Click(".card .edit-btn", chromedp.ByQuery),
		chromedp.WaitVisible(".card .editor", chromedp.ByQuery),
		// Wait out the open animation: while it runs the editor is max-height
		// clipped, so the add/save buttons are clipped and clicks miss them.
		chromedp.Poll(editorAnimationsDone, nil, chromedp.WithPollingTimeout(5*time.Second)),
		chromedp.Evaluate(rowCount, &initialRows),
		// Add an entry (defaults to Mon–Fri 07:00 21°) and save it.
		chromedp.Click(".card .editor .add", chromedp.ByQuery),
		chromedp.WaitVisible(".card .editor .row", chromedp.ByQuery),
		chromedp.Click(".card .editor .save", chromedp.ByQuery),
		chromedp.WaitNotPresent(".card .editor", chromedp.ByQuery),
		// Re-open: the editor must load the persisted schedule from the backend.
		chromedp.Click(".card .edit-btn", chromedp.ByQuery),
		chromedp.WaitVisible(".card .editor .row", chromedp.ByQuery),
		chromedp.Evaluate(rowCount, &savedRows),
		chromedp.Value(".card .editor .row select", &group, chromedp.ByQuery),
		chromedp.Value(".card .editor .row input[type=time]", &hhmm, chromedp.ByQuery),
		chromedp.Value(".card .editor .row input[type=number]", &temp, chromedp.ByQuery),
	); err != nil {
		t.Fatal(err)
	}
	if initialRows != 0 {
		t.Errorf("seeded editor had %d rows, want 0 (no schedule)", initialRows)
	}
	if savedRows != 1 {
		t.Errorf("reopened editor had %d rows, want 1", savedRows)
	}
	if group != "weekdays" || hhmm != "07:00" || temp != "21" {
		t.Errorf("persisted entry = group %q %s %s°, want weekdays 07:00 21", group, hhmm, temp)
	}
}

// TestThermostatUIScheduleTempTenths checks that the editor's temp field accepts
// and preserves tenth-of-a-degree precision. The comfort stepper quantises to
// 0.5° (ComfortStepper covers that), but the schedule editor's number input is
// step="0.1", so a typed 21.3 must survive daysFromEntries → PUT → GET →
// entriesFromDays unrounded. Typing then clicking save blurs the input, firing
// its onchange (entry.temp = Number(value)) before save reads the entries.
func TestThermostatUIScheduleTempTenths(t *testing.T) {
	ctx := newBrowserCtx(t)
	srv := serveThermostatUI(t)

	const tempSel = ".card .editor .row input[type=number]"
	var step, savedTemp string
	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/?lang=en"),
		chromedp.WaitVisible(".card .edit-btn", chromedp.ByQuery),
		chromedp.Click(".card .edit-btn", chromedp.ByQuery),
		chromedp.WaitVisible(".card .editor", chromedp.ByQuery),
		// The open animation max-height-clips the controls; clicking add before
		// it settles would miss.
		chromedp.Poll(editorAnimationsDone, nil, chromedp.WithPollingTimeout(5*time.Second)),
		chromedp.Click(".card .editor .add", chromedp.ByQuery),
		chromedp.WaitVisible(tempSel, chromedp.ByQuery),
		chromedp.AttributeValue(tempSel, "step", &step, nil, chromedp.ByQuery),
		// Replace the default 21 with a tenth-degree value, then save.
		chromedp.Clear(tempSel, chromedp.ByQuery),
		chromedp.SendKeys(tempSel, "21.3", chromedp.ByQuery),
		chromedp.Click(".card .editor .save", chromedp.ByQuery),
		chromedp.WaitNotPresent(".card .editor", chromedp.ByQuery),
		// Re-open and read back the persisted value.
		chromedp.Click(".card .edit-btn", chromedp.ByQuery),
		chromedp.WaitVisible(tempSel, chromedp.ByQuery),
		chromedp.Value(tempSel, &savedTemp, chromedp.ByQuery),
	); err != nil {
		t.Fatal(err)
	}
	if step != "0.1" {
		t.Errorf("temp input step = %q, want 0.1", step)
	}
	if savedTemp != "21.3" {
		t.Errorf("persisted temp = %q, want 21.3 (0.1 precision not preserved)", savedTemp)
	}
}

// firstCardComfort reads the bedroom card's stepper value ("21.5°" -> 21.5).
const firstCardComfort = `parseFloat(document.querySelector(".card .stepper .val").textContent)`

// TestThermostatUIComfortStepper exercises the target-temp stepper round-trip:
// the + and − buttons PUT /api/settings in half-degree steps and the returned
// state re-renders the displayed value. The half-degree quantisation lives in
// the page (setComfort rounds to the nearest 0.5), so only a browser test
// covers it.
func TestThermostatUIComfortStepper(t *testing.T) {
	ctx := newBrowserCtx(t)
	srv := serveThermostatUI(t)

	var start, afterPlus, afterMinus float64
	settled := func(want float64) chromedp.QueryAction {
		return chromedp.Poll(firstCardComfort+` === `+strconv.FormatFloat(want, 'f', -1, 64),
			nil, chromedp.WithPollingTimeout(5*time.Second))
	}
	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/?lang=en"),
		chromedp.WaitVisible(".card .stepper .val", chromedp.ByQuery),
		chromedp.Evaluate(firstCardComfort, &start),
		// + raises the seeded 21.0 to 21.5; the re-render must reflect it.
		chromedp.Click(".card .stepper button:last-child", chromedp.ByQuery),
		settled(21.5),
		chromedp.Evaluate(firstCardComfort, &afterPlus),
		// − drops it back below the start to 20.5 (two effective steps).
		chromedp.Click(".card .stepper button:first-child", chromedp.ByQuery),
		settled(21),
		chromedp.Click(".card .stepper button:first-child", chromedp.ByQuery),
		settled(20.5),
		chromedp.Evaluate(firstCardComfort, &afterMinus),
	); err != nil {
		t.Fatal(err)
	}
	if start != 21 {
		t.Errorf("start comfort = %v, want 21", start)
	}
	if afterPlus != 21.5 {
		t.Errorf("after + = %v, want 21.5", afterPlus)
	}
	if afterMinus != 20.5 {
		t.Errorf("after two − = %v, want 20.5", afterMinus)
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
