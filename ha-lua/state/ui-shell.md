# State: tabbed UI shell + debug tab (ui-shell-spec.md)

Working state for the front-end track: per-script `/s/<id>/` namespaces, the
shared tab bar, and the debug page. Spec: `ui-shell-spec.md`. Global decisions
live in `../AI.state`.

Status: **spec settled, build starting.** Milestones in spec §11, in order.
§7.1 is no longer open — the shell hosts pages in an iframe.

## Decisions made while writing the spec (2026-08-05)

- **Namespace `/s/<id>/`, daemon owns `/`.** Today both `thermostat.lua` and
  `enhanced_climate.lua` register `GET "/"` and the winner is decided by script
  load order — two web UIs cannot coexist. Breaking, so v4.0.0.
- **Explicit opt-in `ha.ui(title)`** rather than auto-detecting a `GET "/"`
  route: scripts serving machine APIs must not show up as tabs by accident.
- **Iframe shell (spec §7.1, decided 2026-08-05).** The draft had ruled iframes
  out and offered auto-injection vs. a manual include; asked to confirm, the
  user picked iframes instead. The daemon serves one document at `/` — tab bar
  plus an `<iframe>` — and never rewrites a script's HTML. That deletes the
  whole injection middleware (content-type/`</head>` sniffing, `../` depth
  computation, per-script warnings) and gives real isolation: separate JS
  global scope, CSS scope and element-ID space per page, for free.
- **Rejected: single-document SPA** that fetches each page and injects its
  markup — shared global JS scope, element-ID collisions, and both bundled
  example pages would need rewriting into fragments.
- **Hash routing in the shell** (`#<id>`): reload and back/forward keep the
  selected tab, and switching tabs never reloads the shell. Deep links address
  a tab, not a sub-path inside it.
- **Debug tab scope:** scripts table + daemon runtime + live log tail. The
  entity/state browser was offered and dropped.
- **Polling, not SSE** — `sse-spec.md` §0 already argues SSE is not worth its
  permanent complexity for this system.

## Gotchas to remember at build time

- Every URL the shell emits must be **relative**: under HA ingress everything
  is served beneath `/api/hassio_ingress/<token>/`. The shell always sits at the
  mount root, so `s/<id>/` and `debug/` resolve without it knowing the prefix.
- The path handed to Lua stays **stripped** — no script or `req.path` consumer
  changes.
- The existing UI tests (`internal/lua/thermostat_ui_test.go`,
  `enhanced_climate*_test.go`) navigate `srv.URL+"/?lang=en"`; they must be
  retargeted to `/s/<id>/` in the same commit as the router change.
- New introspection accessors must never touch an `*lua.LState`
  (spec §8.1).
