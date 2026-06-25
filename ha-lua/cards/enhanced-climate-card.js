// ha-lua Enhanced Climate card
//
// A single self-contained vanilla custom element (no build step, no runtime
// imports) for the enhanced_climate.lua controller. It mirrors the daemon's
// companion sensor (sensor.ha_lua_enhanced_climate_<slug>) plus the native
// climate entity, and provisions the enhanced climate by firing ha_lua_command.
// See enhanced-climate-spec.md §10.
//
// Add it as a dashboard resource of type "module" pointing at
//   /local/ha-lua/enhanced-climate-card.js
//
// Covered here: lifecycle, header (status + held badges), the climate-native
// controls (target stepper + HVAC mode), and the enhanced controls (override
// presets + live countdown + cancel, override-temp stepper, window indicator,
// 7-day schedule editor), all with i18n. The config editor follows.

const VERSION = "0.3.7";

console.info(
  `%c ha-lua-enhanced-climate-card %c v${VERSION} `,
  "color: white; background: #03a9f4; font-weight: 700;",
  "color: #03a9f4; background: white; font-weight: 700;",
);

// ---------------------------------------------------------------------------
// i18n. The card reads HA's user language from hass.language; missing keys fall
// back to English. All user-visible text goes through a translator.
// ---------------------------------------------------------------------------

const MESSAGES = {
  en: {
    "status.on": "on",
    "status.heating": "heating",
    "status.off": "off",
    "held_until": "held until {time}",
    "setting_up": "Setting up…",
    "unavailable": "{name} is unavailable",
    "current": "Current",
    "target": "Target",
    "mode": "Mode",
    "decrease": "Decrease",
    "increase": "Increase",
    "mode.heat": "Heat",
    "mode.off": "Off",
    "mode.cool": "Cool",
    "mode.auto": "Auto",
    "mode.heat_cool": "Heat / Cool",
    "mode.dry": "Dry",
    "mode.fan_only": "Fan",
    "override": "Override",
    "override_temp": "Override target",
    "stop_override": "Stop",
    "custom_minutes": "Custom minutes",
    "custom_minutes_prompt": "Override for how many minutes?",
    "window": "Window",
    "window.open": "open",
    "window.closed": "closed",
    "schedule": "Schedule",
    "edit_schedule": "Edit",
    "no_schedule": "no schedule set",
    "now_period": "currently active period",
    "add_entry": "Add entry",
    "save": "Save",
    "cancel": "Cancel",
    "daygroup.combined": "Groups",
    "daygroup.individual": "Days",
    "day.everyday": "Every day",
    "day.weekdays": "Mon–Fri",
    "day.weekend": "Sat–Sun",
    "day.0": "Monday",
    "day.1": "Tuesday",
    "day.2": "Wednesday",
    "day.3": "Thursday",
    "day.4": "Friday",
    "day.5": "Saturday",
    "day.6": "Sunday",
    "editor.climate": "Climate entity (required)",
    "editor.window_sensors": "Window sensors",
    "editor.presets": "Override presets (minutes)",
    "editor.name": "Name",
  },
  hu: {
    "status.on": "on", // the English word, as in the Ingress UI
    "status.heating": "fűtés",
    "status.off": "ki",
    "held_until": "{time}-ig tartva",
    "setting_up": "Beállítás…",
    "unavailable": "{name} nem elérhető",
    "current": "Jelenlegi",
    "target": "Cél",
    "mode": "Mód",
    "decrease": "Csökkentés",
    "increase": "Növelés",
    "mode.heat": "Fűtés",
    "mode.off": "Ki",
    "mode.cool": "Hűtés",
    "mode.auto": "Auto",
    "mode.heat_cool": "Fűtés / Hűtés",
    "mode.dry": "Párátlanítás",
    "mode.fan_only": "Ventilátor",
    "override": "Felülbírálás",
    "override_temp": "Felülbírálás cél",
    "stop_override": "Leállítás",
    "custom_minutes": "Egyéni időtartam",
    "custom_minutes_prompt": "Hány percig legyen felülbírálva?",
    "window": "Ablak",
    "window.open": "nyitva",
    "window.closed": "zárva",
    "schedule": "Ütemezés",
    "edit_schedule": "Szerkesztés",
    "no_schedule": "nincs beállított ütemezés",
    "now_period": "jelenleg aktív időszak",
    "add_entry": "Új bejegyzés",
    "save": "Mentés",
    "cancel": "Mégse",
    "daygroup.combined": "Csoportok",
    "daygroup.individual": "Napok",
    "day.everyday": "Minden nap",
    "day.weekdays": "Hétfő–Péntek",
    "day.weekend": "Szombat–Vasárnap",
    "day.0": "Hétfő",
    "day.1": "Kedd",
    "day.2": "Szerda",
    "day.3": "Csütörtök",
    "day.4": "Péntek",
    "day.5": "Szombat",
    "day.6": "Vasárnap",
    "editor.climate": "Klíma entitás (kötelező)",
    "editor.window_sensors": "Ablakérzékelők",
    "editor.presets": "Felülbírálás gombok (perc)",
    "editor.name": "Név",
  },
};

function makeTranslator(language) {
  const lang = (language || "en").toLowerCase().slice(0, 2);
  const table = MESSAGES[lang] || MESSAGES.en;
  return function translate(key, params, fallback) {
    let str = table[key];
    if (str == null) str = MESSAGES.en[key];
    if (str == null) str = fallback != null ? fallback : key;
    if (params) {
      str = str.replace(/\{(\w+)\}/g, (whole, name) => (params[name] != null ? params[name] : whole));
    }
    return str;
  };
}

// ---------------------------------------------------------------------------
// Pure helpers (unit-testable without a browser; exposed on the element class
// as a static for the chromedp harness).
// ---------------------------------------------------------------------------

// Each schedule editor entry targets one of these day groups; .days lists the
// 0=Mon..6=Sun indices the entry expands to on save.
const DAY_GROUPS = [
  { value: "everyday", days: [0, 1, 2, 3, 4, 5, 6] },
  { value: "weekdays", days: [0, 1, 2, 3, 4] },
  { value: "weekend", days: [5, 6] },
  { value: "0", days: [0] },
  { value: "1", days: [1] },
  { value: "2", days: [2] },
  { value: "3", days: [3] },
  { value: "4", days: [4] },
  { value: "5", days: [5] },
  { value: "6", days: [6] },
];

// HVAC mode -> mdi icon, mirroring Home Assistant's own climate card so the
// mode buttons read the same. Modes without an entry fall back to a text label.
// Fallback override durations (minutes) shown when the card config sets no
// presets, so the buttons are always there to tap. The daemon accepts any
// 1..1440, so these are just suggestions.
const DEFAULT_PRESETS = [10, 30, 60];

const MODE_ICONS = {
  off: "mdi:power",
  heat: "mdi:fire",
  cool: "mdi:snowflake",
  heat_cool: "mdi:sun-snowflake-variant",
  auto: "mdi:calendar-sync",
  dry: "mdi:water-percent",
  fan_only: "mdi:fan",
};

function slugOf(climateEntity) {
  return climateEntity.replace(/^climate\./, "");
}

function companionId(climateEntity) {
  return "sensor.ha_lua_enhanced_climate_" + slugOf(climateEntity);
}

function statusLabel(translate, mode, hvacAction) {
  if (hvacAction === "heating") return translate("status.heating");
  if (mode === "heat") return translate("status.on");
  return translate("status.off");
}

function clampNumber(value, lo, hi) {
  if (Number.isFinite(lo) && value < lo) return lo;
  if (Number.isFinite(hi) && value > hi) return hi;
  return value;
}

function configHash(config) {
  return JSON.stringify({
    climate_entity: config.climate_entity || "",
    window_sensors: config.window_sensors || [],
    presets: config.presets || [],
  });
}

function formatClock(language, isoTime) {
  const date = new Date(isoTime);
  if (isNaN(date.getTime())) return "";
  return date.toLocaleTimeString(language || undefined, { hour: "2-digit", minute: "2-digit" });
}

function remainingSeconds(isoExpires) {
  const end = new Date(isoExpires).getTime();
  if (isNaN(end)) return 0;
  return Math.max(0, Math.round((end - Date.now()) / 1000));
}

function formatCountdown(seconds) {
  const total = Math.max(0, Math.floor(seconds));
  const hours = Math.floor(total / 3600);
  const mins = Math.floor((total % 3600) / 60);
  const secs = total % 60;
  const pad = (value) => String(value).padStart(2, "0");
  return hours > 0 ? `${hours}:${pad(mins)}:${pad(secs)}` : `${pad(mins)}:${pad(secs)}`;
}

// entriesFromSchedule collapses the companion's per-day schedule into editor
// entries, reusing the every-day / Mon–Fri / Sat–Sun groups whenever a
// transition is shared across all of their days.
function entriesFromSchedule(schedule) {
  const byTransition = new Map();
  const order = [];
  for (let day = 0; day < 7; day++) {
    const list = schedule && Array.isArray(schedule[String(day)]) ? schedule[String(day)] : [];
    for (const transition of list) {
      const key = transition.time + "|" + transition.temp;
      if (!byTransition.has(key)) {
        byTransition.set(key, { time: transition.time, temp: transition.temp, presentDays: new Set() });
        order.push(key);
      }
      byTransition.get(key).presentDays.add(day);
    }
  }
  const entries = [];
  for (const key of order) {
    const info = byTransition.get(key);
    const remaining = new Set(info.presentDays);
    if ([0, 1, 2, 3, 4, 5, 6].every((day) => remaining.has(day))) {
      entries.push({ group: "everyday", time: info.time, temp: info.temp });
      remaining.clear();
    }
    if ([0, 1, 2, 3, 4].every((day) => remaining.has(day))) {
      entries.push({ group: "weekdays", time: info.time, temp: info.temp });
      [0, 1, 2, 3, 4].forEach((day) => remaining.delete(day));
    }
    if ([5, 6].every((day) => remaining.has(day))) {
      entries.push({ group: "weekend", time: info.time, temp: info.temp });
      [5, 6].forEach((day) => remaining.delete(day));
    }
    [...remaining].sort((a, b) => a - b).forEach((day) => entries.push({ group: String(day), time: info.time, temp: info.temp }));
  }
  entries.sort((a, b) => a.time.localeCompare(b.time));
  return entries;
}

// scheduleFromEntries expands editor entries back into the per-day payload the
// daemon expects ({ "0": [...], … }).
function scheduleFromEntries(entries) {
  const days = {};
  for (let day = 0; day < 7; day++) days[String(day)] = [];
  for (const entry of entries) {
    const group = DAY_GROUPS.find((candidate) => candidate.value === entry.group);
    if (!group) continue;
    for (const day of group.days) days[String(day)].push({ time: entry.time, temp: entry.temp });
  }
  for (let day = 0; day < 7; day++) days[String(day)].sort((a, b) => a.time.localeCompare(b.time));
  return days;
}

// todayPeriods derives today's schedule transitions (sorted by time) and the
// index of the one in effect right now from the per-day schedule and the local
// clock, so the card can show the running schedule inline (like thermostat.html)
// without the daemon publishing a separate "today" array. nowIndex is -1 before
// the day's first transition (yesterday's last period is carrying over).
function todayPeriods(schedule, now) {
  const dow = (now.getDay() + 6) % 7; // JS Sun=0 -> schedule Mon=0
  const list = schedule && Array.isArray(schedule[String(dow)]) ? schedule[String(dow)].slice() : [];
  list.sort((a, b) => String(a.time).localeCompare(String(b.time)));
  const hhmm = String(now.getHours()).padStart(2, "0") + ":" + String(now.getMinutes()).padStart(2, "0");
  let nowIndex = -1;
  for (let index = 0; index < list.length; index++) {
    if (String(list[index].time) <= hhmm) nowIndex = index;
    else break;
  }
  return { periods: list, nowIndex };
}

// Tiny hyperscript builder: h(tag, attrs?, ...children) -> DOM node.
function h(tag, attrs, ...children) {
  const el = document.createElement(tag);
  for (const key in attrs) {
    const val = attrs[key];
    if (val == null || val === false) continue;
    if (key === "class") el.className = val;
    else if (key.startsWith("on")) el[key.toLowerCase()] = val;
    else el.setAttribute(key, val);
  }
  for (const child of children.flat()) {
    if (child == null || child === false) continue;
    el.append(child.nodeType ? child : document.createTextNode(child));
  }
  return el;
}

const STYLES = `
  :host { display: block; }
  ha-card { padding: 16px; }
  .header { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; margin-bottom: 14px; }
  .title { font-size: 1.1rem; font-weight: 600; flex: 1; min-width: 0; }
  .badge { font-size: .72rem; padding: 2px 8px; border-radius: 10px; white-space: nowrap;
    background: color-mix(in oklch, var(--primary-color) 16%, transparent); color: var(--primary-color); }
  .badge.held { background: color-mix(in oklch, var(--warning-color, #ffa600) 22%, transparent);
    color: var(--warning-color, #ffa600); }
  .content { display: flex; flex-direction: column; gap: 12px; }
  .row { display: flex; align-items: center; justify-content: space-between; gap: 10px; }
  .label { color: var(--secondary-text-color); }
  .current { color: var(--secondary-text-color); font-size: .95rem; }
  .stepper { display: flex; align-items: center; gap: 6px; }
  .stepper .value { width: 64px; height: 42px; box-sizing: border-box; text-align: center;
    font-size: 1.15rem; padding: 6px 4px; border: 1px solid var(--divider-color, #ccc);
    border-radius: 8px; background: var(--card-background-color); color: var(--primary-text-color); }
  .stepper .unit { color: var(--secondary-text-color); }
  .step { width: 40px; height: 42px; border-radius: 8px; border: 1px solid var(--divider-color, #ccc);
    background: transparent; color: var(--primary-text-color); font-size: 1.3rem; cursor: pointer; }
  .step:hover { background: color-mix(in oklch, var(--primary-text-color) 8%, transparent); }
  .modes { display: flex; gap: 6px; flex-wrap: wrap; }
  .mode-btn { width: 40px; height: 40px; padding: 0; display: inline-flex; align-items: center;
    justify-content: center; border: 1px solid var(--divider-color, #ccc); border-radius: 50%;
    background: transparent; color: var(--secondary-text-color); cursor: pointer; }
  .mode-btn:hover { background: color-mix(in oklch, var(--primary-text-color) 8%, transparent); }
  .mode-btn.active { background: var(--mode-color, var(--primary-color));
    border-color: var(--mode-color, var(--primary-color)); color: var(--text-primary-color, #fff); }
  .mode-btn ha-icon { --mdc-icon-size: 22px; }
  .notice, .hint { color: var(--secondary-text-color); }
  .enhanced { display: flex; flex-direction: column; gap: 12px; }
  .group { border: 1px solid var(--divider-color, #ccc); border-radius: 10px; padding: 10px 12px;
    display: flex; flex-direction: column; gap: 10px; }
  .group-head { display: flex; align-items: center; justify-content: space-between; gap: 8px;
    font-size: .78rem; font-weight: 600; letter-spacing: .04em; text-transform: uppercase;
    color: var(--secondary-text-color); }
  .today { display: flex; flex-wrap: wrap; gap: 6px 12px; font-size: .92rem;
    color: var(--secondary-text-color); }
  .today .period.now { color: var(--primary-color); font-weight: 700; }
  .today.muted { font-style: italic; }
  .presets { display: flex; gap: 6px; flex-wrap: wrap; }
  button.override { border-radius: 999px; border: 1px solid var(--primary-color); background: transparent;
    color: var(--primary-color); padding: 6px 12px; font: inherit; cursor: pointer; }
  button.override:hover { background: color-mix(in oklch, var(--primary-color) 14%, transparent); }
  .override-active { display: flex; align-items: center; gap: 10px; }
  .countdown { font-variant-numeric: tabular-nums; font-weight: 600; }
  .window.open { color: var(--warning-color, #ffa600); }
  .window.closed { color: var(--secondary-text-color); }
  button.edit-schedule { border: 1px solid var(--divider-color, #ccc); background: transparent;
    color: var(--primary-text-color); border-radius: 8px; padding: 6px 12px; font: inherit; cursor: pointer; }
  .editor { display: flex; flex-direction: column; gap: 8px; }
  .editor-row { display: flex; align-items: center; gap: 6px; }
  .editor-row select, .editor-row input { padding: 6px; border-radius: 6px;
    border: 1px solid var(--divider-color, #ccc); background: var(--card-background-color);
    color: var(--primary-text-color); font: inherit; }
  .editor-row select { flex: 1; min-width: 0; }
  .editor-row input[type="time"] { width: 88px; }
  .editor-row input[type="number"] { width: 64px; }
  button.rm { border: none; background: transparent; color: var(--error-color, #db4437);
    cursor: pointer; font-size: 1rem; }
  button.add { align-self: flex-start; border: 1px dashed var(--divider-color, #ccc); background: transparent;
    color: var(--primary-text-color); border-radius: 8px; padding: 6px 12px; font: inherit; cursor: pointer; }
  .editor-actions { display: flex; gap: 8px; }
  button.save { border: none; background: var(--primary-color); color: white; border-radius: 8px;
    padding: 6px 14px; font: inherit; cursor: pointer; }
  button.cancel { border: 1px solid var(--divider-color, #ccc); background: transparent;
    color: var(--primary-text-color); border-radius: 8px; padding: 6px 14px; font: inherit; cursor: pointer; }
`;

class HaLuaEnhancedClimateCard extends HTMLElement {
  setConfig(config) {
    if (!config || !config.climate_entity) {
      throw new Error("enhanced-climate-card: climate_entity is required");
    }
    this._config = config;
    this._configHash = configHash(config);
    this._scheduleRender();
  }

  set hass(hass) {
    this._hass = hass;
    this._maybeConfigure();
    this._scheduleRender();
  }

  connectedCallback() {
    // A local 1s timer drives only the override countdown display; all data comes
    // from hass push, so there is no polling.
    this._countdownTimer = setInterval(() => this._tickCountdown(), 1000);
  }

  disconnectedCallback() {
    clearInterval(this._countdownTimer);
  }

  getCardSize() {
    return 5;
  }

  // getGridOptions declares sizing for the sections dashboard so HA stops
  // warning that the card "does not fully support resizing". rows is auto (the
  // body height varies with the schedule editor); columns is a plain default
  // span the user can freely resize. NOT columns:"full" — that pinned the card
  // full-width and overrode the user's own layout options.
  getGridOptions() {
    return {
      rows: "auto",
      columns: 6,
      min_columns: 3,
    };
  }

  static getStubConfig() {
    return { climate_entity: "" };
  }

  static getConfigElement() {
    return document.createElement("ha-lua-enhanced-climate-card-editor");
  }

  _maybeConfigure() {
    if (!this._hass || !this._config) return;
    if (this._configHash === this._sentConfigHash) return;
    this._sentConfigHash = this._configHash;
    this.fireCommand("configure", {
      window_sensors: this._config.window_sensors || [],
      presets: this._config.presets || [],
    });
  }

  fireCommand(action, data) {
    if (!this._hass) return;
    this._hass.callApi("POST", "events/ha_lua_command", {
      script: "enhanced_climate",
      action: action,
      data: Object.assign({ climate_entity: this._config.climate_entity }, data),
    });
  }

  callClimate(service, data) {
    if (!this._hass) return;
    this._hass.callService("climate", service, Object.assign({ entity_id: this._config.climate_entity }, data));
  }

  _scheduleRender() {
    if (this._renderQueued) return;
    this._renderQueued = true;
    requestAnimationFrame(() => {
      this._renderQueued = false;
      this._render();
    });
  }

  // _render is the hass-driven render; it is suppressed (optimism-free) while an
  // input is focused or the schedule editor is open so a server push can't yank
  // the work away. _renderNow forces a render for those local interactions.
  _render() {
    if (this._fieldFocused || this._editorOpen) return;
    this._renderNow();
  }

  _renderNow() {
    if (!this._config) return;
    if (!this.shadowRoot) this.attachShadow({ mode: "open" });

    const hass = this._hass;
    const translate = makeTranslator(hass && hass.language);
    const entity = this._config.climate_entity;
    const climate = hass && hass.states ? hass.states[entity] : null;

    const root = h("ha-card", {});
    if (!climate || climate.state === "unavailable") {
      root.append(h("div", { class: "content" },
        h("div", { class: "notice" }, translate("unavailable", { name: this._config.name || entity }))));
      this._replace(root);
      return;
    }

    const attrs = climate.attributes || {};
    const companion = hass.states[companionId(entity)];
    const companionAttrs = companion ? companion.attributes || {} : null;
    const mode = climate.state;
    const hvacAction = attrs.hvac_action;
    const name = this._config.name || attrs.friendly_name || entity;

    const header = h("div", { class: "header" },
      h("div", { class: "title" }, name),
      h("span", { class: "badge status" }, statusLabel(translate, mode, hvacAction)));
    if (companionAttrs && companionAttrs.manual && companionAttrs.manual.active && companionAttrs.manual.until) {
      const clock = formatClock(hass.language, companionAttrs.manual.until);
      if (clock) header.append(h("span", { class: "badge held" }, translate("held_until", { time: clock })));
    }
    root.append(header);

    const content = h("div", { class: "content" });
    if (Number.isFinite(Number(attrs.current_temperature))) {
      content.append(h("div", { class: "current" }, translate("current") + ": " + attrs.current_temperature + "°"));
    }
    content.append(this._stepper(translate, {
      label: translate("target"),
      value: attrs.temperature,
      lo: Number(attrs.min_temp),
      hi: Number(attrs.max_temp),
      step: Number(attrs.target_temp_step) || 0.5,
      onCommit: (value) => this.callClimate("set_temperature", { temperature: value }),
    }));
    content.append(this._renderMode(translate, attrs, mode));

    if (companionAttrs) {
      content.append(this._renderEnhanced(translate, companionAttrs));
    } else {
      content.append(h("div", { class: "hint" }, translate("setting_up")));
    }
    root.append(content);
    this._replace(root);
  }

  _replace(root) {
    this.shadowRoot.innerHTML = "";
    const style = document.createElement("style");
    style.textContent = STYLES;
    this.shadowRoot.append(style, root);
  }

  // _stepper is the shared ± / typed numeric control, clamped to [lo, hi] and
  // committed through onCommit. lastSent (per render) dedupes no-op writes; the
  // focused field suppresses the hass-driven re-render.
  _stepper(translate, opts) {
    const current = Number(opts.value);
    let lastSent = Number.isFinite(current) ? current : null;
    const commit = (raw) => {
      const parsed = Number(raw);
      if (!Number.isFinite(parsed)) return;
      const next = clampNumber(Math.round(parsed * 10) / 10, opts.lo, opts.hi);
      if (next === lastSent) return;
      lastSent = next;
      opts.onCommit(next);
    };
    const base = () => (lastSent != null ? lastSent : (Number.isFinite(opts.lo) ? opts.lo : 20));

    const input = h("input", {
      class: "value",
      type: "number",
      inputmode: "decimal",
      step: String(opts.step),
      min: Number.isFinite(opts.lo) ? String(opts.lo) : null,
      max: Number.isFinite(opts.hi) ? String(opts.hi) : null,
      value: Number.isFinite(current) ? String(current) : "",
      onfocus: () => { this._fieldFocused = true; },
      onblur: () => { this._fieldFocused = false; commit(input.value); },
      onkeydown: (ev) => {
        if (ev.key === "Enter") {
          input.blur();
        } else if (ev.key === "Escape") {
          input.value = Number.isFinite(current) ? String(current) : "";
          input.blur();
        }
      },
    });
    const minus = h("button", {
      class: "step", type: "button", "aria-label": translate("decrease"),
      onmousedown: (ev) => ev.preventDefault(),
      onclick: () => commit(base() - opts.step),
    }, "−");
    const plus = h("button", {
      class: "step", type: "button", "aria-label": translate("increase"),
      onmousedown: (ev) => ev.preventDefault(),
      onclick: () => commit(base() + opts.step),
    }, "+");

    return h("div", { class: "row" },
      h("span", { class: "label" }, opts.label),
      h("div", { class: "stepper" }, minus, input, h("span", { class: "unit" }, "°"), plus));
  }

  // _renderMode draws the HVAC modes as a row of round icon buttons (like HA's
  // own climate card) rather than a dropdown; the active mode is filled with its
  // state colour. Modes without a known icon fall back to their translated name.
  _renderMode(translate, attrs, mode) {
    const modes = Array.isArray(attrs.hvac_modes) ? attrs.hvac_modes : [];
    if (modes.length === 0) return h("span", {});
    const buttons = modes.map((hvacMode) => {
      const active = hvacMode === mode;
      const label = translate("mode." + hvacMode, null, hvacMode);
      const button = h("button", {
        class: "mode-btn" + (active ? " active" : ""),
        type: "button",
        title: label,
        "aria-label": label,
        style: active ? `--mode-color: var(--state-climate-${hvacMode}-color, var(--primary-color))` : null,
        onclick: () => this.callClimate("set_hvac_mode", { hvac_mode: hvacMode }),
      });
      const icon = MODE_ICONS[hvacMode];
      if (icon) {
        const haIcon = document.createElement("ha-icon");
        haIcon.setAttribute("icon", icon);
        button.append(haIcon);
      } else {
        button.append(label);
      }
      return button;
    });
    return h("div", { class: "row" },
      h("span", { class: "label" }, translate("mode")),
      h("div", { class: "modes" }, ...buttons));
  }

  // _renderEnhanced builds the daemon-driven controls from the companion as
  // distinct bordered groups: the override group (presets/countdown +
  // override-temp stepper), an optional window row, and the schedule group.
  _renderEnhanced(translate, companionAttrs) {
    const section = h("div", { class: "enhanced" });

    section.append(h("div", { class: "group" },
      h("div", { class: "group-head" }, translate("override")),
      this._renderOverride(translate, companionAttrs),
      this._stepper(translate, {
        label: translate("override_temp"),
        value: companionAttrs.override_temp,
        lo: Number(companionAttrs.min_temp),
        hi: Number(companionAttrs.max_temp),
        step: 0.5,
        onCommit: (value) => this.fireCommand("settings", { override_temp: value }),
      })));

    const windowInfo = companionAttrs.window;
    if (windowInfo && Array.isArray(windowInfo.sensors) && windowInfo.sensors.length > 0) {
      section.append(h("div", { class: "row" },
        h("span", { class: "label" }, translate("window")),
        h("span", { class: "window " + (windowInfo.open ? "open" : "closed") },
          translate(windowInfo.open ? "window.open" : "window.closed"))));
    }

    section.append(this._renderScheduleGroup(translate, companionAttrs));
    return section;
  }

  // _renderOverride shows the preset buttons, or — while an override is active —
  // a live countdown plus a cancel button. The enclosing group supplies the
  // "Override" heading, so no label row here.
  _renderOverride(translate, companionAttrs) {
    const override = companionAttrs.override;
    if (override && override.active && override.expires) {
      const countdown = h("span", { class: "countdown", "data-expires": override.expires },
        formatCountdown(remainingSeconds(override.expires)));
      const cancel = h("button", { class: "override", type: "button",
        onclick: () => this.fireCommand("override", { cancel: true }) }, translate("stop_override"));
      return h("div", { class: "override-active" }, countdown, cancel);
    }
    const configured = Array.isArray(companionAttrs.presets) ? companionAttrs.presets : [];
    const presets = configured.length ? configured : DEFAULT_PRESETS;
    const buttons = presets.map((minutes) => h("button", { class: "override", type: "button",
      onclick: () => this.fireCommand("override", { minutes: Number(minutes) }) }, "+" + minutes + "m"));
    // A custom-duration button: prompt for an arbitrary minute count.
    const custom = h("button", {
      class: "override custom", type: "button",
      title: translate("custom_minutes"), "aria-label": translate("custom_minutes"),
      onclick: () => {
        const raw = window.prompt(translate("custom_minutes_prompt"), "45");
        if (raw == null) return;
        const minutes = parseInt(raw, 10);
        if (Number.isFinite(minutes) && minutes > 0) this.fireCommand("override", { minutes });
      },
    }, "…");
    return h("div", { class: "presets" }, ...buttons, custom);
  }

  // _renderScheduleGroup shows today's running schedule inline (like
  // thermostat.html) with the active period highlighted, an Edit button in the
  // heading, and the full editor below once opened.
  _renderScheduleGroup(translate, companionAttrs) {
    const group = h("div", { class: "group" },
      h("div", { class: "group-head" },
        h("span", null, translate("schedule")),
        h("button", { class: "edit-schedule", type: "button",
          onclick: () => this._openEditor(companionAttrs) }, translate("edit_schedule"))));

    if (this._editorOpen) {
      group.append(this._renderEditor(translate));
      return group;
    }

    const { periods, nowIndex } = todayPeriods(companionAttrs.schedule || {}, new Date());
    if (periods.length === 0) {
      group.append(h("div", { class: "today muted" }, translate("no_schedule")));
    } else {
      group.append(h("div", { class: "today" }, ...periods.map((period, index) =>
        h("span", {
          class: "period" + (index === nowIndex ? " now" : ""),
          title: index === nowIndex ? translate("now_period") : null,
        }, period.time + " " + period.temp + "°"))));
    }
    return group;
  }

  _openEditor(companionAttrs) {
    this._editorEntries = entriesFromSchedule(companionAttrs.schedule || {});
    this._editorBounds = [Number(companionAttrs.min_temp), Number(companionAttrs.max_temp)];
    this._editorOpen = true;
    this._renderNow();
  }

  _closeEditor() {
    this._editorOpen = false;
    this._renderNow();
  }

  _renderEditor(translate) {
    const entries = this._editorEntries;
    const [lo, hi] = this._editorBounds;
    const editor = h("div", { class: "editor" });
    entries.forEach((entry, index) => editor.append(this._editorRow(translate, entries, index, lo, hi)));
    editor.append(h("button", { class: "add", type: "button", onclick: () => {
      entries.push({ group: "weekdays", time: "07:00", temp: clampNumber(21, lo, hi) });
      this._renderNow();
    } }, translate("add_entry")));
    editor.append(h("div", { class: "editor-actions" },
      h("button", { class: "save", type: "button", onclick: () => {
        this.fireCommand("schedule", { schedule: scheduleFromEntries(entries) });
        this._closeEditor();
      } }, translate("save")),
      h("button", { class: "cancel", type: "button", onclick: () => this._closeEditor() }, translate("cancel"))));
    return editor;
  }

  _editorRow(translate, entries, index, lo, hi) {
    const entry = entries[index];
    const combined = DAY_GROUPS.filter((group) => group.days.length > 1);
    const individual = DAY_GROUPS.filter((group) => group.days.length === 1);
    const option = (group) => {
      const node = h("option", { value: group.value }, translate("day." + group.value));
      if (group.value === entry.group) node.setAttribute("selected", "");
      return node;
    };
    const daySelect = h("select", { onchange: (ev) => { entry.group = ev.target.value; } },
      h("optgroup", { label: translate("daygroup.combined") }, ...combined.map(option)),
      h("optgroup", { label: translate("daygroup.individual") }, ...individual.map(option)));
    const time = h("input", { type: "time", value: entry.time,
      onchange: (ev) => { entry.time = ev.target.value; } });
    const temp = h("input", {
      type: "number", step: "0.1",
      min: Number.isFinite(lo) ? String(lo) : null,
      max: Number.isFinite(hi) ? String(hi) : null,
      value: String(entry.temp),
      onchange: (ev) => { entry.temp = clampNumber(Number(ev.target.value), lo, hi); ev.target.value = String(entry.temp); },
    });
    const remove = h("button", { class: "rm", type: "button",
      onclick: () => { entries.splice(index, 1); this._renderNow(); } }, "✕");
    return h("div", { class: "editor-row" }, daySelect, time, temp, remove);
  }

  // _tickCountdown updates only the override countdown text each second without a
  // full re-render; when it reaches zero it reconciles from the next push.
  _tickCountdown() {
    if (!this.shadowRoot) return;
    const el = this.shadowRoot.querySelector(".countdown[data-expires]");
    if (!el) return;
    const remaining = remainingSeconds(el.getAttribute("data-expires"));
    el.textContent = formatCountdown(remaining);
    if (remaining <= 0) this._scheduleRender();
  }
}

HaLuaEnhancedClimateCard.pure = {
  slugOf, companionId, statusLabel, clampNumber, configHash, formatClock,
  remainingSeconds, formatCountdown, entriesFromSchedule, scheduleFromEntries,
  todayPeriods, makeTranslator, MESSAGES, DAY_GROUPS,
};

// ---------------------------------------------------------------------------
// Config editor (§10.6). Uses HA's own ha-entity-picker / ha-entities-picker,
// which are undocumented frontend internals — this element works only inside a
// live HA frontend and may need adjusting across HA releases. It is NOT covered
// by the chromedp harness (which has no HA frontend). The only required field is
// the climate entity; everything else is optional.
// ---------------------------------------------------------------------------

class HaLuaEnhancedClimateCardEditor extends HTMLElement {
  setConfig(config) {
    this._config = Object.assign({}, config);
    this._render();
  }

  set hass(hass) {
    this._hass = hass;
    this._render();
  }

  _emit() {
    this.dispatchEvent(new CustomEvent("config-changed", {
      detail: { config: this._config }, bubbles: true, composed: true,
    }));
  }

  _update(patch) {
    this._config = Object.assign({}, this._config, patch);
    this._emit();
  }

  _render() {
    // HA sets hass before setConfig, so _render runs once with no config yet;
    // wait until both arrive before touching this._config.
    if (!this._hass || !this._config) return;
    if (!this.shadowRoot) this.attachShadow({ mode: "open" });
    // The pickers are live HA elements; rebuilding on every hass would tear down
    // a focused one, so build the form once.
    if (this._built) return;
    this._built = true;

    const translate = makeTranslator(this._hass.language);
    const style = document.createElement("style");
    style.textContent = `
      .form { display: flex; flex-direction: column; gap: 12px; padding: 8px 0; }
      label { display: flex; flex-direction: column; gap: 4px;
        color: var(--secondary-text-color); font-size: .85rem; }
      input { padding: 8px; border-radius: 6px; border: 1px solid var(--divider-color, #ccc);
        background: var(--card-background-color); color: var(--primary-text-color); font: inherit; }
    `;
    const form = h("div", { class: "form" });

    const climatePicker = document.createElement("ha-entity-picker");
    climatePicker.hass = this._hass;
    climatePicker.value = this._config.climate_entity || "";
    climatePicker.includeDomains = ["climate"];
    climatePicker.label = translate("editor.climate");
    climatePicker.required = true;
    climatePicker.addEventListener("value-changed", (ev) => this._update({ climate_entity: ev.detail.value }));
    form.append(climatePicker);

    const windowPicker = document.createElement("ha-entities-picker");
    windowPicker.hass = this._hass;
    windowPicker.value = this._config.window_sensors || [];
    windowPicker.includeDomains = ["binary_sensor"];
    windowPicker.addEventListener("value-changed", (ev) => this._update({ window_sensors: ev.detail.value }));
    form.append(h("label", {}, translate("editor.window_sensors"), windowPicker));

    const presetsInput = h("input", {
      type: "text", inputmode: "numeric",
      value: (this._config.presets || []).join(", "),
      placeholder: "10, 30, 60",
      onchange: (ev) => {
        const presets = ev.target.value.split(",")
          .map((part) => Number(part.trim()))
          .filter((minutes) => Number.isFinite(minutes) && minutes > 0);
        this._update({ presets });
      },
    });
    form.append(h("label", {}, translate("editor.presets"), presetsInput));

    const nameInput = h("input", {
      type: "text",
      value: this._config.name || "",
      onchange: (ev) => {
        const name = ev.target.value.trim();
        if (name) {
          this._update({ name });
        } else {
          delete this._config.name;
          this._emit();
        }
      },
    });
    form.append(h("label", {}, translate("editor.name"), nameInput));

    this.shadowRoot.innerHTML = "";
    this.shadowRoot.append(style, form);
  }
}

customElements.define("ha-lua-enhanced-climate-card", HaLuaEnhancedClimateCard);
customElements.define("ha-lua-enhanced-climate-card-editor", HaLuaEnhancedClimateCardEditor);

window.customCards = window.customCards || [];
window.customCards.push({
  type: "ha-lua-enhanced-climate-card",
  name: "ha-lua Enhanced Climate",
  description: "Schedule, override and window-aware control for a climate entity.",
  preview: true,
});
