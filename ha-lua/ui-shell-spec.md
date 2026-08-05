# Tabbed UI shell + debug tab — Specification (draft)

> **Working state:** [`state/ui-shell.md`](state/ui-shell.md) — implementation progress, decisions.

Status: **ready to build, one decision to confirm** — §7.1 (how the tab bar
gets into a script's page). Everything else is settled.

## 1. Goal

One coherent web front end for the daemon:

- every script that opts in gets a **tab**, and every script page carries the
  same tab bar,
- a **Debug** tab shows what the daemon is doing right now: running scripts,
  their routes/timers/errors, daemon runtime numbers, and a live log tail.

## 2. The problem today

`ha.serve` registers into one flat path space shared by every script.
`examples/thermostat.lua` and `examples/enhanced_climate.lua` both register
`GET "/"`; `Router.match` (`internal/lua/router.go`) sorts bindings by prefix
length with a stable sort, so **which script owns `/` is decided by script load
order**. Two scripts with web UIs cannot coexist. There is also no runtime
introspection surface at all — the only debug tool is the log and, if enabled,
pprof.

## 3. Locked decisions

| Decision | Choice |
|----------|--------|
| Namespace | Scripts mount under **`/s/<script_id>/`**; the daemon owns `/`. Breaking → **v4.0.0** |
| Tab opt-in | **Explicit**: a script appears only if it calls the new **`ha.ui(title)`**. Machine-only API scripts never show up by accident |
| Composition | **No iframes.** The tab bar lives inside each page; switching tabs is an ordinary link → full page load. Only one page's CSS/JS is ever live, so scripts cannot collide |
| Debug content | Scripts table + daemon runtime + live log tail. **No** entity browser |
| Path seen by Lua | **Stripped** (`/api/state`, not `/s/thermostat/api/state`) — handler code and `req.path` are unchanged |
| Transport | Plain polling JSON. No SSE (see `sse-spec.md` §0) |

## 4. URL layout

```
/                       redirect to the first tab (→ /debug/ if no script opted in)
/api/tabs               JSON: [{id, title, path}] — the tab bar is built from this
/ui/tabs.js             tab-bar asset, embedded in the binary
/s/<id>/                script <id>'s  GET "/"  handler, tab bar injected
/s/<id>/api/...         script <id>'s other routes
/debug/                 debug page (embedded asset, same tab bar)
/debug/api/info         JSON snapshot: runtime, HA link, scripts, storage
/debug/api/logs?since=N&level=L   ring-buffer log lines newer than seq N
```

Everything the chrome emits must be **relative**. Under HA ingress the app is
served beneath `/api/hassio_ingress/<token>/`; absolute paths break there. The
injector knows the request's depth below the mount and emits the matching
number of `../` (see §7.2).

## 5. Router namespacing (`internal/lua/router.go`)

The route table changes shape from `method -> [(prefix, scriptID)]` to
`scriptID -> method -> [prefix]` (longest first). `ServeHTTP`:

1. path must start with `/s/`; the next segment is the script id,
2. `/s/<id>` without a trailing slash → **308** to `/s/<id>/` (relative fetches
   inside the page depend on the trailing slash),
3. unknown id, stopped runner (`reg.Get` returns nil), or no matching route → **404**,
4. otherwise forward with the **stripped** path.

`Register`/`Unregister` adapt to the new map. The pprof labelling
(`goroutine=web-request`, `script=<id>`) and the `reqCh` + deadline machinery
stay exactly as they are.

The routing table remains a hint only — the authoritative handler lookup still
happens in the script's own run loop, so a stale entry mid-reload self-heals to
a 404.

## 6. `ha.ui(title)` (`internal/lua/api_ha.go`)

Load-time only, like `ha.serve`. `CheckString(1)`, stored on `haAPI` next to
`routes`. `Runner` caches it after load beside `cachedRoutes`
(`cachedUITitle`) and exposes `UITitle() string`.

A script that sets a title but registers no `GET "/"` route gets a
`slog.Warn` at load — its tab would open onto a 404.

```lua
ha.ui("Heating")                    -- this script is a tab named "Heating"
ha.serve("GET", "/", function() ... end)
```

## 7. The tab bar

### 7.1 DECISION TO CONFIRM: how the bar gets into the page

A browser cannot compose two independent HTML documents without an iframe, and
iframes are out. So the bar has to be part of each page. Two variants:

- **(A) Auto-injected — recommended.** The daemon injects one `<script>` tag
  into HTML responses of opted-in scripts. No script edits, nothing to forget;
  the cost is that the daemon rewrites a script's HTML output (bounded, single
  insertion, skipped when the page already has the tag).
- **(B) Manually included.** Each UI page adds the one line itself. Explicit
  and inspectable; a page that forgets it silently loses the tabs, and every
  existing UI page must be edited.

Both share everything below except who inserts the line. Build (A) unless the
rewrite is judged too magic at implementation time.

A third option — a single-document SPA that fetches each page and injects its
markup — was **rejected**: shared global JS scope, element-ID collisions
between pages, and both bundled example pages would need rewriting into
fragments.

### 7.2 Injection rules (`internal/web/chrome.go`)

Middleware over the `*lua.Router`, mounted at `/s/`. It buffers the reply (the
body is already a Go string on the Lua side, so nothing is lost) and injects
only when **all** hold:

- the request maps to a script whose `UITitle() != ""`,
- status is 2xx and `Content-Type` is `text/html`,
- the body contains `</head>` (else insert after `<body …>`; if neither, skip
  and warn once per script),
- the body does not already reference `ui/tabs.js`.

The inserted line, with `../` depth computed from the request path:

```html
<script src="../../ui/tabs.js" data-base="../../" data-active="thermostat" defer></script>
```

JSON and other non-HTML replies are never touched.

### 7.3 `tabs.js` contract (`internal/web/assets/tabs.js`)

Reads `document.currentScript.dataset` (`base`, `active`), fetches
`<base>api/tabs`, prepends a `<nav>` with one `<a href="<base>s/<id>/">` per
tab plus the Debug tab, and marks the active one. It ships its own
class-prefixed `<style>` so it neither depends on nor leaks into the host
page's styling: `color-scheme: light dark`, neutrals from one `oklch()` hue
ramp, derived shades via `oklch(from …)` (project CSS rule).

`/api/tabs` is built from `Registry.All()` filtered on `UITitle() != ""`,
sorted by title, plus the trailing Debug entry.

## 8. Debug page (`internal/web/debug.go`, `assets/debug.html`)

`/debug/api/info` returns one snapshot:

- **Runtime** — version (§10), uptime, Go version, GOMAXPROCS, goroutines,
  MemStats (heap alloc/sys, GC count), pprof address when configured.
- **HA link** — `Client.Stats()`: connected, connected-since, reconnect count,
  last error, subscribed event types.
- **Scripts** — id, file, loaded/failed, ui title, routes, timers (from
  `Scheduler.Timers()`, grouped by script), handler count, event-queue
  depth/cap, dropped events, last exception.
- **Storage** — DB file size, `state_history` row count, mirror entity count,
  write-queue depth, retention days + purge interval.

`/debug/api/logs` serves `logbuf.Snapshot(since, level)`. The page polls info
every 3 s and logs incrementally, with a level filter. Same styling rules as
§7.3; carries the tab bar line with `data-active="debug"`.

### 8.1 New accessors this needs

| Package | Addition |
|---------|----------|
| `internal/scheduler` | `Timers() []TimerInfo{id, script_id, type, spec, next_run}` — snapshot under `s.mu` from the heap |
| `internal/lua` | `Runner`: last exception (`atomic.Pointer`, recorded in `dispatchException`), dropped-event counter (bumped where `SendHAEvent` warns today), queue depth/cap, handler count, `immediate_events` flag |
| `internal/state` | `Tracker`: mirror entity count, write-queue depth |
| `internal/ha` | `Client.Stats()` — new fields under the existing `c.mu`, set in `connect`/`loop` |

None of these may touch an `*lua.LState`.

## 9. Log ring buffer (`internal/logbuf`)

New package. `Handler` wraps a next `slog.Handler`, so stderr + log-file output
is unchanged, and keeps the last **500** records with a monotonic seq:
`{seq, time, level, msg, attrs map[string]string}` behind a mutex.
`Snapshot(sinceSeq, minLevel)` supports incremental polling. Wired in
`cmd/ha-lua/main.go` where the TextHandler/MultiWriter is built.

## 10. Version stamping

`var version = "dev"` in `cmd/ha-lua/main.go`, set with
`-X main.version=$(VERSION)` in `Makefile` and `Dockerfile`; `VERSION` is read
from `config.yaml`, which stays the single source of truth per the release
process.

## 11. Milestones (one commit each, `make test` green at every step)

1. `lua: mount script routes under /s/<id>/` — §5 + router tests; retarget the
   existing UI tests (`internal/lua/thermostat_ui_test.go`,
   `enhanced_climate*_test.go`) from `/` to `/s/<id>/`.
2. `lua: add ha.ui for naming a script's UI tab` — §6.
3. `web: tab bar chrome for script pages` — §7, the mux (`/s/`, `/api/tabs`,
   `/ui/`, `/debug/`, `/`), `web.Deps` (mirrors the `lua.Deps` pattern in
   `supervisor.go`), main wiring for both `web.Start` calls.
4. `logbuf: ring-buffer slog handler` — §9.
5. `scheduler: expose registered timers` — §8.1.
6. `lua/state/ha: expose runtime counters` — §8.1 (rest).
7. `web: debug page` — §8.
8. `build: stamp the version into the binary` — §10.
9. `examples: opt the bundled UIs into the tab bar` — `ha.ui("Heating")` in
   `thermostat.lua`, `ha.ui("Climate")` in `enhanced_climate.lua`. Both pages
   already fetch with `./…`, so nothing else changes.
10. Docs: `DOCS.md` (Web UIs section), `lua_api.md` (`ha.ui`, path semantics),
    `README.md`, this spec's state file, AI.state.
11. Release **v4.0.0** — changelog with a bold **BREAKING** lead, version bump,
    annotated tag, explicit push to both remotes.

## 12. Testing

- `internal/lua/router_test.go` — namespaced round trip, the 308, longest
  prefix within one script, two scripts both serving `/` and `/api/x` staying
  isolated, unknown id → 404.
- `internal/web/*_test.go` (`httptest`) — injection happens / does not happen
  per §7.2 rule, correct `../` depth for a nested path, tab JSON, `/` redirect,
  debug JSON shape.
- `internal/logbuf` — wrap-around, seq monotonicity, level filter, group/attr
  flattening.
- chromedp (same skip-when-no-browser harness as
  `internal/lua/thermostat_ui_test.go:96-145`) — the bar renders on a script
  page, its links point at the other tabs, the debug page renders.
- Manual: `go run ./cmd/ha-lua --config config.dev.yaml` with both examples in
  `./scripts`, open `http://localhost:8100/` — tabs on both pages, thermostat
  controls still work (proves stripped-path forwarding), debug tab lists both
  scripts, their timers, and live log lines while events flow.

## 13. Breaking changes (v4.0.0)

- Script routes no longer answer at the root. `http://<host>:8100/` now serves
  the daemon, and a script's UI moves to `http://<host>:8100/s/<id>/` —
  dashboard Webpage cards and bookmarks must be updated.
- Pages that fetch with **relative** URLs (`./api/state`) keep working
  unchanged; pages using absolute `/api/...` must be fixed.
- No Lua API removals: `ha.serve` keeps its signature and its stripped path.
