# Syslog input — Specification

Status: **planned, not started** (written 2026-10-17). No working-state file
yet; create `state/syslog.md` and link it here when the work begins.

## 1. Goal

Let a device push events to scripts the moment they happen, instead of a
script polling it.

The motivating case is `halo_pc_tv.lua`. The TV wakes on the bedroom PC's
router port (AmonHen, ether3) renegotiating at power-on. HA's MikroTik
integration polls about every 30 s and misses that ~1 s drop, so since
`21d2503` the script polls RouterOS's REST `link-downs` counter every second.
That works (10-17: TV on 22 s before 445 answered), but it costs 86,400 REST
calls a day, a stored router login, and the `rest-api` policy on the
`homeassistant` user group.

RouterOS can't run a script on a link change, but it writes one log line per
change and can forward its log over UDP syslog as each line is written:

```
2026-10-17 19:14:47 interface,info ether3 link up (speed 10M, full duplex)   power button
2026-10-17 19:14:53 interface,info ether3 link down
2026-10-17 19:14:57 interface,info ether3 link up (speed 1G, full duplex)    Windows driver
2026-10-17 19:15:51 interface,info ether3 link down                          hibernate
2026-10-17 19:15:52 interface,info ether3 link up (speed 10M, full duplex)   WoL link
```

The latency gain is under a second. The point is no polling, no credentials,
real events, and a hook other scripts can use for anything else that speaks
syslog.

## 2. Non-goals

- **Not a log store.** Datagrams go to scripts and are dropped; nothing is
  persisted or shown, beyond a received/dropped count on the debug page.
- **UDP only.** No TCP, TLS or RFC 5425 framing. The sender is on the LAN.
- **No authentication.** UDP syslog has none. This is the same LAN trust as
  port 8100. Scripts must check the sender (§4) and only do things that are
  harmless if spoofed. A faked line can at worst turn the TV on for 2 min.

## 3. Configuration

- New option `syslog_port` (int, default `0` = off), in `internal/config`,
  `config.yaml` (`options` + `schema`) and `config.dev.yaml`, next to
  `http_port`.
- `config.yaml` `ports:` gains `"5514/udp": 5514`, with a `ports_description`
  entry. Not 514: the port is unprivileged, and stays clear of any other
  syslog receiver on the host.
- The add-on is not on the host network; the Supervisor maps the port.
  **Open question 1 (§9)** is whether that mapping keeps the sender's source
  address.

## 4. Lua API

A `syslog` module, shaped like `mqtt`:

```lua
syslog.on(function(msg)
  -- msg.from      "192.168.1.1"  sender address (UDP source)
  -- msg.host      "AmonHen" or nil  hostname from the header, if the format has one
  -- msg.severity  6, msg.facility 3  from <PRI>, nil without one
  -- msg.message   "interface,info ether3 link down"  the text after the header
  -- msg.raw       the datagram as received
end)
```

- `syslog.on` may be called more than once; every handler sees every message.
  Scripts filter with `strings`/`re`; no filter argument in the first cut.
- Calling `syslog.on` while `syslog_port` is 0 raises, the same way `mqtt.*`
  reports a missing broker. A script then fails to load visibly instead of
  waiting forever.
- Document it in `lua_api.md` (module section) and `DOCS.md` (API table, plus
  a short "Receiving syslog" section with the RouterOS lines from §7).

## 5. Delivery

Copy the MQTT path (`mqtt-spec.md` §5, `Registry.DispatchMQTT`):

- One listener goroutine in a new `internal/syslog` package reads datagrams
  (64 KiB buffer) and calls `Registry.DispatchSyslog(msg)`.
- Only runners that registered a handler receive it (a cached flag, like
  `cachedStatePatterns`), through the existing per-script channel. It bypasses
  the 100 ms batch window, and drops with a warning on a full queue.
- Each handler runs in `pcall`; errors go to the script's `on_exception`.
- The listener stops on context cancel. A bind failure is logged as an error
  and disables the module; it does not stop the daemon.

### Parsing

Tolerant, never rejecting: anything unparsed lands whole in `message`.

- Optional `<PRI>` prefix → facility = PRI / 8, severity = PRI % 8.
- An optional RFC 3164 timestamp (`Oct 17 19:14:53`) and hostname after it.
- An optional RFC 5424 header (`1 2026-10-17T19:14:53+02:00 host app - - `).
- Whatever is left is `message`, with surrounding whitespace trimmed.

Pin the exact RouterOS bytes in the parser tests (milestone 0).

## 6. halo_pc_tv.lua migration

- Replace the `LINK_EVERY` timer, `read_link_downs`, the `mikrotik_login`
  lookup and `crypto`/`http` use with:

  ```lua
  syslog.on(function(msg)
    if msg.from ~= ROUTER_IP then return end
    if msg.message:find(" " .. PORT .. " link ", 1, true) then link_dropped() end
  end)
  ```

  Match on `msg.host` instead if §9 Q1 says the source address is rewritten.
- React to **any** ether3 link line, up or down. The earliest sign at power-on
  is a `link up (speed 10M)`, before any 1G renegotiation. `link_dropped`
  keeps its gates: PC offline ≥ 60 s, and a revert after 2 min if the PC
  never answers. The hibernate's down/up pair arrives before the probe's
  offline verdict, so it is ignored, as it is with polling today.
- Rename `link_dropped` to `link_changed`.
- Tests: drive `Registry.DispatchSyslog` directly from the harness instead
  of the fake REST server. Keep the existing scenarios: wake, revert without
  boot, ignore at hibernate, ignore while online. Add one for a line from
  another sender or another port.

## 7. Router side (mikrotik repo; apply only on explicit request)

Into `router/logging.rsc`, written idempotently (remove by name, then add):

```
/system logging action add name=ha-lua target=remote remote=192.168.1.123 remote-port=5514
/system logging add topics=interface action=ha-lua
```

`192.168.1.123` is homeassistant.lan's static lease. Once the script no
longer polls:

```
/user group set [find name=homeassistant] policy=read,test,api
```

That puts the group back to its pre-10-17 policy (`!rest-api`). Follow that
repo's rules: back up first, re-export with `scripts/backup.sh`, commit and
push the repo.

## 8. Milestones (each its own commit; `make check` green)

0. **Capture the real format.** Point a temporary logging action with
   `topics=script` at the dev machine (`nc -ul 5514`) and run
   `:log info "ha-lua test"` on the router. Try both the default format and
   the one with a hostname (§9 Q1). Record the exact bytes for the parser
   tests, then remove the temporary action. *Router change: ask first.*
1. `config: add syslog_port` — option, defaults, `config.yaml` schema and
   UDP port mapping, config tests.
2. `syslog: parse datagrams` — `internal/syslog` parser and its table tests,
   including the captured RouterOS lines.
3. `syslog: listen and dispatch` — listener, `Registry.DispatchSyslog`, the
   runner's handler flag, a loopback UDP test that a datagram reaches a
   script, and that one without a handler is skipped.
4. `lua: add syslog.on` — binding, raise when disabled, `api_syslog_test.go`.
5. `debug: count syslog datagrams` — received/dropped on the debug page.
6. `docs:` — `lua_api.md`, `DOCS.md`, `CLAUDE.md` packages line.
7. Release **v5.4.0** (minor: new feature), using the `release` skill.
8. Update the add-on on the box (Supervisor API, as for v5.3.0) and set
   `syslog_port: 5514` in its options. *Changes the add-on config: confirm.*
9. Apply §7's logging lines on AmonHen and watch a datagram arrive (debug
   counter, or a throwaway script logging `msg.raw`). Settle §9 Q1 here.
10. `examples: wake the TV on router syslog` (§6), then deploy it.
11. Once a real power-on has woken the TV through syslog: revoke `rest-api`
    (§7) and delete `global mikrotik_login` with a throwaway script.

## 9. Open questions

1. **Does the Supervisor's UDP port mapping keep the source address?** With
   iptables DNAT it does; Docker's userland proxy would replace it with the
   bridge gateway. Check in milestone 9: log `msg.from` for a router line.
   If it is rewritten, filter on `msg.host`. That needs a header format with
   a hostname: RouterOS `remote-log-format=syslog` (or `bsd-syslog=yes` on
   older 7.x), to be checked against 7.23.5 in milestone 0.
2. **Topic filter.** `topics=interface` also forwards every other port's
   up/down and interface warnings. That's a handful of lines a day, so no
   router-side filter. Revisit if another script wants a wider topic.
3. **Router reboot.** The log action is persistent config, so it survives a
   reboot. Lines logged while ha-lua is down are lost (UDP); the 445 probe
   remains the fallback wake, as it is now when the router is unreachable.
