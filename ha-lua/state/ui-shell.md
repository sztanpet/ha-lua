# State: tabbed UI shell + debug tab (ui-shell-spec.md)

Working state for the front-end track: per-script `/s/<id>/` namespaces, the
shared tab bar, and the debug page. Spec: `ui-shell-spec.md`. Global decisions
live in `../AI.state`.

Status: **spec written, nothing built.** Milestones in spec §11, in order.

## Decisions made while writing the spec (2026-08-05)

- **Namespace `/s/<id>/`, daemon owns `/`.** Today both `thermostat.lua` and
  `enhanced_climate.lua` register `GET "/"` and the winner is decided by script
  load order — two web UIs cannot coexist. Breaking, so v4.0.0.
- **Explicit opt-in `ha.ui(title)`** rather than auto-detecting a `GET "/"`
  route: scripts serving machine APIs must not show up as tabs by accident.
- **No iframes** (user rejected them). A browser cannot compose two independent
  HTML documents any other way, so the tab bar lives inside each page and tab
  switching is a plain link → full page load. Upside: only one page's CSS/JS is
  ever live, so scripts cannot collide.
- **Rejected: single-document SPA** that fetches each page and injects its
  markup — shared global JS scope, element-ID collisions, and both bundled
  example pages would need rewriting into fragments.
- **Open (spec §7.1):** auto-injecting the tab-bar `<script>` tag into a
  script's HTML reply (recommended, no script edits) vs. each page including
  the line itself. Confirm before building milestone 3.
- **Debug tab scope:** scripts table + daemon runtime + live log tail. The
  entity/state browser was offered and dropped.
- **Polling, not SSE** — `sse-spec.md` §0 already argues SSE is not worth its
  permanent complexity for this system.

## Gotchas to remember at build time

- Every URL the chrome emits must be **relative**: under HA ingress everything
  is served beneath `/api/hassio_ingress/<token>/`. The injected tag carries a
  `data-base` computed from the request's depth below `/s/<id>/`.
- The path handed to Lua stays **stripped** — no script or `req.path` consumer
  changes.
- The existing UI tests (`internal/lua/thermostat_ui_test.go`,
  `enhanced_climate*_test.go`) navigate `srv.URL+"/?lang=en"`; they must be
  retargeted to `/s/<id>/` in the same commit as the router change.
- New introspection accessors must never touch an `*lua.LState`
  (spec §8.1).
