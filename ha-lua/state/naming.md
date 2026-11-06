# State: HA 2026.11 naming model

Goal (user, 2026-11-05: "do it all"): adopt HA's model — area = room, device =
what it is, entity = which part — plus Zigbee2MQTT names that match and
regenerated entity ids with every dependant updated. DONE 2026-11-06.

## ha-lua (v5.6.0, examples in v5.6.1)
- `aaea9b7` Client.Query; `aea4b0b` Names registry mirror (refetch 1 s after
  the last registry event); `e0128a7` ha.entity_name/device_name/area_name;
  `25903d5` daemon wiring.
- `40f6619` battery_levels and `95e163d` enhanced_climate: room-first names,
  stored state follows `entity_registry_updated` renames. Proven live: all 12
  climate-side renames followed, learned c kept (Gyerekszoba 370 samples), and
  battery series moved (oldest samples from August).
- `965df15` card 0.3.45: HA's entity_name picker.
- `f98a3c0` examples follow the box's new ids and Zigbee2MQTT topics.

## The box (2026-11-06, ~03:20–03:50)
- Areas renamed to Háló, Nappali, Fürdő, Gyerekszoba, Konyha (old English names
  kept as area aliases); new: Galéria, Előszoba, Kint.
- 74 devices named for what they are (table: scratchpad tool `adopt`,
  devDefs), 48 entity names; 85 exposed entities got their old name as a voice
  alias first.
- Zigbee2MQTT (runs in `~/services/zigbee2mqtt`, services commit `81cc50e`):
  every device is "<room> <device name>"; the "light2" bulb is gone from
  Zigbee2MQTT's database, so it kept its old name.
- 403 entity ids regenerated: area + device + entity, slugified the way HA
  does. HA's own generator agreed on all 116 non-Zigbee ones.
- Dependants rewritten: automations.yaml, configuration.yaml (a comment), the
  Home dashboard (95 ids, 36 rows named with their room), the IKEA light group,
  six ha-lua scripts + the box's stale lib/zones.lua, and the ESPHome configs
  (esphome repo `2252973`). Mold indicator, statistics and switch_as_x helpers
  followed on their own. A rescan of every box config finds no old id.
- Follow-up (user: badges "just say hőmérő hom", then "make them match"):
  every reading on a badge, tile or history-graph row is named by
  composition, the user's own short names ("Gal hom.") included:
  thermometer `[area, entity]` ("Háló hőm."), radiator `[area, device,
  entity]`. The Galéria and Fürdő temperatures were the last "hőmérséklet"
  readings; renamed "hőm." (`sensor.galeria_homero_hom`,
  `sensor.furdo_homero_hom`), their history and statistics moved in place.
  Checked in a rendered browser.
- The two outdoor statistics helpers (daily mean, 3-day average) lost their
  window at the rename and averaged only new readings (2.4 instead of 6.3)
  until reloaded at 05:49; the dip stays in their history.
- The daily mean was renamed "napi átlag" (`sensor.kint_homero_napi_atlag`)
  to match "3-napos átlag". Dependants: the Fürdő morning automation's
  condition, and `outdoor_entity` on all four enhanced-climate cards (the
  script's own registry followed by itself).
- Fürdő, Háló and Nappali heating history merged live at 06:01 (user OK'd;
  restart offered and judged unnecessary): rows swapped between
  `states_meta` ids so the recorder's cached ids (180–182) now hold June →
  now, and the dead generic thermostats' 190 "unavailable" rows each went to
  449–451, renamed `climate.<zone>_futes_old_generic`. Undo: swap back
  (rows of 180–182 below `last_updated_ts` 1793932860 ↔ all of 449–451)
  and rename 449–451 to `climate.konyha_<zone>_futes`.
- The three commented-out generic_thermostat entries were removed: they held
  climate.halo_futes etc.
- The kitchen LED is its own ESPHome device ("LED szalag", Konyha).

## Gotchas
- **HA's `get_automatic_entity_ids` is stale for Zigbee2MQTT entities**: it
  reuses the object id Zigbee2MQTT suggested when the entity was first
  announced, and rediscovery does not refresh it. Compose the id instead.
- **Zigbee2MQTT device option `homeassistant: null` turns discovery off** for
  the device; use `{}` to drop a name override. An options change does not
  republish discovery; a rename (to a temp name and back) does.
- **HA's WS `mqtt/subscribe` delivers nothing on this box**; confirm a
  Zigbee2MQTT request from its config file or `docker logs zigbee2mqtt`.
- **konyha.yaml reads HA entities** (room temps, radiator valves): renaming
  them means a reflash in the same minute. Precompile, rename, OTA.
- Rename a heating zone's entities in a small paced batch and check
  enhanced_climate followed each one before the bulk: a dropped registry event
  would strand a zone's schedule under the old id.
- **A statistics helper follows a source rename by reloading, before the
  recorder has moved the source's history**: it comes back with an empty
  window. Reload it again once the rename has settled.
- **The recorder will not move history onto an id that already has a
  states_meta row** ("Cannot migrate history … already in use"), even one
  left by a long-deleted entity. Check states_meta for the target ids first.
  It hit Fürdő, Háló and Nappali heating (merged by hand, see above);
  retention is 730 days, so such a split does not age out.

## Found broken before this work (not touched)
- Offline in Zigbee2MQTT since 2026-11-05 07:55: Ventilátor, Konyha hűtő,
  Konyha mosogatógép plugs and Galéria asztali lámpa (dugalj1).
- The bedroom sensor node (halo-galeria2) has been unavailable since at least
  2026-10-20; its config is updated and compiled, flash it when it is back.
- The IKEA light group and both bulbs are unavailable.
