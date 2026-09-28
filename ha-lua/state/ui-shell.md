# State: tabbed UI shell + debug tab (ui-shell-spec.md)

Spec: `ui-shell-spec.md`. Global decisions live in `../AI.state`.

Status: **COMPLETE** — v4.0.0 (all 11 milestones), the log source filter in
v4.2.0, the scroll fixes in v4.3.0–v4.3.2 (confirmed on the user's Android).

## Decisions and why
- **Namespace `/s/<id>/`, daemon owns `/`** — two scripts serving `GET "/"`
  used to be decided by load order. Tabs are explicit opt-in via
  `ha.ui(title)`, so machine APIs never become tabs by accident.
- **Iframe shell, chosen by the user** over auto-injection and over a
  single-document SPA (shared JS scope, element-ID collisions, every page
  rewritten into fragments). Hash routing (`#<id>`) keeps the tab across reload
  and back/forward.
- Debug tab scope: scripts table, runtime, live log tail. An entity browser was
  offered and dropped. Polling, not SSE (`sse-spec.md` §0).
- **One log panel, filtered by source. Do not add a second panel.** `ha.log`
  always carried `script=<id>`; it was just unreadable at the tail of the attrs.
  A filtered snapshot still returns the GLOBAL newest seq, so the poller never
  re-fetches what the filter dropped. `print()` lands in the panel too.
- `web.Deps.Scripts` is a func, not `*lua.Registry` (a Runner cannot be faked
  from another package's test). `Deps.Debug` nil means no Debug tab.
- `Router.Register` REPLACES a script's routes, so a reload cannot pile up
  duplicates. `Registry.All()` sorts by id (it ranged a map, so the debug table
  reshuffled each poll).
- The goroutine dump uses `pprof.Lookup("goroutine")` with `debug=2`, so it
  works without `debug.pprof_addr` (enabling that needs a restart, exactly when
  you least want one). That output has no header — it is raw stacks.
- The debug page flags a script that serves `GET "/"` without `ha.ui`: its page
  is unreachable from the tab bar.

## Gotchas
- The shell scroll bug never reproduced in headless Chromium at any DPR. What
  cracked it was the user describing the overscroll glow: the gesture WAS
  consumed, by a framed document with one scrollable pixel. Content heights are
  fractional and `scrollHeight` is not, so `fit()` forces `overflow:hidden` on
  the framed document plus 2 px of slack. `min-height:0` on `#view` is
  load-bearing.
- Browser tests of styling must assert the COMPUTED style: the per-line route
  and timer newlines were always in the DOM, only `white-space` hid them.
- Registry-order regression tests loop 20×: one pass can match map order by
  luck.
- Browser tests install a `logbuf`-backed default logger in `serveShell`;
  without it no real `ha.log` call can reach the page.
- **Not a bug, expect the question again:** changing a bundled example (adding
  `ha.ui`, say) does nothing for the user's own copy in `scripts/`; examples
  are reference-only.
- The spec §12 manual run was never done (the daemon needs a reachable HA);
  chromedp tests in `internal/web/shell_browser_test.go` cover the composition.
