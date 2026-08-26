# MQTT client — Specification

> **Working state:** [`state/mqtt.md`](state/mqtt.md) — implementation progress and decisions.

Status: **in progress.**

## 1. Goal

Let scripts see and send MQTT directly, without Home Assistant in the middle.

The motivating case is a Zigbee button. Zigbee2MQTT 2.x publishes a button as
an **MQTT device trigger** (`device_automation` discovery): it produces no
entity, and HA's mqtt integration consumes it inside the automation engine, so
the press never reaches the event bus and no WebSocket client can observe it.
The only way for a script to see one today is an HA automation that re-fires
the press as an `ha_lua_command` event — which puts an HA automation back in
the path this daemon exists to remove, and adds MQTT → HA → bus → WS hops to
the most latency-sensitive thing in the house (a finger on a button).

Publishing matters for the same reason: `zigbee2mqtt/<device>/set` accepts
commands HA's `light` domain does not expose. `{"brightness_move": -40}` makes
the **bulb** ramp its own brightness until told to stop, which is what a dimmer
bound directly to a bulb does. Reaching that needs one publish per press
instead of a 250 ms service-call chain that only approximates it.

## 2. Non-goals

- **Not an HA replacement.** Entity state stays on the WebSocket API and the
  memory mirror. MQTT is a second, independent input; the daemon does not
  attempt to parse HA discovery topics or synthesize entities from them.
- **No broker.** The daemon is a client. Users already run one (Mosquitto).
- **No TLS client certificates** in the first cut: user/password over TCP,
  which is what the Supervisor's Mosquitto add-on hands out.

## 3. Configuration

In **add-on mode** there is nothing to configure. `config.yaml` declares
`services: - mqtt:need`, and the Supervisor answers
`GET http://supervisor/services/mqtt` (bearer `$SUPERVISOR_TOKEN`) with host,
port, ssl, username and password for the broker the user already set up. The
daemon fetches it at boot; a failure disables MQTT with a warning rather than
killing the process, because every other subsystem still works without it.

In **dev mode** (`--config`) the same fields are given explicitly:

```yaml
mqtt:
  broker: "tcp://127.0.0.1:1883"   # empty disables MQTT entirely
  username: ""
  password: ""
  client_id: ""                    # default: ha-lua-<hostname>
```

## 4. Lua API

```lua
mqtt.subscribe(filter, fn)   -- load time only, like ha.on_state_change
mqtt.publish(topic, payload [, opts])
```

- `filter` is a topic filter with the MQTT wildcards `+` (one level) and `#`
  (the rest). Validated at load: a filter with `#` anywhere but the last level
  raises, as does an empty one.
- `fn(topic, payload, decoded)` — the concrete topic the message arrived on,
  the raw payload as a string, and, when the payload is a JSON object or
  array, its decoded Lua table (else `nil`). Zigbee2MQTT publishes both shapes
  (`zigbee2mqtt/<name>/action` is a bare string, `zigbee2mqtt/<name>` is a JSON
  object), so both must be first-class.
- `payload` for publish is a string, number, boolean or table; tables and
  non-strings are JSON-encoded, strings are sent as-is.
- `opts.qos` (0–2, default 0) and `opts.retain` (default false).

## 5. Delivery semantics

- Messages **bypass the batch window**, like timers. Batching exists to keep a
  `state_changed` burst from overflowing a script channel; a button press must
  never be coalesced with the release that follows it.
- Delivery is per-script and non-blocking, reusing the existing runner channel
  and its drop-and-warn behaviour on a full queue.
- Subscriptions are per-script, but the *broker* subscription is deduplicated
  across scripts, like `Client.AddEventType` does for HA event types. Every
  reconnect re-subscribes the full set.
- QoS 0 in, by default: for a button, a message that arrives late is worse
  than one that never arrives, and the broker is on the same host.

## 6. Failure handling

- No broker configured → the `mqtt` module is still installed, but
  `mqtt.subscribe` logs a warning once per script and `mqtt.publish` raises.
  A script written for MQTT must fail loudly, not silently do nothing — that
  failure mode cost a full debugging session on the dimmer script.
- Connection loss → the client auto-reconnects and re-subscribes; publishes
  attempted while down raise.
- The debug page reports broker, connection state, reconnect count, and the
  subscribed filters, the same way it does for the HA link.
