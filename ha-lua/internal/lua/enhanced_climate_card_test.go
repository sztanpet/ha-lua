package lua

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

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
  window.__calls = { api: [], service: [] };
  window.__mkHass = (language, states) => ({
    language: language,
    states: states,
    callApi: (method, path, data) => { window.__calls.api.push({ method, path, data }); return Promise.resolve({}); },
    callService: (domain, service, data) => { window.__calls.service.push({ domain, service, data }); return Promise.resolve(); },
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
		chromedp.Poll(`!!window.__shadow(".badge.status")`, &ok),
		chromedp.Evaluate(`window.__text(".badge.status")`, &status),
		chromedp.Evaluate(`window.__val(".stepper .value")`, &target),
		chromedp.Evaluate(`window.__text(".presets button")`, &firstPreset),
	); err != nil {
		t.Fatal(err)
	}
	if status != "on" {
		t.Errorf("status badge = %q, want on", status)
	}
	if target != "20" {
		t.Errorf("target value = %q, want 20", target)
	}
	if firstPreset != "+10m" {
		t.Errorf("first preset = %q, want +10m", firstPreset)
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

	// Click the first override preset -> override command for 10 minutes.
	var apiCalls string
	if err := chromedp.Run(ctx,
		chromedp.Evaluate(`window.__clickAll(".presets button", 0)`, &ok),
		chromedp.Evaluate(`JSON.stringify(window.__calls.api)`, &apiCalls),
	); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"events/ha_lua_command"`, `"action":"override"`, `"minutes":10`, `"climate_entity":"climate.lr"`} {
		if !strings.Contains(apiCalls, want) {
			t.Errorf("override preset did not fire %s; api calls = %s", want, apiCalls)
		}
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

	// Localize: Hungarian translates the target label ("Cél").
	var label string
	if err := chromedp.Run(ctx,
		chromedp.Evaluate(`window.__apply("hu", `+cardStates+`)`, &ok),
		chromedp.Poll(`window.__text(".row .label") === "Cél"`, &ok),
		chromedp.Evaluate(`window.__text(".row .label")`, &label),
	); err != nil {
		t.Fatal(err)
	}
	if label != "Cél" {
		t.Errorf("hu target label = %q, want Cél", label)
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
		chromedp.Evaluate(`JSON.stringify(window.__calls.api)`, &apiCalls),
	); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(apiCalls, `"action":"schedule"`) {
		t.Errorf("schedule save did not fire a schedule command; api calls = %s", apiCalls)
	}
}
