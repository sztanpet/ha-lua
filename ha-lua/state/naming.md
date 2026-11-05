# State: HA 2026.11 naming model (in progress)

Goal (user, 2026-11-05: "do it all"): adopt HA's model — area = room, device =
what it is, entity = which part — plus Zigbee2MQTT names that match and
regenerated entity IDs with EVERY dependant updated.

## Done (local commits, not released)
- `aaea9b7` ha: Client.Query returns a command's result payload.
- `aea4b0b` ha: Names mirrors list_for_display / device list / area list;
  refetch 1 s after the last registry event. Verified parsing on the live box.
- `e0128a7` lua: ha.entity_name / device_name / area_name (HA template semantics).
- `25903d5` daemon wiring: subscribe, fetch after first seed, ChangedAll on reconnect.

## Next, in order
1. battery_levels: name = area + device + entity; follow renames via
   `ha.on_event("entity_registry_updated")` (data.old_entity_id → entity_id):
   move `series:`/`events:` keys, `tracked`, `ignored`. Without it the next
   scan deletes a renamed entity's series (state of old id is nil).
2. enhanced_climate: same naming for page + companion friendly_name; follow
   renames of climates (registry entry + ~12 per-climate store keys, remove the
   old companion state) and of window/radiator/outdoor sensors in entries.
3. Card 0.3.45: name field → `ha-selector` with `{entity_name: {}}`, context
   `{entity: climate}`; unavailable notice must not print an array name.
4. Release v5.6.0, deploy image + scripts (rename-following BEFORE any rename).
5. Box: areas → Hungarian (Háló, Nappali, Fürdő, Gyerekszoba, Konyha; add
   Galéria, Előszoba, Folyosó, Kint; English names as area aliases); assign
   ~55 devices; rename devices/entities (room out of names); voice aliases.
6. Z2M (runs in ~/services/zigbee2mqtt, NOT the HA add-on, which is stopped):
   names must be unique → "<area> <device>"; rename via bridge/request/device/
   rename, homeassistant_rename false. Update topics in ikea_dimmer.lua and
   nappali_switches.lua (+ tests, DOCS).
7. Entity IDs via config/entity_registry/get_automatic_entity_ids + update
   new_entity_id. Dependants found: dashboards (lovelace_lights,
   dashboard_tablet), scripts (group_switches, halo_pc_tv, heating_season,
   ikea_dimmer, mirrored_switches, nappali_switches = repo examples + tests),
   group helper "Bedroom IKEA lights" (does NOT follow renames; switch_as_x,
   mold_indicator, statistics do), konyha.yaml + halo-galeria2.yaml
   `platform: homeassistant` refs (heating board: compile first, rename, OTA at
   once, while no zone heats), companion sensors' voice exposure. Orphaned
   generic_thermostat entries (YAML commented out) hold climate.halo_futes etc.
