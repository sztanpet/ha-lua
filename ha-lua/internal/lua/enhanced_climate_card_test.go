package lua

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"

	"github.com/sztanpet/ha-lua/cards"
)

// cardHarnessHTML loads the real card asset, then exposes a stub hass (a fake
// states map plus callApi/callService spies) and a few helpers so the chromedp
// test can drive the card without a Home Assistant frontend. §10.8.
const cardHarnessHTML = `<!doctype html>
<html><head><meta charset="utf-8"></head><body>
<script src="/card.js"></script>
<script>
  window.__calls = { api: [], service: [], ws: [] };
  window.__mkHass = (language, states) => ({
    language: language,
    states: states,
    callApi: (method, path, data) => { window.__calls.api.push({ method, path, data }); return Promise.resolve({}); },
    callService: (domain, service, data) => { window.__calls.service.push({ domain, service, data }); return Promise.resolve(); },
    callWS: (msg) => { window.__calls.ws.push(msg); return Promise.resolve({}); },
  });
  window.__card = document.createElement("ha-lua-enhanced-climate-card");
  window.__card.setConfig({ climate_entity: "climate.lr" });
  document.body.appendChild(window.__card);
  window.__apply = (language, states) => { window.__card.hass = window.__mkHass(language, states); return true; };
  window.__shadow = (sel) => (window.__card.shadowRoot ? window.__card.shadowRoot.querySelector(sel) : null);
  window.__text = (sel) => { const el = window.__shadow(sel); return el ? el.textContent : null; };
  window.__val = (sel) => { const el = window.__shadow(sel); return el ? el.value : null; };
  window.__clickAll = (sel, index) => { const els = window.__card.shadowRoot.querySelectorAll(sel); els[index].click(); return true; };
</script>
</body></html>`

// statesEN is a JSON object literal (valid JS) of the stub hass states: a heat
// climate plus its companion (controlled at 21, three override presets, one bound
// window, empty schedule).
const cardStates = `{
  "climate.lr": { "entity_id": "climate.lr", "state": "heat", "attributes": {
    "friendly_name": "Living Room", "current_temperature": 19.5, "temperature": 20,
    "min_temp": 7, "max_temp": 30, "target_temp_step": 0.5, "hvac_modes": ["off", "heat"] } },
  "sensor.ha_lua_enhanced_climate_lr": { "entity_id": "sensor.ha_lua_enhanced_climate_lr", "state": "21",
    "attributes": { "controlled": true, "override": { "active": false }, "override_temp": 23,
      "manual": { "active": false }, "window": { "sensors": ["binary_sensor.w1"], "open": false },
      "presets": [10, 30, 60], "min_temp": 7, "max_temp": 30, "schedule": {} } }
}`

func serveEnhancedCard(t *testing.T) *httptest.Server {
	t.Helper()
	cardJS, err := cards.FS.ReadFile("enhanced-climate-card.js")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/card.js", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/javascript")
		_, _ = w.Write(cardJS)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(cardHarnessHTML))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestEnhancedClimateCard drives the real card asset in a browser against a stub
// hass: it asserts the rendered DOM, that an override preset fires the right
// ha_lua_command and the target stepper the right climate service, that the card
// is optimism-free, that it reconciles from a new hass, and that it localizes.
func TestEnhancedClimateCard(t *testing.T) {
	ctx := newBrowserCtx(t)
	srv := serveEnhancedCard(t)

	var ok bool
	var status, target, firstPreset string

	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/"),
		chromedp.Evaluate(`window.__apply("en", `+cardStates+`)`, &ok),
		chromedp.Poll(`!!window.__shadow(".subtitle .status")`, &ok),
		chromedp.Evaluate(`window.__text(".subtitle .status")`, &status),
		chromedp.Evaluate(`window.__val(".stepper .value")`, &target),
		chromedp.Evaluate(`window.__text(".presets button")`, &firstPreset),
	); err != nil {
		t.Fatal(err)
	}
	if status != "on" {
		t.Errorf("status = %q, want on", status)
	}

	// The window state rides the subtitle (status line), not a separate row;
	// the seed has one bound, closed window.
	var windowText string
	if err := chromedp.Run(ctx,
		chromedp.Evaluate(`window.__text(".subtitle .window")`, &windowText),
	); err != nil {
		t.Fatal(err)
	}
	if windowText != "window closed" {
		t.Errorf("subtitle window = %q, want \"window closed\"", windowText)
	}

	// An active manual hold shows the held badge with an explanatory tooltip
	// (title attribute) — the badge text alone doesn't say what a hold is.
	heldStates := strings.Replace(cardStates, `"manual": { "active": false }`,
		`"manual": { "active": true, "until": "2026-07-02T04:30:00" }`, 1)
	var heldTitle string
	if err := chromedp.Run(ctx,
		chromedp.Evaluate(`window.__apply("en", `+heldStates+`)`, &ok),
		chromedp.Poll(`!!window.__shadow(".badge.held")`, &ok),
		chromedp.Evaluate(`window.__shadow(".badge.held").getAttribute("title")`, &heldTitle),
		chromedp.Evaluate(`window.__apply("en", `+cardStates+`)`, &ok), // restore
		chromedp.Poll(`!window.__shadow(".badge.held")`, &ok),
	); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(heldTitle, "changed outside this card") {
		t.Errorf("held badge tooltip = %q, want the manual-hold explanation", heldTitle)
	}

	// formatClock honours the HA profile's time format: 24h yields no AM/PM
	// and an HH:MM shape regardless of the en language default.
	var clock24, clock12 string
	if err := chromedp.Run(ctx,
		chromedp.Evaluate(`window.__card.constructor.pure.formatClock({language:"en",locale:{time_format:"24"}}, "2026-07-02T04:30:00")`, &clock24),
		chromedp.Evaluate(`window.__card.constructor.pure.formatClock({language:"en",locale:{time_format:"12"}}, "2026-07-02T04:30:00")`, &clock12),
	); err != nil {
		t.Fatal(err)
	}
	if clock24 != "04:30" {
		t.Errorf("formatClock 24h = %q, want 04:30", clock24)
	}
	if !strings.Contains(clock12, "AM") {
		t.Errorf("formatClock 12h = %q, want an AM time", clock12)
	}
	if target != "20" {
		t.Errorf("target value = %q, want 20", target)
	}
	if firstPreset != "10m" {
		t.Errorf("first preset = %q, want 10m (no + sign)", firstPreset)
	}

	// Both steppers inherit the device's target_temp_step (0.5 in the seed); the
	// override-temp stepper (the 2nd .stepper) is no longer a hardcoded step.
	var overrideStep string
	if err := chromedp.Run(ctx,
		chromedp.Evaluate(`window.__card.shadowRoot.querySelectorAll(".stepper .value")[1].getAttribute("step")`, &overrideStep),
	); err != nil {
		t.Fatal(err)
	}
	if overrideStep != "0.5" {
		t.Errorf("override-temp step = %q, want 0.5 (device target_temp_step)", overrideStep)
	}

	// Grid sizing is declared (no resize warning) but not pinned full-width, so
	// the user's own layout options win.
	var gridCols, gridRows string
	if err := chromedp.Run(ctx,
		chromedp.Evaluate(`String(window.__card.getGridOptions().columns)`, &gridCols),
		chromedp.Evaluate(`String(window.__card.getGridOptions().rows)`, &gridRows),
	); err != nil {
		t.Fatal(err)
	}
	if gridCols == "full" {
		t.Error("getGridOptions pins columns:full, forcing full-width")
	}
	if gridRows != "auto" {
		t.Errorf("getGridOptions rows = %q, want auto", gridRows)
	}

	// Mode renders as buttons (like HA's own card), not a <select>; clicking the
	// first one (off) calls set_hvac_mode.
	var modeBtns int
	var hasSelect bool
	if err := chromedp.Run(ctx,
		chromedp.Evaluate(`window.__card.shadowRoot.querySelectorAll(".modes .mode-btn").length`, &modeBtns),
		chromedp.Evaluate(`!!window.__shadow("select.mode")`, &hasSelect),
	); err != nil {
		t.Fatal(err)
	}
	if hasSelect {
		t.Error("mode still renders a <select>; want buttons")
	}
	if modeBtns != 2 {
		t.Errorf("mode buttons = %d, want 2 (off, heat)", modeBtns)
	}
	// Each mode button names the mode via its title (tooltip) / aria-label, not
	// visible text.
	var modeTitle string
	if err := chromedp.Run(ctx,
		chromedp.Evaluate(`window.__shadow(".modes .mode-btn").getAttribute("title")`, &modeTitle),
	); err != nil {
		t.Fatal(err)
	}
	if modeTitle != "Off" {
		t.Errorf("first mode button title = %q, want Off", modeTitle)
	}
	var modeCalls string
	if err := chromedp.Run(ctx,
		chromedp.Evaluate(`window.__clickAll(".modes .mode-btn", 0)`, &ok),
		chromedp.Evaluate(`JSON.stringify(window.__calls.service)`, &modeCalls),
	); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"service":"set_hvac_mode"`, `"hvac_mode":"off"`} {
		if !strings.Contains(modeCalls, want) {
			t.Errorf("mode button did not call %s; service calls = %s", want, modeCalls)
		}
	}

	// Click the first override preset -> override command for 10 minutes, fired
	// over the websocket (fire_event), not a REST fetch.
	var cmdCalls string
	if err := chromedp.Run(ctx,
		chromedp.Evaluate(`window.__clickAll(".presets button", 0)`, &ok),
		chromedp.Evaluate(`JSON.stringify(window.__calls.ws)`, &cmdCalls),
	); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"event_type":"ha_lua_command"`, `"action":"override"`, `"minutes":10`, `"climate_entity":"climate.lr"`} {
		if !strings.Contains(cmdCalls, want) {
			t.Errorf("override preset did not fire %s; ws calls = %s", want, cmdCalls)
		}
	}
	// And it must NOT use a REST fetch (that fetch is what tripped Firefox's
	// Local Network Access and dropped the live websocket).
	var apiCalls string
	if err := chromedp.Run(ctx,
		chromedp.Evaluate(`JSON.stringify(window.__calls.api)`, &apiCalls),
	); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(apiCalls, "ha_lua_command") {
		t.Errorf("command went over REST fetch, want websocket only; api calls = %s", apiCalls)
	}

	// Click the target stepper's + (second .step in the first stepper) -> native
	// set_temperature at 20.5.
	var serviceCalls string
	if err := chromedp.Run(ctx,
		chromedp.Evaluate(`window.__clickAll(".stepper .step", 1)`, &ok),
		chromedp.Evaluate(`JSON.stringify(window.__calls.service)`, &serviceCalls),
		chromedp.Evaluate(`window.__val(".stepper .value")`, &target),
	); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"domain":"climate"`, `"service":"set_temperature"`, `"temperature":20.5`} {
		if !strings.Contains(serviceCalls, want) {
			t.Errorf("target + did not call %s; service calls = %s", want, serviceCalls)
		}
	}
	// Optimism-free: the spy did not update hass, so the field still shows the
	// server value (20), not an optimistic 20.5.
	if target != "20" {
		t.Errorf("target value after click = %q, want 20 (optimism-free)", target)
	}

	// Reconcile from a fresh hass: the target follows the new server value.
	reconciled := strings.Replace(cardStates, `"temperature": 20`, `"temperature": 22`, 1)
	if err := chromedp.Run(ctx,
		chromedp.Evaluate(`window.__apply("en", `+reconciled+`)`, &ok),
		chromedp.Poll(`window.__val(".stepper .value") === "22"`, &ok),
	); err != nil {
		t.Fatal(err)
	}

	// Localize: Hungarian translates the override fieldset legend ("Felülbírálás").
	var label string
	if err := chromedp.Run(ctx,
		chromedp.Evaluate(`window.__apply("hu", `+cardStates+`)`, &ok),
		chromedp.Poll(`window.__text("legend") === "Felülbírálás"`, &ok),
		chromedp.Evaluate(`window.__text("legend")`, &label),
	); err != nil {
		t.Fatal(err)
	}
	if label != "Felülbírálás" {
		t.Errorf("hu override legend = %q, want Felülbírálás", label)
	}

	// Override durations are visible even when the card configured none (fallback
	// presets), plus a custom-duration button.
	noPresets := strings.Replace(cardStates, `"presets": [10, 30, 60]`, `"presets": []`, 1)
	var presetCount int
	var hasCustom bool
	if err := chromedp.Run(ctx,
		chromedp.Evaluate(`window.__apply("en", `+noPresets+`)`, &ok),
		chromedp.Poll(`!!window.__shadow(".presets .custom")`, &ok),
		chromedp.Evaluate(`window.__card.shadowRoot.querySelectorAll(".presets button").length`, &presetCount),
		chromedp.Evaluate(`!!window.__shadow(".presets .custom")`, &hasCustom),
	); err != nil {
		t.Fatal(err)
	}
	if !hasCustom {
		t.Error("override has no custom-duration button")
	}
	if presetCount < 4 { // 3 fallback durations + custom
		t.Errorf("default override presets = %d, want >= 4 (defaults + custom)", presetCount)
	}

	// Inline schedule: today's periods render read-only (two per day here)
	// without opening the editor.
	day := `[{"time":"06:00","temp":21},{"time":"22:00","temp":18}]`
	days := make([]string, 0, 7)
	for i := 0; i < 7; i++ {
		days = append(days, fmt.Sprintf("%q:%s", strconv.Itoa(i), day))
	}
	schedStates := strings.Replace(cardStates, `"schedule": {}`, `"schedule": {`+strings.Join(days, ",")+`}`, 1)
	var periods int
	if err := chromedp.Run(ctx,
		chromedp.Evaluate(`window.__apply("en", `+schedStates+`)`, &ok),
		chromedp.Poll(`window.__card.shadowRoot.querySelectorAll(".today .period").length === 2`, &ok),
		chromedp.Evaluate(`window.__card.shadowRoot.querySelectorAll(".today .period").length`, &periods),
	); err != nil {
		t.Fatal(err)
	}
	if periods != 2 {
		t.Errorf("today periods = %d, want 2", periods)
	}

	// Open the schedule editor and save -> schedule command.
	if err := chromedp.Run(ctx,
		chromedp.Evaluate(`(window.__shadow(".edit-schedule").click(), true)`, &ok),
		chromedp.Poll(`!!window.__shadow(".editor .save")`, &ok),
		chromedp.Evaluate(`(window.__shadow(".editor .save").click(), true)`, &ok),
		chromedp.Evaluate(`JSON.stringify(window.__calls.ws)`, &cmdCalls),
	); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cmdCalls, `"action":"schedule"`) {
		t.Errorf("schedule save did not fire a schedule command; ws calls = %s", cmdCalls)
	}

	// Render skipping: HA pushes hass on EVERY state change in the install and
	// keeps unchanged state objects reference-identical. A push where neither
	// this climate, its companion, nor the language changed must NOT rebuild
	// the DOM (marker survives); a push with a changed climate object must
	// (marker gone). Fresh JSON literals give new references, so the same-refs
	// push reuses the states object already on the card.
	awaitPromise := func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithAwaitPromise(true) }
	// Two rAFs flush any scheduled render before/after the probe pushes.
	const flushRAF = `new Promise(r => requestAnimationFrame(() => requestAnimationFrame(() => r(true))))`
	var markerAlive, markerGone bool
	if err := chromedp.Run(ctx,
		chromedp.Evaluate(`window.__apply("en", `+cardStates+`)`, &ok),
		chromedp.Evaluate(flushRAF, &ok, awaitPromise),
		chromedp.Evaluate(`(window.__shadow("ha-card").__marker = true, true)`, &ok),
		chromedp.Evaluate(`(window.__card.hass = window.__mkHass("en", window.__card._hass.states), true)`, &ok),
		chromedp.Evaluate(flushRAF, &ok, awaitPromise),
		chromedp.Evaluate(`!!window.__shadow("ha-card").__marker`, &markerAlive),
	); err != nil {
		t.Fatal(err)
	}
	if !markerAlive {
		t.Error("irrelevant hass push rebuilt the DOM; want render skipped")
	}
	if err := chromedp.Run(ctx,
		chromedp.Evaluate(`window.__apply("en", `+strings.Replace(cardStates, `"temperature": 20`, `"temperature": 23`, 1)+`)`, &ok),
		chromedp.Poll(`!window.__shadow("ha-card").__marker`, &ok),
		chromedp.Evaluate(`!window.__shadow("ha-card").__marker`, &markerGone),
	); err != nil {
		t.Fatal(err)
	}
	if !markerGone {
		t.Error("relevant hass push did not re-render")
	}

	// Radiator temp is display-only card config: with radiator_entity set and a
	// numeric sensor the subtitle gains a "rad. X°" segment. A push where ONLY
	// the radiator sensor changed must re-render too (same-reference states for
	// everything else), i.e. the radiator is a relevant entity.
	var radiatorText string
	if err := chromedp.Run(ctx,
		chromedp.Evaluate(`(() => {
			window.__card.setConfig({ climate_entity: "climate.lr", radiator_entity: "sensor.rad" });
			const states = `+cardStates+`;
			states["sensor.rad"] = { entity_id: "sensor.rad", state: "47.5", attributes: {} };
			return window.__apply("en", states);
		})()`, &ok),
		chromedp.Poll(`window.__text(".subtitle .radiator") === "rad. 47.5°"`, &ok),
		chromedp.Evaluate(`(() => {
			const states = Object.assign({}, window.__card._hass.states);
			states["sensor.rad"] = { entity_id: "sensor.rad", state: "51", attributes: {} };
			window.__card.hass = window.__mkHass("en", states);
			return true;
		})()`, &ok),
		chromedp.Poll(`window.__text(".subtitle .radiator") === "rad. 51°"`, &ok),
		chromedp.Evaluate(`window.__text(".subtitle .radiator")`, &radiatorText),
	); err != nil {
		t.Fatal(err)
	}
	if radiatorText != "rad. 51°" {
		t.Errorf("radiator segment = %q, want rad. 51°", radiatorText)
	}

	// A non-numeric radiator state (unavailable sensor) hides the segment
	// instead of rendering junk.
	if err := chromedp.Run(ctx,
		chromedp.Evaluate(`(() => {
			const states = Object.assign({}, window.__card._hass.states);
			states["sensor.rad"] = { entity_id: "sensor.rad", state: "unavailable", attributes: {} };
			window.__card.hass = window.__mkHass("en", states);
			return true;
		})()`, &ok),
		chromedp.Poll(`!window.__shadow(".subtitle .radiator")`, &ok),
	); err != nil {
		t.Fatal(err)
	}
}

// TestEnhancedClimateCardConfigureNoStorm proves configure is fire-once and
// cannot flood the event API. HA rebuilds and re-pushes the card constantly; here
// we simulate that with 50 hass updates and assert configure is sent exactly once
// per distinct config (tracked module-side so it survives element recreation),
// regardless of whether the companion's state matches.
func TestEnhancedClimateCardConfigureNoStorm(t *testing.T) {
	ctx := newBrowserCtx(t)
	srv := serveEnhancedCard(t)

	var ok bool
	var first, afterChange int
	countConfigures := `window.__calls.ws.filter(m => m.event_type === "ha_lua_command" && m.event_data && m.event_data.action === "configure").length`

	// 50 hass updates with one config must send configure exactly once.
	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/"),
		chromedp.Evaluate(`for (let i = 0; i < 50; i++) window.__apply("en", `+cardStates+`); true`, &ok),
		chromedp.Evaluate(countConfigures, &first),
	); err != nil {
		t.Fatal(err)
	}
	if first != 1 {
		t.Errorf("50 hass updates sent %d configure POSTs; want exactly 1", first)
	}

	// A real config change sends exactly one more — not one per hass update.
	if err := chromedp.Run(ctx,
		chromedp.Evaluate(`window.__calls.ws = []; window.__card.setConfig(`+
			`{ climate_entity: "climate.lr", window_sensors: ["binary_sensor.w1"] }); true`, &ok),
		chromedp.Evaluate(`for (let i = 0; i < 50; i++) window.__apply("en", `+cardStates+`); true`, &ok),
		chromedp.Evaluate(countConfigures, &afterChange),
	); err != nil {
		t.Fatal(err)
	}
	if afterChange != 1 {
		t.Errorf("config change + 50 hass updates sent %d configure POSTs; want exactly 1", afterChange)
	}
}

// TestEnhancedClimateCardConfigureTwoCardsNoStorm reproduces the editor storm:
// two cards for the SAME climate entity but DIFFERENT configs (the saved
// dashboard card behind the edit dialog, plus the editor preview in it) both
// receive every hass push. With the old per-entity guard each saw the other's
// hash and re-sent configure on every push, ping-ponging forever and flooding
// the event API. Keying the guard by (entity, config) bounds it to exactly one
// send per distinct config: two cards, two configs -> two configures, no storm.
func TestEnhancedClimateCardConfigureTwoCardsNoStorm(t *testing.T) {
	ctx := newBrowserCtx(t)
	srv := serveEnhancedCard(t)

	var ok bool
	var configures int
	countConfigures := `window.__calls.ws.filter(m => m.event_type === "ha_lua_command" && m.event_data && m.event_data.action === "configure").length`

	// Build a second card for climate.lr with a different config (a window
	// sensor), then push 50 hass updates to BOTH cards, interleaved.
	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/"),
		chromedp.Evaluate(`
			window.__card2 = document.createElement("ha-lua-enhanced-climate-card");
			window.__card2.setConfig({ climate_entity: "climate.lr", window_sensors: ["binary_sensor.w1"] });
			document.body.appendChild(window.__card2);
			window.__calls.ws = [];
			for (let i = 0; i < 50; i++) {
				window.__card.hass = window.__mkHass("en", `+cardStates+`);
				window.__card2.hass = window.__mkHass("en", `+cardStates+`);
			}
			true`, &ok),
		chromedp.Evaluate(countConfigures, &configures),
	); err != nil {
		t.Fatal(err)
	}
	if configures != 2 {
		t.Errorf("two cards (same entity, two configs) + 50 hass updates each sent %d configure POSTs; want exactly 2", configures)
	}
}

// TestEnhancedClimateCardPreviewNoConfigure proves the editor preview never
// writes to the daemon. HA assigns hass BEFORE preview when it builds the
// element, so this mirrors that order (setConfig, hass, then preview=true) and
// asserts that once the deferred configure runs it sees preview and sends
// nothing — provisioning is reserved for the real, saved dashboard card.
func TestEnhancedClimateCardPreviewNoConfigure(t *testing.T) {
	ctx := newBrowserCtx(t)
	srv := serveEnhancedCard(t)

	var ok bool
	var configures int
	// Count configures for the preview's distinct entity so the harness card's
	// own provisioning can never be mistaken for the preview's.
	countConfigures := `window.__calls.ws.filter(m => m.event_type === "ha_lua_command" && m.event_data && m.event_data.action === "configure" && m.event_data.data && m.event_data.data.climate_entity === "climate.preview").length`

	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/"),
		// Build a preview element exactly as HA does: setConfig, then hass, then
		// the preview flag, all synchronously. The card defers its configure to a
		// microtask, which drains at the end of this Evaluate — after preview is
		// set — so by the next Evaluate it has had its chance to (not) fire.
		chromedp.Evaluate(`
			const preview = document.createElement("ha-lua-enhanced-climate-card");
			preview.setConfig({ climate_entity: "climate.preview", window_sensors: ["binary_sensor.w1"] });
			document.body.appendChild(preview);
			preview.hass = window.__mkHass("en", `+cardStates+`);
			preview.preview = true;
			true`, &ok),
		chromedp.Evaluate(countConfigures, &configures),
	); err != nil {
		t.Fatal(err)
	}
	if configures != 0 {
		t.Errorf("editor preview sent %d configure POSTs; want 0 (preview must not provision the daemon)", configures)
	}
}

// TestEnhancedClimateCardPendingSpinner proves a daemon round-trip gets
// immediate feedback: pressing an override preset shows a spinner and disables
// the buttons right away (a click pushes no hass, so without this the UI would
// sit dead until the server answered), and the next companion push that confirms
// the command clears it.
func TestEnhancedClimateCardPendingSpinner(t *testing.T) {
	ctx := newBrowserCtx(t)
	srv := serveEnhancedCard(t)

	var ok, hasSpinner, btnDisabled, cleared bool
	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/"),
		chromedp.Evaluate(`window.__apply("en", `+cardStates+`)`, &ok),
		chromedp.Poll(`!!window.__shadow(".presets button")`, &ok),
		// A click pushes no hass; the spinner must appear synchronously anyway.
		chromedp.Evaluate(`window.__clickAll(".presets button", 0)`, &ok),
		chromedp.Evaluate(`!!window.__shadow(".presets .spinner")`, &hasSpinner),
		chromedp.Evaluate(`window.__shadow(".presets button").disabled`, &btnDisabled),
	); err != nil {
		t.Fatal(err)
	}
	if !hasSpinner {
		t.Error("no spinner shown after pressing an override preset")
	}
	if !btnDisabled {
		t.Error("override buttons not disabled while the command is pending")
	}

	// The companion push that confirms the override (now active) clears the
	// spinner — the feedback is honest, tied to the server, not a fixed timer.
	active := strings.Replace(cardStates,
		`"override": { "active": false }`,
		`"override": { "active": true, "expires": "2099-01-01T00:00:00+00:00" }`, 1)
	if err := chromedp.Run(ctx,
		chromedp.Evaluate(`window.__apply("en", `+active+`)`, &ok),
		chromedp.Poll(`!window.__card.shadowRoot.querySelector(".spinner")`, &cleared),
	); err != nil {
		t.Fatal(err)
	}
	if !cleared {
		t.Error("spinner did not clear after the companion confirmed the command")
	}
}
