# State: Syslog input (syslog-spec.md)

Spec: `syslog-spec.md`. Global decisions live in `../AI.state`.

Status: **in progress** (started 2026-10-17).

## Milestones
- [x] 0. Capture the real format (AmonHen, 7.23.5, 2026-10-17).
- [x] 1. `config: add syslog_port` (913afb9)
- [x] 2. `syslog: parse datagrams` (7e2d24f)
- [x] 3. `syslog: listen on UDP` (eda27c9) — the listener alone; the
  dispatch moved into 4, which is the first commit that can test it.
- [x] 4. `lua: add syslog.on` (adf7107) — DispatchSyslog, the runner flag,
  the loopback test.
- [x] 5. `debug: count syslog datagrams` (9c05c47)
- [x] 6. `docs: document the syslog input` (2562146). CLAUDE.md has no
  package list, so nothing to add there.
- [x] 7. release v5.4.0 (5390bb7, tagged 2026-10-17)
- [x] 8. Add-on on the box updated to 5.4.0 and `syslog_port: 5514` set
  (Supervisor API, 10-17 19:39 CEST); "syslog: listening addr=[::]:5514".
- [x] 9. AmonHen `halua` action applied from `router/logging.rsc` (mikrotik
  `47b3755`; binary backup + export fetched first, re-run checked
  idempotent). A temporary `topics=script` rule proved the path end to end.
- [x] 10. `examples: wake the TV on router syslog` (8f24add), deployed
  10-17 19:44 CEST: loaded with syslog=true, 1 timer.
- [ ] 11. **Pending a real power-on through syslog.** Then: drop `rest-api`
  from the `homeassistant` group (`policy=read,test,api`, mikrotik repo
  rules: backup, apply, re-export, commit, push) and delete
  `global mikrotik_login` with a throwaway script. User authorization for
  the router change is per-session: ask.

## Decisions
- **The "wants syslog" flag is an atomic set by `syslog.on` itself**, not a
  load-time cache like the MQTT filters: a handler registered from a callback
  works, and datagrams arriving mid-load after the call queue up.
- `syslog.on` with the input off (port 0 or a failed bind) raises with a
  `syslog_port` hint, same rule as `mqtt.*` without a broker.
- Drops are counted per registry (`SyslogDropped`): `Runner.Send` now returns
  whether it queued. The per-script dropped counter still counts them too.
- `(*net.conn).Close` joined the errcheck exclusions: `*net.UDPConn`'s Close
  is promoted from the unexported type, so that is the name errcheck sees.

## §9 Q1 answered: the source address survives
The Supervisor's 5514/udp mapping keeps the sender's address: a datagram
from the dev box arrived as `from=192.168.1.139`, AmonHen's as
`from=192.168.1.1 host=AmonHen`. halo_pc_tv filters on `msg.from`.

## Field facts: RouterOS 7.23.5 datagrams
One line per datagram, no trailing newline. Captured with `:log` on
`topics=script`:

| action settings | bytes |
|---|---|
| `remote-log-format=default` | `script,info ha-lua test` |
| `syslog`, `bsd-syslog`, `add-topics-string=no` | `<30>Oct 17 19:24:25 AmonHen ha-lua test` |
| `syslog`, `bsd-syslog`, `add-topics-string=yes` | `<30>Oct 17 19:24:34 AmonHen script,info ha-lua test` |
| `syslog`, `iso8601`, `add-topics-string=yes` | `<28>2026-10-17T19:24:45.159+02:00 AmonHen script,warning ha-lua warn` |

- `default` has no PRI and no header at all: just topics and text.
- `syslog` defaults to `syslog-facility=daemon` (3) and `syslog-severity=auto`
  (info → 6, warning → 4), and DROPS the topics unless `add-topics-string=yes`.
- The iso8601 form is not RFC 5424 (no version field): PRI, timestamp, host.
- **A logging action name may contain only letters and digits** — `ha-lua`
  is refused. The action is `halua`.
- Chosen for §7: `remote-log-format=syslog add-topics-string=yes`
  (bsd time). It carries the hostname, so `msg.host` works even if §9 Q1
  says the source address is rewritten, and `message` keeps the
  `interface,info` prefix the spec's examples match on.
