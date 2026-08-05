# Tabbed UI shell + debug tab — Specification (draft)

> **Working state:** [`state/ui-shell.md`](state/ui-shell.md) — implementation progress, decisions.

Status: **ready to build.** §7.1 was resolved on 2026-08-05 in favour of an
iframe shell; nothing is open.

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
| Composition | **Iframe shell.** The daemon serves the tab bar at `/`; the active page loads in an `<iframe>`. Script HTML is never rewritten and the browser isolates each page's CSS/JS/element IDs |
| Debug content | Scripts table + daemon runtime + live log tail. **No** entity browser |
| Path seen by Lua | **Stripped** (`/api/state`, not `/s/thermostat/api/state`) — handler code and `req.path` are unchanged |
| Transport | Plain polling JSON. No SSE (see `sse-spec.md` §0) |

## 4. URL layout

```
/                       shell: tab bar + iframe. #<id> in the hash picks the tab
/api/tabs               JSON: [{id, title, path}] — the tab bar is built from this
/ui/shell.js            shell asset, embedded in the binary
/s/<id>/                script <id>'s  GET "/"  handler (also usable standalone)
/s/<id>/api/...         script <id>'s other routes
/debug/                 debug page (embedded asset)
/debug/api/info         JSON snapshot: runtime, HA link, scripts, storage
/debug/api/logs?since=N&level=L   ring-buffer log lines newer than seq N
```

Every URL the shell emits must be **relative**. Under HA ingress the app is
served beneath `/api/hassio_ingress/<token>/`; absolute paths break there. The
shell is the only document the daemon generates and it always sits at the mount
root, so `s/<id>/` and `debug/` resolve correctly under any prefix without the
shell knowing what that prefix is.

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

## 7. The shell

### 7.1 RESOLVED: the shell is an iframe host

The daemon serves **one** document, at `/`: a tab bar plus an `<iframe>` that
holds the active page. Script pages are served exactly as the script wrote
them — the daemon never rewrites a script's HTML — and the browser gives each
page its own JS global scope, CSS scope and element-ID space, so two script
UIs can never collide no matter what they contain.

Two alternatives were considered and dropped: **auto-injecting** a tab-bar
`<script>` tag into every script's HTML reply (the daemon rewriting user output
is magic, and needs content-type/`</head>` sniffing), and a **single-document
SPA** that fetches each page and splices its markup in (shared global scope,
element-ID collisions, both bundled example pages would need rewriting into
fragments). A script page still answers standalone at `/s/<id>/`, without the
bar, for anyone who wants to bookmark just that page.

### 7.2 Shell page (`internal/web/assets/shell.html`, `shell.js`)

Static embedded assets — no templating, no per-request work. Layout is a flex
column at `100dvh`: `<nav>` on top, `<iframe>` filling the rest with
`border:0; width:100%; flex:1`.

`shell.js` on load and on every `hashchange`:

1. fetch `api/tabs` (relative — see §4), build one `<a href="#<id>">` per tab
   plus the trailing Debug tab,
2. read `location.hash`; empty → the first tab, or `#debug` when no script
   opted in,
3. point the iframe at `s/<id>/` (or `debug/` for the debug tab) and mark the
   active link.

Hash routing rather than a real path means back/forward and reload keep the
selected tab, and switching tabs never reloads the shell itself. Deep links go
to the tab, not to a sub-path inside it — a script's own internal navigation is
the script's business.

Styling ships in the shell only: `color-scheme: light dark`, neutrals from one
`oklch()` hue ramp, derived shades via `oklch(from …)` (project CSS rule).
Nothing the shell does can leak into a script's page.

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
§7.2. It is an ordinary page inside the shell's iframe — it carries no tab bar
of its own.

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
3. `web: shell page with the tab bar and iframe` — §7, the mux (`/s/`,
   `/api/tabs`, `/ui/`, `/debug/`, `/`), `web.Deps` (mirrors the `lua.Deps`
   pattern in `supervisor.go`), main wiring for both `web.Start` calls.
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
- `internal/web/*_test.go` (`httptest`) — shell served at `/`, tab JSON
  (opted-in scripts only, sorted, Debug last), assets served under `/ui/`,
  script replies passed through byte-for-byte, debug JSON shape.
- `internal/logbuf` — wrap-around, seq monotonicity, level filter, group/attr
  flattening.
- chromedp (same skip-when-no-browser harness as
  `internal/lua/thermostat_ui_test.go:96-145`) — the shell renders its tabs,
  the iframe loads the first script's page, changing the hash swaps the iframe,
  the debug page renders.
- Manual: `go run ./cmd/ha-lua --config config.dev.yaml` with both examples in
  `./scripts`, open `http://localhost:8100/` — a tab per example, thermostat
  controls still work (proves stripped-path forwarding), debug tab lists both
  scripts, their timers, and live log lines while events flow.

## 13. Breaking changes (v4.0.0)

- Script routes no longer answer at the root. `http://<host>:8100/` now serves
  the daemon, and a script's UI moves to `http://<host>:8100/s/<id>/` —
  dashboard Webpage cards and bookmarks must be updated.
- Pages that fetch with **relative** URLs (`./api/state`) keep working
  unchanged; pages using absolute `/api/...` must be fixed.
- No Lua API removals: `ha.serve` keeps its signature and its stripped path.
