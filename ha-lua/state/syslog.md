# State: Syslog input (syslog-spec.md)

Spec: `syslog-spec.md`. Global decisions live in `../AI.state`.

Status: **in progress** (started 2026-10-17).

## Milestones
- [x] 0. Capture the real format (AmonHen, 7.23.5, 2026-10-17).
- [ ] 1. `config: add syslog_port`
- [ ] 2. `syslog: parse datagrams`
- [ ] 3. `syslog: listen and dispatch`
- [ ] 4. `lua: add syslog.on`
- [ ] 5. `debug: count syslog datagrams`
- [ ] 6. docs
- [ ] 7. release v5.4.0
- [ ] 8. update the add-on, `syslog_port: 5514`
- [ ] 9. router logging action, settle §9 Q1
- [ ] 10. halo_pc_tv on syslog
- [ ] 11. revoke `rest-api`, delete `mikrotik_login` (after a real wake)

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
