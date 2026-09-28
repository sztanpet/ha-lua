# State: MQTT client (mqtt-spec.md)

Spec: `mqtt-spec.md`. Global decisions live in `../AI.state`.

Status: **COMPLETE**, shipped in 4.7.0 (2026-08-26).

## Why it exists
An IKEA dimmer on Zigbee2MQTT 2.x is published as an MQTT device trigger: not
an entity, not a bus event, invisible to the HA WebSocket API. The alternative
was an HA automation re-firing it, which puts HA back in the path this daemon
exists to remove.

## Decisions and why
- Client: `eclipse/paho.mqtt.golang` v1.5 (MQTT 3.1.1, what Mosquitto and HA
  speak). Test broker: `mochi-mqtt/server/v2` in-process, test-only.
- Add-on mode configures itself from the Supervisor's `GET /services/mqtt`
  (`services: - mqtt:need`); dev mode takes an explicit `mqtt:` block.
- MQTT messages bypass the batch window: coalescing a press with its release
  would break every button.
- **Start waits for the first subscribe pass**, or a publish right after Start
  beat its own SUBSCRIBE and vanished.
- **paho's `SetConnectRetry` is NOT used**: with it a rejected CONNECT leaves
  the token pending forever and a wrong password reads as "connect timed out".
  `retryConnect` logs the broker's real reason.

## Field facts (live broker, 2026-08-26)
- Broker `homeassistant.lan:1883`, credentials `mqtt`/`mqtt`.
- Each Z2M press is published twice: the bare word on `<device>/action` and the
  full JSON on `<device>`. Subscribe to ONE; the examples use `/action`.
- `light.konyha_konyha_led` is not on MQTT at all, so a device-side
  `brightness_move` ramp is impossible for it; the daemon-side ramp stays.
