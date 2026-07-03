package lua

import (
	"context"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/chromedp"
	"github.com/go-json-experiment/json/jsontext"

	"github.com/sztanpet/ha-lua/internal/ha"
	"github.com/sztanpet/ha-lua/internal/scheduler"
	"github.com/sztanpet/ha-lua/internal/state"
	"github.com/sztanpet/ha-lua/internal/store"
	"github.com/sztanpet/ha-lua/internal/testutil"
)

// defaultZoneSeed is the three-heating-zone fixture the UI tests render: one
// climate entity per zone in the testZonesLua fixture, all in heat mode. Tests
// that need a different mode copy it and tweak one entry.
func defaultZoneSeed() []ha.StateData {
	return []ha.StateData{
		{EntityID: "climate.bedroom", State: "heat", Attributes: jsontext.Value(`{"current_temperature":19.5,"temperature":18}`)},
		{EntityID: "climate.livingroom", State: "heat", Attributes: jsontext.Value(`{"current_temperature":21.0,"temperature":20}`)},
		{EntityID: "climate.childrens_room", State: "heat", Attributes: jsontext.Value(`{"current_temperature":20.0,"temperature":19}`)},
	}
}

// serveThermostatUI boots the real thermostat.lua (libs, scheduler, router) the
// same way TestThermostatAPI does, seeds three heating zones so cards render,
// and exposes it over a real listening server for a browser to drive.
func serveThermostatUI(t *testing.T) *httptest.Server {
	return serveThermostatUISeed(t, defaultZoneSeed())
}

// serveThermostatUISeed is serveThermostatUI with a caller-supplied seed, so a
// test can render a zone in a non-heat mode without disturbing the others.
func serveThermostatUISeed(t *testing.T, seed []ha.StateData) *httptest.Server {
	t.Helper()
	dir := t.TempDir()
	libDir := filepath.Join(dir, "lib")
	if err := os.MkdirAll(libDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestZones(t, libDir)
	copyRepoFile(t, filepath.Join(repoScriptsDir, "lib", "schedule.lua"), filepath.Join(libDir, "schedule.lua"))
	copyRepoFile(t, filepath.Join(repoScriptsDir, "lib", "control.lua"), filepath.Join(libDir, "control.lua"))
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

	if err := tracker.Seed(context.Background(), seed); err != nil {
		t.Fatal(err)
	}

	r := NewRunner("thermostat", dir, openTestRoot(t, dir), openTestRoot(t, t.TempDir()), tracker, sched, kv, global)
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

// TestThermostatUINotControlledCard seeds one zone in a non-heat hvac mode and
// checks its card degrades to the "not controlled" notice with no override or
// stepper controls, while the heat zones keep theirs. The card body branches on
// status.mode in zoneCard, so only a rendered DOM exercises it.
func TestThermostatUINotControlledCard(t *testing.T) {
	ctx := newBrowserCtx(t)
	seed := defaultZoneSeed()
	seed[1].State = "off" // livingroom is switched off, not heating
	srv := serveThermostatUISeed(t, seed)

	// One card shows the .muted notice; the off card carries no controls; the
	// two heat cards keep their .override fieldset.
	const script = `(() => {
		const cards = Array.from(document.querySelectorAll(".card"));
		const muted = cards.filter(card => card.querySelector(".muted"));
		return {
			mutedCount: muted.length,
			mutedText: muted[0] ? muted[0].querySelector(".muted").textContent : "",
			mutedHasControls: muted.some(card => card.querySelector(".override, .stepper, .override-row")),
			controlledCards: cards.filter(card => card.querySelector(".override")).length,
		};
	})()`
	var res struct {
		MutedCount       int    `json:"mutedCount"`
		MutedText        string `json:"mutedText"`
		MutedHasControls bool   `json:"mutedHasControls"`
		ControlledCards  int    `json:"controlledCards"`
	}
	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/?lang=en"),
		chromedp.WaitVisible(".card .muted", chromedp.ByQuery),
		chromedp.Evaluate(script, &res),
	); err != nil {
		t.Fatal(err)
	}
	if res.MutedCount != 1 {
		t.Errorf("not-controlled cards = %d, want 1", res.MutedCount)
	}
	if !strings.Contains(res.MutedText, "off") || !strings.Contains(res.MutedText, "not controlled") {
		t.Errorf("notice = %q, want it to name the mode and \"not controlled\"", res.MutedText)
	}
	if res.MutedHasControls {
		t.Error("not-controlled card still shows override/stepper controls")
	}
	if res.ControlledCards != 2 {
		t.Errorf("heat cards with controls = %d, want 2", res.ControlledCards)
	}
}

// TestThermostatUIStatusLabel checks the card head badge: a heat zone reads
// "on", or "heating" while the device is actively calling for heat
// (hvac_action), and the raw "heat" mode word is never shown. statusLabel runs
// in zoneCard, so only a rendered DOM exercises it.
func TestThermostatUIStatusLabel(t *testing.T) {
	ctx := newBrowserCtx(t)
	srv := serveThermostatUISeed(t, []ha.StateData{
		{EntityID: "climate.bedroom", State: "heat", Attributes: jsontext.Value(`{"current_temperature":19.5,"temperature":18,"hvac_action":"heating"}`)},
		{EntityID: "climate.livingroom", State: "heat", Attributes: jsontext.Value(`{"current_temperature":21.0,"temperature":20,"hvac_action":"idle"}`)},
		{EntityID: "climate.childrens_room", State: "heat", Attributes: jsontext.Value(`{"current_temperature":20.0,"temperature":19}`)},
	})

	const script = `Array.from(document.querySelectorAll(".card .status b")).map(el => el.textContent)`
	var labels []string
	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/?lang=en"),
		chromedp.WaitVisible(".card .status b", chromedp.ByQuery),
		chromedp.Evaluate(script, &labels),
	); err != nil {
		t.Fatal(err)
	}
	heating, on := 0, 0
	for _, label := range labels {
		switch label {
		case "heating":
			heating++
		case "on":
			on++
		case "heat":
			t.Errorf("status badge shows raw %q, want on/heating", label)
		}
	}
	// bedroom is actively heating; the idle and the action-less zones read "on".
	if heating != 1 {
		t.Errorf("heating badges = %d, want 1 (labels %v)", heating, labels)
	}
	if on != 2 {
		t.Errorf("on badges = %d, want 2 (labels %v)", on, labels)
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

// TestThermostatUIOverrideFlow drives the override mutate→render cycle through
// the real backend: clicking a preset duration POSTs /api/override, and the
// returned state re-renders the card's override-row into a live countdown with a
// cancel button; clicking cancel POSTs /api/override/cancel and restores the
// preset row. The first sorted card is bedroom (cards render in zone-key order).
func TestThermostatUIOverrideFlow(t *testing.T) {
	ctx := newBrowserCtx(t)
	srv := serveThermostatUI(t)

	var countdown string
	var rowBack bool
	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/?lang=en"),
		chromedp.WaitVisible(".card .override-row button", chromedp.ByQuery),
		// First preset is "10m"; clicking it overrides bedroom.
		chromedp.Click(".card .override-row button", chromedp.ByQuery),
		chromedp.WaitVisible(".card .overriding", chromedp.ByQuery),
		chromedp.Text(".card .overriding .cd", &countdown, chromedp.ByQuery),
		// Cancel is the only button inside .overriding; it restores the preset row.
		chromedp.Click(".card .overriding button", chromedp.ByQuery),
		chromedp.WaitVisible(".card .override-row button", chromedp.ByQuery),
		chromedp.Evaluate(`!!document.querySelector(".card .override-row")`, &rowBack),
	); err != nil {
		t.Fatal(err)
	}
	// The countdown started under the 10-minute cap and above zero.
	if !regexp.MustCompile(`^\d+:[0-5]\d$`).MatchString(countdown) {
		t.Errorf("countdown = %q, want mm:ss", countdown)
	}
	if !rowBack {
		t.Error("preset override-row did not return after cancel")
	}
}

// TestThermostatUILocalizesHungarian loads the page with ?lang=hu and checks
// the whole localization path resolves in a real browser: the static document
// chrome the HTML ships (the <h1>/<title>) is rewritten after load, and the
// dynamically rendered cards translate both the zone names and the override
// fieldset legend through t(). en is covered by RendersZones; this guards that
// a non-default locale actually takes effect rather than silently falling back.
func TestThermostatUILocalizesHungarian(t *testing.T) {
	ctx := newBrowserCtx(t)
	srv := serveThermostatUI(t)

	var heading, legend string
	var zoneNames []string
	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/?lang=hu"),
		chromedp.WaitVisible(".card .override legend", chromedp.ByQuery),
		chromedp.Text("h1", &heading, chromedp.ByQuery),
		// textContent, not Text: the legend is CSS text-transform:uppercase, so
		// innerText would report the visually upper-cased form.
		chromedp.Evaluate(`document.querySelector(".card .override legend").textContent`, &legend),
		chromedp.Evaluate(`Array.from(document.querySelectorAll(".card .zone")).map(node => node.textContent)`, &zoneNames),
	); err != nil {
		t.Fatal(err)
	}
	if heading != "Fűtés" {
		t.Errorf("h1 = %q, want Hungarian \"Fűtés\"", heading)
	}
	if legend != "Ideiglenes felülbírálás" {
		t.Errorf("override legend = %q, want Hungarian", legend)
	}
	got := strings.Join(zoneNames, ", ")
	for _, want := range []string{"Hálószoba", "Nappali", "Gyerekszoba"} {
		if !strings.Contains(got, want) {
			t.Errorf("zone %q not rendered in Hungarian; got %q", want, got)
		}
	}
}

// TestThermostatUILanguagePicker drives the #lang dropdown: selecting Magyar
// must persist the choice and reload to a URL without ?lang=, so the page comes
// back localized from the stored preference rather than the query param. The
// change is fired from a deferred timer so the Evaluate returns before the
// reload tears the execution context down (otherwise chromedp reports it as an
// error); the subsequent waits run against the freshly loaded page.
func TestThermostatUILanguagePicker(t *testing.T) {
	ctx := newBrowserCtx(t)
	srv := serveThermostatUI(t)

	var langBefore, langAfter, heading string
	var zoneNames []string
	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/?lang=en"),
		chromedp.WaitVisible(".card .zone", chromedp.ByQuery),
		chromedp.Value("#lang", &langBefore, chromedp.ByQuery),
		// Select Magyar exactly as a user would: set the value and fire change,
		// which stores "hu" and reloads to location.pathname (no ?lang=).
		chromedp.Evaluate(`setTimeout(() => {
			const picker = document.getElementById("lang");
			picker.value = "hu";
			picker.dispatchEvent(new Event("change"));
		}, 0)`, nil),
		chromedp.WaitVisible(".card .zone", chromedp.ByQuery),
		chromedp.Value("#lang", &langAfter, chromedp.ByQuery),
		chromedp.Text("h1", &heading, chromedp.ByQuery),
		chromedp.Evaluate(`Array.from(document.querySelectorAll(".card .zone")).map(node => node.textContent)`, &zoneNames),
	); err != nil {
		t.Fatal(err)
	}
	if langBefore != "en" {
		t.Errorf("initial picker value = %q, want en", langBefore)
	}
	if langAfter != "hu" {
		t.Errorf("picker value after switch = %q, want hu (preference not persisted)", langAfter)
	}
	if heading != "Fűtés" {
		t.Errorf("h1 after switch = %q, want Hungarian \"Fűtés\"", heading)
	}
	if got := strings.Join(zoneNames, ", "); !strings.Contains(got, "Hálószoba") {
		t.Errorf("zones not re-rendered in Hungarian after switch; got %q", got)
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
// and preserves tenth-of-a-degree precision: the number input is step="0.1", so
// a typed 21.3 must survive daysFromEntries → PUT → GET → entriesFromDays
// unrounded. Typing then clicking save blurs the input, firing its onchange
// (entry.temp = Number(value)) before save reads the entries.
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

// firstCardOverrideTemp reads the bedroom card's always-visible setpoint input.
const firstCardOverrideTemp = `parseFloat(document.querySelector(".card .stepper .val-input").value)`

// TestThermostatUIOverrideTempStepper exercises the target-temp stepper
// round-trip: the + and − buttons PUT /api/settings in tenth-degree steps and
// the returned state re-renders the displayed value. The 0.1° quantisation lives
// in the page (nudge rounds to the nearest 0.1), so only a browser test covers it.
func TestThermostatUIOverrideTempStepper(t *testing.T) {
	ctx := newBrowserCtx(t)
	srv := serveThermostatUI(t)

	var start, afterPlus, afterMinus float64
	settled := func(want float64) chromedp.QueryAction {
		return chromedp.Poll(firstCardOverrideTemp+` === `+strconv.FormatFloat(want, 'f', -1, 64),
			nil, chromedp.WithPollingTimeout(5*time.Second))
	}
	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/?lang=en"),
		chromedp.WaitVisible(".card .stepper .val-input", chromedp.ByQuery),
		chromedp.Evaluate(firstCardOverrideTemp, &start),
		// + raises the seeded 21.0 to 21.1; the re-render must reflect it.
		chromedp.Click(".card .stepper button:last-child", chromedp.ByQuery),
		settled(21.1),
		chromedp.Evaluate(firstCardOverrideTemp, &afterPlus),
		// − drops it back below the start to 20.9 (two effective steps).
		chromedp.Click(".card .stepper button:first-child", chromedp.ByQuery),
		settled(21),
		chromedp.Click(".card .stepper button:first-child", chromedp.ByQuery),
		settled(20.9),
		chromedp.Evaluate(firstCardOverrideTemp, &afterMinus),
	); err != nil {
		t.Fatal(err)
	}
	if start != 21 {
		t.Errorf("start override temp = %v, want 21", start)
	}
	if afterPlus != 21.1 {
		t.Errorf("after + = %v, want 21.1", afterPlus)
	}
	if afterMinus != 20.9 {
		t.Errorf("after two − = %v, want 20.9", afterMinus)
	}
}

// cardOrderJS reads the rendered card order as a list of zone ids.
const cardOrderJS = `Array.from(document.querySelectorAll(".card[data-zone]")).map(card => card.dataset.zone)`

// TestThermostatUIReorderPersists drives the drag-to-reorder gesture end to end:
// a real pointer drag (CDP mouse input, which fires pointer events) on the first
// card's grip down past the second card swaps the two, the new order is PUT to
// /api/order, and a fresh page load — standing in for another browser — comes
// back in the persisted order rather than the alphabetical default.
func TestThermostatUIReorderPersists(t *testing.T) {
	ctx := newBrowserCtx(t)
	srv := serveThermostatUI(t)

	// Pointer path: press on the first grip, then move so the dragged card's
	// centre passes the second card's centre by a small margin (so it swaps with
	// the second card but not a third).
	const coordJS = `(() => {
		const cards = document.querySelectorAll(".card[data-zone]");
		const grip = cards[0].querySelector(".grip").getBoundingClientRect();
		const c1 = cards[0].getBoundingClientRect();
		const c2 = cards[1].getBoundingClientRect();
		const gx = grip.left + grip.width / 2, gy = grip.top + grip.height / 2;
		return { gx, gy, targetY: gy + (c2.top + c2.height / 2) - (c1.top + c1.height / 2) + 6 };
	})()`

	var before []string
	var coords struct{ Gx, Gy, TargetY float64 }
	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/?lang=en"),
		chromedp.WaitVisible(".card[data-zone] .grip", chromedp.ByQuery),
		chromedp.Evaluate(cardOrderJS, &before),
		chromedp.Evaluate(coordJS, &coords),
	); err != nil {
		t.Fatal(err)
	}
	if len(before) < 2 {
		t.Fatalf("need at least 2 cards to reorder, got %v", before)
	}

	// Press the grip, walk the pointer down in steps so onDragMove sees the swap
	// threshold cross, then release.
	actions := []chromedp.Action{
		chromedp.MouseEvent(input.MousePressed, coords.Gx, coords.Gy, chromedp.ButtonType(input.Left)),
	}
	const steps = 8
	for i := 1; i <= steps; i++ {
		y := coords.Gy + (coords.TargetY-coords.Gy)*float64(i)/steps
		actions = append(actions, chromedp.MouseEvent(input.MouseMoved, coords.Gx, y))
	}
	actions = append(actions, chromedp.MouseEvent(input.MouseReleased, coords.Gx, coords.TargetY, chromedp.ButtonType(input.Left)))
	if err := chromedp.Run(ctx, actions...); err != nil {
		t.Fatal(err)
	}

	// The PUT response re-renders; wait for the first card to become the zone
	// that was second, then read the settled order.
	firstIsSwapped := `(() => { const card = document.querySelector(".card[data-zone]"); return card && card.dataset.zone; })() === ` + strconv.Quote(before[1])
	var after, reloaded []string
	if err := chromedp.Run(ctx,
		chromedp.Poll(firstIsSwapped, nil, chromedp.WithPollingTimeout(5*time.Second)),
		chromedp.Evaluate(cardOrderJS, &after),
		// A fresh navigation is a stand-in for another browser: the order must
		// come back from the backend, not reset to alphabetical.
		chromedp.Navigate(srv.URL+"/?lang=en"),
		chromedp.WaitVisible(".card[data-zone]", chromedp.ByQuery),
		chromedp.Evaluate(cardOrderJS, &reloaded),
	); err != nil {
		t.Fatal(err)
	}

	if len(after) != len(before) || after[0] != before[1] || after[1] != before[0] {
		t.Errorf("after drag order = %v, want first two of %v swapped", after, before)
	}
	if !reflect.DeepEqual(reloaded, after) {
		t.Errorf("reloaded order = %v, want persisted order %v", reloaded, after)
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
