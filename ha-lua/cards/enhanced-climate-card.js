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
// This is the render/lifecycle skeleton; the climate-native controls, the
// boost/schedule editor, the config editor, and i18n are layered on in the
// following commits.

const VERSION = "0.1.0";

console.info(
  `%c ha-lua-enhanced-climate-card %c v${VERSION} `,
  "color: white; background: #03a9f4; font-weight: 700;",
  "color: #03a9f4; background: white; font-weight: 700;",
);

class HaLuaEnhancedClimateCard extends HTMLElement {
  // setConfig validates and stashes the dashboard YAML. climate_entity is the
  // only required field; HA renders an error card if it is missing.
  setConfig(config) {
    if (!config || !config.climate_entity) {
      throw new Error("enhanced-climate-card: climate_entity is required");
    }
    this._config = config;
    this._render();
  }

  // hass is pushed on every state change; stash it and re-render.
  set hass(hass) {
    this._hass = hass;
    this._render();
  }

  getCardSize() {
    return 5;
  }

  static getStubConfig() {
    return { climate_entity: "" };
  }

  _render() {
    if (!this._config) {
      return;
    }
    if (!this.shadowRoot) {
      this.attachShadow({ mode: "open" });
    }
    this.shadowRoot.innerHTML = `
      <ha-card>
        <div class="card-content">Setting up…</div>
      </ha-card>
    `;
  }
}

customElements.define("ha-lua-enhanced-climate-card", HaLuaEnhancedClimateCard);

window.customCards = window.customCards || [];
window.customCards.push({
  type: "ha-lua-enhanced-climate-card",
  name: "ha-lua Enhanced Climate",
  description: "Schedule, boost and window-aware control for a climate entity.",
  preview: true,
});
