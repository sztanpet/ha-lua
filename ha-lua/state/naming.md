# State: HA 2026.11 naming model (in progress)

Goal (user, 2026-11-05: "do it all"): adopt HA's model — area = room, device =
what it is, entity = which part — plus Zigbee2MQTT names that match and
regenerated entity IDs with EVERY dependant updated.

## Released: v5.6.0 (2026-11-06)
- `aaea9b7` ha: Client.Query returns a command's result payload.
- `aea4b0b` ha: Names mirrors list_for_display / device list / area list;
  refetch 1 s after the last registry event. Verified parsing on the live box.
- `e0128a7` lua: ha.entity_name / device_name / area_name (HA template semantics).
- `25903d5` daemon wiring: subscribe, fetch after first seed, ChangedAll on reconnect.
- `40f6619` battery_levels: room-first names; moves series/events/tracked/
  ignored on a rename (the next scan would otherwise drop the old id's series).
- `95e163d` enhanced_climate: room-first names (page + companion friendly_name);
  moves the registry entry and all 12 per-climate store keys on a climate
  rename, rewires window/radiator/outdoor refs, removes the old companion.
- `965df15` card 0.3.45: ha-selector entity_name picker (text-field fallback);
  a composed name never prints "[object Object]".
- Tag `v5.6.0` on `41236aa`.

## Next, in order
1. Deploy: add-on update + restart; copy battery_levels.lua and
   enhanced_climate.lua to the box (no lib/html change); bump the card's
   lovelace resource cache-buster. Rename-following must be live BEFORE any
   entity id changes.
2. Box: areas → Hungarian (Háló, Nappali, Fürdő, Gyerekszoba, Konyha; add
   Galéria, Előszoba, Folyosó, Kint; English names as area aliases); assign
   ~55 devices; rename devices/entities (room out of names); voice aliases.
3. Z2M (runs in ~/services/zigbee2mqtt, NOT the HA add-on, which is stopped):
   names must be unique → "<area> <device>"; rename via bridge/request/device/
   rename, homeassistant_rename false. Update topics in ikea_dimmer.lua and
   nappali_switches.lua (+ tests, DOCS).
4. Entity IDs via config/entity_registry/get_automatic_entity_ids + update
   new_entity_id. Dependants found: dashboards (lovelace_lights,
   dashboard_tablet), scripts (group_switches, halo_pc_tv, heating_season,
   ikea_dimmer, mirrored_switches, nappali_switches = repo examples + tests),
   group helper "Bedroom IKEA lights" (does NOT follow renames; switch_as_x,
   mold_indicator, statistics do), konyha.yaml + halo-galeria2.yaml
   `platform: homeassistant` refs (heating board: compile first, rename, OTA at
   once, while no zone heats), companion sensors' voice exposure. Orphaned
   generic_thermostat entries (YAML commented out) hold climate.halo_futes etc.
