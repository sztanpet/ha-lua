# State: MQTT client (mqtt-spec.md)

Working state for the daemon's MQTT client and the Lua `mqtt` module. Spec:
`mqtt-spec.md`. Global decisions live in `../AI.state`.

Status: **COMPLETE** (2026-08-26), unreleased. Commits `1e65a83` (spec),
`6d79eb4` (topic matching), `07befd2` (client), `6b310fa` (connect
diagnostics), `c78cac7` (Lua module), `66b11ac` (wiring), `45885c0` (dimmer
example), `9b265b0` (docs).

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

## Field facts, confirmed against the live broker (2026-08-26)
- The dimmer's topic is **`zigbee2mqtt/ikea dimmer 1/action`** — Zigbee2MQTT
  puts the friendly name in the topic VERBATIM, spaces included. It is not
  the underscored entity-id spelling, and assuming otherwise is what made the
  first version of the example watch a topic that never existed. A test pins
  the topic now.
- Each press is published twice: the bare word on `<device>/action` and the
  full JSON state on `<device>` (which also carries `action_rate`, 83 for
  this dimmer). Subscribe to ONE of them; the example uses /action.
- `light.konyha_konyha_led` is **not on MQTT at all** — 390 retained HA
  discovery messages, zero mention it, and Z2M's only lights are `light1`
  and `csillar1`. So the device-side `brightness_move` ramp the user asked
  for is impossible for that light; the daemon-side step chain stays, and
  the header documents the better path for lights that ARE on the broker.
- Broker: `homeassistant.lan:1883`, credentials `mqtt`/`mqtt`
  (`homeassistant`/`homeassistant` is rejected: "not Authorized").

## Implementation notes
- **Start waits for the first OnConnect subscribe pass.** Without it a
  publish issued right after Start beat its own SUBSCRIBE to the broker and
  the message vanished. Kept as a test.
- **paho's SetConnectRetry is NOT used.** With it a rejected CONNECT leaves
  the token pending forever, so a wrong password surfaces as "connect timed
  out" instead of the broker's "not Authorized" — found against the live
  broker. Retrying is done in retryConnect where the cause gets logged, and
  paho's own loggers are routed into slog.
- Registry.DispatchMQTT matches filters before sending, so one script's
  broker-wide subscription does not wake every other script.
- Load errors now land in the runner's recorded error (debug page), and a
  successful load logs a line with the handler counts. Both came out of the
  same session: "the script does nothing and the log says nothing".
