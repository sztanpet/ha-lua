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
// This commit covers the lifecycle, the header (status + held badges), and the
// climate-native controls (target stepper + HVAC mode) driven by native climate
// services, plus i18n. The boost/override-temp/schedule editor and the config
// editor are layered on in the following commits.

const VERSION = "0.2.0";

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
  },
};

// makeTranslator returns a t(key, params?, fallback?) bound to a language, with
// English fallback and {var} substitution.
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

function slugOf(climateEntity) {
  return climateEntity.replace(/^climate\./, "");
}

function companionId(climateEntity) {
  return "sensor.ha_lua_enhanced_climate_" + slugOf(climateEntity);
}

// statusLabel maps the climate mode + hvac_action to a badge word: "heating"
// while the device is calling for heat, "on" in heat mode, else "off".
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

// configHash is the effective config the daemon cares about; the card re-fires
// configure whenever it changes.
function configHash(config) {
  return JSON.stringify({
    climate_entity: config.climate_entity || "",
    window_sensors: config.window_sensors || [],
    presets: config.presets || [],
  });
}

// formatClock renders an ISO timestamp as HH:MM in the user's locale, or ""
// when unparseable.
function formatClock(language, isoTime) {
  const date = new Date(isoTime);
  if (isNaN(date.getTime())) return "";
  return date.toLocaleTimeString(language || undefined, { hour: "2-digit", minute: "2-digit" });
}

// Tiny hyperscript builder: h(tag, attrs?, ...children) -> DOM node. `class`
// sets className, on* set handlers, null/false children/attrs are skipped, text
// children become escaped text nodes.
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
  .stepper .value { width: 64px; text-align: center; font-size: 1.15rem; padding: 6px 4px;
    border: 1px solid var(--divider-color, #ccc); border-radius: 8px;
    background: var(--card-background-color); color: var(--primary-text-color); }
  .stepper .unit { color: var(--secondary-text-color); }
  .step { width: 40px; height: 42px; border-radius: 8px; border: 1px solid var(--divider-color, #ccc);
    background: transparent; color: var(--primary-text-color); font-size: 1.3rem; cursor: pointer; }
  .step:hover { background: color-mix(in oklch, var(--primary-text-color) 8%, transparent); }
  select.mode { padding: 8px; border-radius: 8px; border: 1px solid var(--divider-color, #ccc);
    background: var(--card-background-color); color: var(--primary-text-color); font: inherit; }
  .notice, .hint { color: var(--secondary-text-color); }
`;

class HaLuaEnhancedClimateCard extends HTMLElement {
  // setConfig validates and stashes the dashboard YAML. climate_entity is the
  // only required field; HA renders an error card if it is missing.
  setConfig(config) {
    if (!config || !config.climate_entity) {
      throw new Error("enhanced-climate-card: climate_entity is required");
    }
    this._config = config;
    this._configHash = configHash(config);
    this._scheduleRender();
  }

  // hass is pushed on every state change; provision on config change, then
  // re-render (rAF-coalesced).
  set hass(hass) {
    this._hass = hass;
    this._maybeConfigure();
    this._scheduleRender();
  }

  getCardSize() {
    return 5;
  }

  static getStubConfig() {
    return { climate_entity: "" };
  }

  // _maybeConfigure fires configure whenever the effective config changed since
  // the last send (first hass, a later editor change, or a re-mount). Driven
  // from hass, not setConfig, because callApi needs hass.
  _maybeConfigure() {
    if (!this._hass || !this._config) return;
    if (this._configHash === this._sentConfigHash) return;
    this._sentConfigHash = this._configHash;
    this.fireCommand("configure", {
      window_sensors: this._config.window_sensors || [],
      presets: this._config.presets || [],
    });
  }

  // fireCommand posts an ha_lua_command event addressed to enhanced_climate.
  fireCommand(action, data) {
    if (!this._hass) return;
    this._hass.callApi("POST", "events/ha_lua_command", {
      script: "enhanced_climate",
      action: action,
      data: Object.assign({ climate_entity: this._config.climate_entity }, data),
    });
  }

  // callClimate calls a native climate service on the configured entity.
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

  _replace(root) {
    this.shadowRoot.innerHTML = "";
    const style = document.createElement("style");
    style.textContent = STYLES;
    this.shadowRoot.append(style, root);
  }

  _render() {
    if (!this._config) return;
    if (!this.shadowRoot) this.attachShadow({ mode: "open" });
    // Optimism-free: while an input is focused, skip the re-render so server
    // pushes can't yank the field away mid-edit (commit happens on blur/Enter).
    if (this._busy) return;

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
    content.append(this._renderTarget(translate, attrs));
    content.append(this._renderMode(translate, attrs, mode));
    if (!companion) {
      content.append(h("div", { class: "hint" }, translate("setting_up")));
    }
    root.append(content);
    this._replace(root);
  }

  // _renderTarget is the native target-temperature stepper: ± nudge and a typed
  // input, both clamped to the device range and committed through
  // climate.set_temperature. lastSent (per render) dedupes no-op writes.
  _renderTarget(translate, attrs) {
    const lo = Number(attrs.min_temp);
    const hi = Number(attrs.max_temp);
    const step = Number(attrs.target_temp_step) || 0.5;
    const current = Number(attrs.temperature);
    let lastSent = Number.isFinite(current) ? current : null;

    const commit = (raw) => {
      const parsed = Number(raw);
      if (!Number.isFinite(parsed)) return;
      const value = clampNumber(Math.round(parsed * 10) / 10, lo, hi);
      if (value === lastSent) return;
      lastSent = value;
      this.callClimate("set_temperature", { temperature: value });
    };
    const base = () => (lastSent != null ? lastSent : (Number.isFinite(lo) ? lo : 20));

    const input = h("input", {
      class: "value",
      type: "number",
      inputmode: "decimal",
      step: String(step),
      min: Number.isFinite(lo) ? String(lo) : null,
      max: Number.isFinite(hi) ? String(hi) : null,
      value: Number.isFinite(current) ? String(current) : "",
      onfocus: () => { this._busy = true; },
      onblur: () => { this._busy = false; commit(input.value); },
      onkeydown: (ev) => {
        if (ev.key === "Enter") {
          input.blur();
        } else if (ev.key === "Escape") {
          input.value = Number.isFinite(current) ? String(current) : "";
          input.blur();
        }
      },
    });
    // preventDefault on mousedown keeps the input focused (no blur-commit mid
    // click) for users typing then nudging.
    const minus = h("button", {
      class: "step", type: "button", "aria-label": translate("decrease"),
      onmousedown: (ev) => ev.preventDefault(),
      onclick: () => commit(base() - step),
    }, "−");
    const plus = h("button", {
      class: "step", type: "button", "aria-label": translate("increase"),
      onmousedown: (ev) => ev.preventDefault(),
      onclick: () => commit(base() + step),
    }, "+");

    return h("div", { class: "row" },
      h("span", { class: "label" }, translate("target")),
      h("div", { class: "stepper" }, minus, input, h("span", { class: "unit" }, "°"), plus));
  }

  // _renderMode is the native HVAC mode selector, built from the entity's own
  // hvac_modes and committed through climate.set_hvac_mode.
  _renderMode(translate, attrs, mode) {
    const modes = Array.isArray(attrs.hvac_modes) ? attrs.hvac_modes : [];
    if (modes.length === 0) return h("span", {});
    const select = h("select", {
      class: "mode",
      onchange: () => this.callClimate("set_hvac_mode", { hvac_mode: select.value }),
    });
    for (const hvacMode of modes) {
      const option = h("option", { value: hvacMode }, translate("mode." + hvacMode, null, hvacMode));
      if (hvacMode === mode) option.setAttribute("selected", "");
      select.append(option);
    }
    return h("div", { class: "row" },
      h("span", { class: "label" }, translate("mode")),
      select);
  }
}

// Expose the pure helpers for the chromedp harness (it reads them off the
// defined element); they have no browser dependency.
HaLuaEnhancedClimateCard.pure = {
  slugOf, companionId, statusLabel, clampNumber, configHash, formatClock, makeTranslator, MESSAGES,
};

customElements.define("ha-lua-enhanced-climate-card", HaLuaEnhancedClimateCard);

window.customCards = window.customCards || [];
window.customCards.push({
  type: "ha-lua-enhanced-climate-card",
  name: "ha-lua Enhanced Climate",
  description: "Schedule, boost and window-aware control for a climate entity.",
  preview: true,
});
