# State: tabbed UI shell + debug tab (ui-shell-spec.md)

Working state for the front-end track: per-script `/s/<id>/` namespaces, the
shared tab bar, and the debug page. Spec: `ui-shell-spec.md`. Global decisions
live in `../AI.state`.

Status: **COMPLETE — released v4.0.0, v4.0.1, v4.0.2 (all 2026-08-05).** All
11 milestones of spec §11 are done. One addition on top, released as **v4.2.0**
(2026-08-06): the debug log panel's per-script source filter (see below).

## Debug log panel: per-script source filter (RELEASED v4.2.0, 2026-08-06)

Commits `1383635` (logbuf), `345016c` (page), `5ff17c6` (docs), `cc6b124`
(print), `e3b3db0`/`126e005` (state+changelog), `1be2a19` (release, tagged).

Asked for "a log panel showing the messages of the different scripts", then
immediately for "a single log panel that includes everything" — the second
reading is the one built, and it was also the correct one: `ha.log` already
logs through the default `slog` logger with a `script=<id>` attr, so those
lines were *always* in the existing panel. Nothing was missing; the id just sat
at the tail of the attrs where the eye slides past it, and one chatty script
buried the rest. **Do not add a second panel.**

- `logbuf.Snapshot` takes a `Query{Since, Level, Script}` instead of two
  positional args. `Script`: `""` everything, `logbuf.AnyScript` (`"*"`) any
  record carrying the attr, else an exact id. `logbuf.ScriptKey` is the attr
  name. A filtered snapshot still returns the *global* newest seq, so the
  poller never re-fetches what the filter dropped.
- `/debug/api/logs?script=` passes it through — a single script's tail is
  curl-able from a headless box.
- Page: `script` renders as a `[tag]` after the timestamp (removed from the
  trailing attrs), plus a **Source** select rebuilt from `info.scripts` on
  each poll. A filter change resets `since=0` and clears, same as **Level** —
  a narrower filter cannot retroactively hide rendered lines.
- The browser tests now install a `logbuf`-backed default logger in
  `serveShell` (restored via `t.Cleanup`). Without that, `Logs:` was a buffer
  no logger wrote to and no real `ha.log` call could ever reach the page.
- `print()` was documented as stdout-only, then the user asked for it to land
  in the panel too (`cc6b124`). It is now replaced in `registerHaAPI` — next to
  `ha.log`, which is where the script id lives; `RegisterStdlib` has no script
  id and 7 test call sites, so it was not worth a signature change. One info
  line, base print's own formatting (`L.ToStringMeta`, tab-separated) so
  scripts read unchanged. Nothing in the repo's Lua uses `print`, so no example
  suddenly spams the log.

## Post-release fixes (v4.0.1)

- **`Registry.All()` now sorts by script id.** It ranged over a map, so the
  debug page's script table reshuffled on every 3s poll. Fixed in `All()`, not
  at the call site — that is where the guarantee belongs. `/api/tabs` uses
  `sort.SliceStable` on top so equal titles cannot swap either. Both regression
  tests loop 20× because one pass can match map order by luck.
- **Sidebar panel renamed** `Heating`/`mdi:thermostat` → `HA Lua`/
  `mdi:language-lua` in config.yaml.
- **Debug page flags a script that serves `GET "/"` but never called `ha.ui`.**
  Its page is unreachable from the tab bar and nothing said so.
- **v4.0.2: routes and timers get one line each on the debug page.** Routes
  were comma-joined; timers already used `\n` but the cell's
  `white-space: normal` ate it. Both now use a `td.lines`
  (`white-space: pre-line`) class. The browser test asserts the *computed*
  white-space, not just the text — the newlines were always in the DOM and
  only styling decided visibility, so a text-only assertion passes against the
  broken version.
- **Not a bug: a user's own script does not get `ha.ui` from the examples.**
  `examples/` materializes read-only to `/config/ha-lua/examples` and is never
  loaded; only `/config/ha-lua/scripts/*.lua` runs. Adding `ha.ui` to an
  example does nothing for the user's own edited copy of it — they must add the
  line themselves. Expect this question again for every future example change.

## Progress

| # | Milestone | Commit |
|---|-----------|--------|
| 1 | `lua: mount script routes under /s/<id>/` | `84aac19` |
| 2 | `lua: add ha.ui for naming a script's UI tab` | `1b8fdd7` |
| 3 | `web: shell page with the tab bar and iframe` | `1595cba` |
| 4 | `logbuf: ring-buffer slog handler` | `1933d36` |
| 5 | `scheduler: expose registered timers` | `006dd90` |
| 6 | `lua/state/ha: expose runtime counters` | `02814c1` |
| 7 | `web: debug page` | `c69d16f` |
| 8 | `build: stamp the version into the binary` | `4a675bf` |
| 9 | `examples: opt the bundled UIs into the tab bar` | `6c26d5a` |
| 10 | docs (DOCS.md, lua_api.md, README.md) | `6bfb973` |
| 11 | release v4.0.0 | `9398b2f`, `2865d0c`, tag `v4.0.0` |

Also `df936ad` (style: cut comments back to the non-obvious why) — the user
asked mid-build for far fewer comments, never narrating code. Applies to
everything still to be written.

## Decisions made while building

- **`web.Deps.Scripts` is a `func() []web.Script`, not `*lua.Registry`.**
  `*lua.Runner` cannot be faked from another package's test, and the shell only
  needs `ScriptID()`/`UITitle()`. main adapts the registry in six lines.
- **`Deps.Debug` nil ⇒ no Debug tab.** Keeps every milestone bisectable: the
  tab appears in the same commit as the page it opens.
- **The 308 writes its own `Location` header** instead of calling
  `http.Redirect`, which resolves a relative target back to an absolute path
  and would escape the ingress prefix.
- **`Router.Register` replaces** a script's routes rather than appending, so a
  reload cannot accumulate duplicates.
- **`logbuf` wraps rather than replaces** the text handler: stderr and the log
  file still get every record. Seq numbers make polling incremental with no
  overlap and no gap when the ring wraps.
- **On-demand goroutine stack dump** (`/debug/api/goroutines`, button on the
  page) — user asked for it mid-build. Uses `pprof.Lookup("goroutine")` with
  `debug=2`, so it works whether or not `debug.pprof_addr` is set; enabling
  that needs an options edit and a restart, exactly when you least want one.
  Note debug=2 output has no "goroutine profile" header — it is raw stacks.
- **Version stamped from `config.yaml`** via `-X main.version` in both the
  Makefile and the Dockerfile. No second copy to drift.

## Not verified

The spec §12 **manual run** was not performed: the daemon blocks on the first
HA state seed at startup (`main.go`, pre-existing), so it cannot serve anything
without a reachable HA. The composition is covered instead by chromedp tests in
`internal/web/shell_browser_test.go` driving real scripts through the real
handler — tab bar, iframe contents, hash switching, the debug tab and the stack
dump button.

## Still to do

`git push origin main && git push github main`, then the tag to both remotes.
Pushing the tag to `github` triggers the GHCR image build.

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
- The existing UI tests were retargeted to `/s/<id>/` in milestone 1; the
  helpers `doReqID`/`waitRouteID` take the script id, `doReq`/`waitRoute`
  default to `"ui"`.
- New introspection accessors must never touch an `*lua.LState`
  (spec §8.1).
