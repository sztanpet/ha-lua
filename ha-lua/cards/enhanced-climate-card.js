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
// Covered here: lifecycle, header (name + current temp · mode, held badge), the
// climate-native controls (target stepper + HVAC mode icon buttons on one row),
// and the enhanced controls (override presets + live countdown + cancel,
// override-temp stepper, window indicator, 7-day schedule editor), all with
// i18n. Every button shares the one `.btn` style (see STYLES). The config editor
// follows.

const VERSION = "0.3.17";

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
    "overriding_to": "overriding to {temp}°",
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
    "overriding_to": "felülbírálás {temp}°-ra",
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
    let str = table[key] ?? MESSAGES.en[key] ?? fallback ?? key;
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

// Fallback override durations (minutes) shown when the card config sets no
// presets, so the buttons are always there to tap. The daemon accepts any
// 1..1440, so these are just suggestions.
const DEFAULT_PRESETS = [10, 30, 60];

// HVAC mode -> mdi icon, mirroring Home Assistant's own climate card so the
// mode buttons read the same. Modes without an entry fall back to a text label.
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
  // Greedily collapse a transition's days into the largest matching groups
  // (everyday > weekdays > weekend, ordered largest-first in DAY_GROUPS), then
  // emit whatever single days are left over.
  const multiDayGroups = DAY_GROUPS.filter((group) => group.days.length > 1);
  const entries = [];
  for (const key of order) {
    const info = byTransition.get(key);
    const remaining = new Set(info.presentDays);
    for (const group of multiDayGroups) {
      if (group.days.every((day) => remaining.has(day))) {
        entries.push({ group: group.value, time: info.time, temp: info.temp });
        group.days.forEach((day) => remaining.delete(day));
      }
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
  .heading { flex: 1; min-width: 0; display: flex; flex-direction: column; gap: 2px; }
  .title { color: var(--ha-card-header-color, var(--primary-text-color));
    font-family: var(--ha-card-header-font-family, inherit);
    font-size: var(--ha-card-header-font-size, var(--ha-font-size-2xl));
    letter-spacing: -0.012em; line-height: var(--ha-line-height-expanded);
    font-weight: var(--ha-font-weight-normal); }
  .subtitle { display: flex; align-items: center; gap: 8px; font-size: .9rem;
    color: var(--secondary-text-color); }
  .subtitle .divider { width: 1px; height: 12px; background: var(--divider-color, #ccc); }
  .badge { font-size: .72rem; padding: 2px 8px; border-radius: 10px; white-space: nowrap;
    background: color-mix(in oklch, var(--primary-color) 16%, transparent); color: var(--primary-color); }
  .badge.held { background: color-mix(in oklch, var(--warning-color, #ffa600) 22%, transparent);
    color: var(--warning-color, #ffa600); }
  .content { display: flex; flex-direction: column; gap: 12px; }
  .row { display: flex; align-items: center; justify-content: space-between; gap: 10px; }
  .label { color: var(--secondary-text-color); }
  .stepper { display: flex; align-items: center; }
  .stepper .value { width: 72px; height: 44px; box-sizing: border-box; text-align: center;
    font-size: 1.15rem; padding: 6px 14px; border: 1px solid var(--divider-color, #ccc);
    border-left: none; border-right: none; border-radius: 0;
    background: var(--card-background-color); color: var(--primary-text-color); }
  /* The ± buttons and the value read as one pill: shared background, no
     internal dividers, only the two outer corners rounded. */
  .stepper .btn.step { background: var(--card-background-color); }
  .stepper .btn.step:first-child { border-right: none; border-radius: 12px 0 0 12px; }
  .stepper .btn.step:last-child { border-left: none; border-radius: 0 12px 12px 0; }
  /* space-between pushes the mode icons to the right edge when they share the
     stepper's row; once they wrap to their own line it leaves them left-aligned. */
  .climate-controls { display: flex; flex-wrap: wrap; align-items: center;
    justify-content: space-between; gap: 10px 16px; }
  .modes { display: flex; gap: 6px; flex-wrap: wrap; }

  /* One button look for every control; modifiers only tweak it. */
  .btn { min-width: 44px; height: 44px; padding: 0 12px; display: inline-flex; align-items: center;
    justify-content: center; gap: 6px; border: 1px solid var(--divider-color, #ccc); border-radius: 12px;
    background: transparent; color: var(--primary-text-color); font: inherit; cursor: pointer; }
  .btn:hover { background: color-mix(in oklch, var(--primary-text-color) 8%, transparent); }
  .btn.icon, .btn.step { width: 44px; padding: 0; }
  .btn.icon { color: var(--secondary-text-color); }
  .btn.icon ha-icon { --mdc-icon-size: 24px; }
  .btn.step { font-size: 1.3rem; }
  .btn.active { background: var(--mode-color, var(--primary-color));
    border-color: var(--mode-color, var(--primary-color)); color: var(--text-primary-color, #fff); }
  .btn.primary { background: var(--primary-color); border-color: var(--primary-color);
    color: var(--text-primary-color, #fff); }
  .btn.primary:hover { background: color-mix(in oklch, var(--primary-color) 88%, black); }
  .btn.ghost { border-style: dashed; color: var(--secondary-text-color); align-self: flex-start; }
  .btn.link { border: none; min-width: 0; width: auto; height: auto; padding: 6px; }
  .btn.danger { color: var(--error-color, #db4437); }

  .notice, .hint { color: var(--secondary-text-color); }
  .enhanced { display: flex; flex-direction: column; gap: 12px; }
  /* Each section is a fieldset; its legend rides the border as the title, which
     saves the whole height of a separate heading row. */
  fieldset.group { border: 1px solid var(--divider-color, #ccc); border-radius: 10px; margin: 0;
    padding: 4px 12px 10px; display: flex; flex-direction: column; gap: 8px; min-width: 0; }
  fieldset.group legend { padding: 0 6px; font-size: .78rem; font-weight: 600; letter-spacing: .04em;
    text-transform: uppercase; color: var(--secondary-text-color); }
  .override-controls { display: flex; align-items: center; gap: 8px 12px; flex-wrap: wrap; }
  .overriding { display: flex; align-items: center; gap: 12px; flex-wrap: wrap; }
  .sched-line { display: flex; align-items: center; justify-content: space-between; gap: 10px; }
  .today { display: flex; flex-wrap: wrap; gap: 6px 12px; font-size: .92rem;
    color: var(--secondary-text-color); }
  .today.muted { font-style: italic; }
  .today .period.now { color: var(--primary-color); font-weight: 700; }
  .presets { display: flex; gap: 6px; flex-wrap: wrap; }
  .countdown { font-variant-numeric: tabular-nums; font-weight: 600; }
  .window.open { color: var(--warning-color, #ffa600); }
  .window.closed { color: var(--secondary-text-color); }
  .editor { display: flex; flex-direction: column; gap: 8px; }
  .editor-row { display: flex; align-items: center; gap: 6px; }
  .editor-row select, .editor-row input { padding: 6px; border-radius: 6px;
    border: 1px solid var(--divider-color, #ccc); background: var(--card-background-color);
    color: var(--primary-text-color); font: inherit; }
  .editor-row select { flex: 1; min-width: 0; }
  .editor-row input[type="time"] { width: 88px; }
  .editor-row input[type="number"] { width: 64px; }
  .editor-actions { display: flex; gap: 8px; }
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
      columns: 12,
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
      action,
      data: { climate_entity: this._config.climate_entity, ...data },
    });
  }

  callClimate(service, data) {
    if (!this._hass) return;
    this._hass.callService("climate", service, { entity_id: this._config.climate_entity, ...data });
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

    // Title = name, with a subtitle that pairs the current temperature and the
    // current mode/status side by side, split by a thin divider.
    const heading = h("div", { class: "heading" }, h("div", { class: "title" }, name));
    const subtitle = h("div", { class: "subtitle" });
    if (Number.isFinite(Number(attrs.current_temperature))) {
      subtitle.append(h("span", { class: "current-temp" }, attrs.current_temperature + "°"));
      subtitle.append(h("span", { class: "divider", "aria-hidden": "true" }));
    }
    subtitle.append(h("span", { class: "status" }, statusLabel(translate, mode, hvacAction)));
    heading.append(subtitle);
    const header = h("div", { class: "header" }, heading);
    if (companionAttrs && companionAttrs.manual && companionAttrs.manual.active && companionAttrs.manual.until) {
      const clock = formatClock(hass.language, companionAttrs.manual.until);
      if (clock) header.append(h("span", { class: "badge held" }, translate("held_until", { time: clock })));
    }
    root.append(header);

    const content = h("div", { class: "content" });
    // Target stepper and the mode buttons share one row, wrapping to two lines
    // only when there isn't room; labels are dropped (the stepper keeps an
    // aria-label, each mode button a title) to keep it tight.
    // Every temperature input steps by the device's target_temp_step, falling
    // back to 0.1 when the device advertises none.
    const tempStep = Number(attrs.target_temp_step) || 0.1;
    const target = this._stepperControl(translate, {
      label: translate("target"),
      value: attrs.temperature,
      lo: Number(attrs.min_temp),
      hi: Number(attrs.max_temp),
      step: tempStep,
      onCommit: (value) => this.callClimate("set_temperature", { temperature: value }),
    });
    content.append(h("div", { class: "climate-controls" }, target, this._renderMode(translate, attrs, mode)));

    if (companionAttrs) {
      content.append(this._renderEnhanced(translate, companionAttrs, tempStep));
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

  // _stepperControl is the bare ± / typed numeric control (.stepper), clamped to
  // [lo, hi] and committed through onCommit. lastSent (per render) dedupes no-op
  // writes; the focused field suppresses the hass-driven re-render. opts.label
  // is used as the input's aria-label so the control is named even with no
  // visible label beside it.
  _stepperControl(translate, opts) {
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
      "aria-label": opts.label,
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
      class: "btn step", type: "button", "aria-label": translate("decrease"),
      onmousedown: (ev) => ev.preventDefault(),
      onclick: () => commit(base() - opts.step),
    }, "−");
    const plus = h("button", {
      class: "btn step", type: "button", "aria-label": translate("increase"),
      onmousedown: (ev) => ev.preventDefault(),
      onclick: () => commit(base() + opts.step),
    }, "+");

    return h("div", { class: "stepper" }, minus, input, plus);
  }

  // _renderMode draws the HVAC modes as rounded icon buttons (like HA's own
  // climate card) rather than a dropdown; the active mode is filled with its
  // state colour. The mode name is the button's title/aria-label (tooltip + screen
  // reader), not visible text. Modes without a known icon fall back to the name.
  // Returns the bare .modes group so it can sit next to the target stepper.
  _renderMode(translate, attrs, mode) {
    const modes = Array.isArray(attrs.hvac_modes) ? attrs.hvac_modes : [];
    if (modes.length === 0) return h("span", {});
    const buttons = modes.map((hvacMode) => {
      const active = hvacMode === mode;
      const label = translate("mode." + hvacMode, null, hvacMode);
      const icon = MODE_ICONS[hvacMode];
      return h("button", {
        class: "btn icon mode-btn" + (active ? " active" : ""),
        type: "button",
        title: label,
        "aria-label": label,
        style: active ? `--mode-color: var(--state-climate-${hvacMode}-color, var(--primary-color))` : null,
        onclick: () => this.callClimate("set_hvac_mode", { hvac_mode: hvacMode }),
      }, icon ? h("ha-icon", { icon }) : label); // fall back to the text when no icon
    });
    return h("div", { class: "modes" }, ...buttons);
  }

  // _renderEnhanced builds the daemon-driven controls from the companion as
  // fieldsets (legend = title): the override fieldset (durations/countdown +
  // override-temp stepper sharing one row, like thermostat.html), an optional
  // window row, and the schedule fieldset.
  _renderEnhanced(translate, companionAttrs, tempStep) {
    const section = h("div", { class: "enhanced" });

    const overrideTemp = this._stepperControl(translate, {
      label: translate("override_temp"),
      value: companionAttrs.override_temp,
      lo: Number(companionAttrs.min_temp),
      hi: Number(companionAttrs.max_temp),
      step: tempStep,
      onCommit: (value) => this.fireCommand("settings", { override_temp: value }),
    });
    section.append(h("fieldset", { class: "group" },
      h("legend", null, translate("override")),
      h("div", { class: "override-controls" },
        this._renderOverride(translate, companionAttrs), overrideTemp)));

    const windowInfo = companionAttrs.window;
    if (windowInfo && Array.isArray(windowInfo.sensors) && windowInfo.sensors.length > 0) {
      section.append(h("div", { class: "row" },
        h("span", { class: "label" }, translate("window")),
        h("span", { class: "window " + (windowInfo.open ? "open" : "closed") },
          translate(windowInfo.open ? "window.open" : "window.closed"))));
    }

    section.append(this._renderScheduleGroup(translate, companionAttrs, tempStep));
    return section;
  }

  // _renderOverride shows the duration buttons, or — while an override is active
  // — a live countdown, "overriding to X°", and a cancel button (like
  // thermostat.html). Sits inside the override fieldset next to the temp stepper.
  _renderOverride(translate, companionAttrs) {
    const override = companionAttrs.override;
    if (override && override.active && override.expires) {
      return h("div", { class: "overriding" },
        h("span", { class: "countdown", "data-expires": override.expires },
          formatCountdown(remainingSeconds(override.expires))),
        h("span", null, translate("overriding_to", { temp: companionAttrs.override_temp })),
        h("button", { class: "btn", type: "button",
          onclick: () => this.fireCommand("override", { cancel: true }) }, translate("stop_override")));
    }
    const configured = Array.isArray(companionAttrs.presets) ? companionAttrs.presets : [];
    const presets = configured.length ? configured : DEFAULT_PRESETS;
    const buttons = presets.map((minutes) => h("button", { class: "btn", type: "button",
      onclick: () => this.fireCommand("override", { minutes: Number(minutes) }) }, minutes + "m"));
    // A custom-duration button: prompt for an arbitrary minute count.
    const custom = h("button", {
      class: "btn custom", type: "button",
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
  // heading, and the full editor below once opened. With no schedule set the
  // group stays a single heading line — the "no schedule set" text becomes the
  // heading's tooltip rather than an extra row.
  _renderScheduleGroup(translate, companionAttrs, tempStep) {
    const open = this._editorOpen;
    const { periods, nowIndex } = open
      ? { periods: [], nowIndex: -1 }
      : todayPeriods(companionAttrs.schedule || {}, new Date());
    const empty = !open && periods.length === 0;

    // Legend carries the "no schedule set" explanation as a tooltip when empty,
    // so an empty schedule stays a single line.
    const fieldset = h("fieldset", { class: "group" },
      h("legend", { title: empty ? translate("no_schedule") : null }, translate("schedule")));

    if (open) {
      fieldset.append(this._renderEditor(translate));
      return fieldset;
    }
    const today = empty
      ? h("span", { class: "today muted" }, "—")
      : h("div", { class: "today" }, ...periods.map((period, index) =>
        h("span", {
          class: "period" + (index === nowIndex ? " now" : ""),
          title: index === nowIndex ? translate("now_period") : null,
        }, period.time + " " + period.temp + "°")));
    fieldset.append(h("div", { class: "sched-line" }, today,
      h("button", { class: "btn edit-schedule", type: "button",
        onclick: () => this._openEditor(companionAttrs, tempStep) }, translate("edit_schedule"))));
    return fieldset;
  }

  _openEditor(companionAttrs, tempStep) {
    this._editorEntries = entriesFromSchedule(companionAttrs.schedule || {});
    this._editorBounds = [Number(companionAttrs.min_temp), Number(companionAttrs.max_temp)];
    this._editorStep = tempStep;
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
    editor.append(h("button", { class: "btn ghost add", type: "button", onclick: () => {
      entries.push({ group: "weekdays", time: "07:00", temp: clampNumber(21, lo, hi) });
      this._renderNow();
    } }, translate("add_entry")));
    editor.append(h("div", { class: "editor-actions" },
      h("button", { class: "btn primary save", type: "button", onclick: () => {
        this.fireCommand("schedule", { schedule: scheduleFromEntries(entries) });
        this._closeEditor();
      } }, translate("save")),
      h("button", { class: "btn cancel", type: "button", onclick: () => this._closeEditor() }, translate("cancel"))));
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
      type: "number", step: String(this._editorStep || 0.1),
      min: Number.isFinite(lo) ? String(lo) : null,
      max: Number.isFinite(hi) ? String(hi) : null,
      value: String(entry.temp),
      onchange: (ev) => { entry.temp = clampNumber(Number(ev.target.value), lo, hi); ev.target.value = String(entry.temp); },
    });
    const remove = h("button", { class: "btn link danger rm", type: "button",
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

    // Like the climate picker: set .label and append directly. Wrapping a
    // multi-input picker in a <label> (invalid: a label binds one control)
    // swallowed clicks on its add/remove buttons, so selections never stuck.
    const windowPicker = document.createElement("ha-entities-picker");
    windowPicker.hass = this._hass;
    windowPicker.value = this._config.window_sensors || [];
    windowPicker.includeDomains = ["binary_sensor"];
    windowPicker.label = translate("editor.window_sensors");
    windowPicker.addEventListener("value-changed", (ev) => this._update({ window_sensors: ev.detail.value }));
    form.append(windowPicker);

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
