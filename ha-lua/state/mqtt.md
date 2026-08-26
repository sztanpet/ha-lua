# State: MQTT client (mqtt-spec.md)

Working state for the daemon's MQTT client and the Lua `mqtt` module. Spec:
`mqtt-spec.md`. Global decisions live in `../AI.state`.

Status: **in progress** (started 2026-08-26).

## Why it exists (2026-08-26)
- Field problem: an IKEA dimmer on Zigbee2MQTT 2.x is published as an MQTT
  device trigger. That is not an entity and not a bus event — HA's mqtt
  integration calls the automation engine directly — so nothing on the HA
  WebSocket API can ever see the press. The workaround (an HA automation
  re-firing it as `ha_lua_command`) works but puts an HA automation back in
  the path this daemon exists to remove.
- User's call, 2026-08-26: subscribe to MQTT directly, with publish too, and
  move the dimmer's hold-to-dim ramp onto the device (`brightness_move`)
  instead of stepping brightness from Lua.

## Decisions
- **Client library: `github.com/eclipse/paho.mqtt.golang` v1.5.x.** MQTT
  3.1.1, which is what Mosquitto and HA speak; mature, auto-reconnect built
  in, no known vulns (pkg.go.dev, 2026-08-26). `paho.golang`/autopaho is the
  v5 successor but a heavier API for no feature we need.
- **Test broker: `github.com/mochi-mqtt/server/v2`, test-only.** Real broker
  in-process on 127.0.0.1:0 beats a hand-rolled fake for a protocol client.
- **Add-on mode configures itself** from the Supervisor's
  `GET /services/mqtt` (needs `services: - mqtt:need` in config.yaml). Dev
  mode takes an explicit `mqtt:` block.
- MQTT messages bypass the batch window (like timers): coalescing a press
  with its release would break every button.
