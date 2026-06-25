# Bundled examples — Specification (draft)

> **Working state:** [`state/bundled-examples.md`](state/bundled-examples.md) — implementation progress and decisions.

Status: **ready to build**. Open decisions are consolidated in §7.

> Filename is `load-examples-spec.md` for continuity, but examples are
> **reference-only** — the daemon never *loads/runs* them (see §1, decision
> history below).

## 1. Goal

Ship a small, **generic, sanitized** set of example scripts inside the add-on
and drop them, read-only, into `/config/ha-lua/examples/` on every boot so a
user can read them in Studio Code Server, copy what they want into
`scripts/`, and edit. Nothing is executed from `examples/`.

Today the add-on ships **no scripts** (the Dockerfile copies only the binary +
`run.sh`, `Dockerfile:21-22`), and the example set lives only in the repo at
`ha-lua/scripts/` — where it doubles as the *author's own* heating deployment
(real entity IDs like `climate.konyha_halo_futes` in `lib/zones.lua`). That
conflation is the smell this spec removes: the public artifact should carry
*examples*, not one household's config.

### Decision history (why this is reference-only)

An earlier draft proposed a `load_examples` option that made the examples dir a
second, runnable script source (with Supervisor multi-source precedence, a
watcher fallback, etc.). Rejected: running examples against entities a stranger
doesn't have is useless, and the whole multi-source machinery existed only to
let the author run their own scripts from the bundle — which is a *deployment*
concern, solved by keeping personal scripts in `/config` (git-managed), not by
the add-on. Reference-only keeps the honest UX and deletes ~3 milestones of
complexity.

## 2. Locked decisions

| Decision | Choice |
|----------|--------|
| Repo layout | **Rename `ha-lua/scripts/` → `ha-lua/examples/`.** Makes the dir's role explicit — it is the project's example set, not the author's deployment. |
| Personal config | The author's real `lib/zones.lua` (real entity IDs) **leaves the repo.** It is replaced by a sanitized placeholder `lib/zones.lua` (`climate.living_room`, etc.) with a "replace these" comment. The author's real copy lives only in their `/config/ha-lua/scripts/` (which persists across add-on updates) — git-managed there if they want bootstrap convenience. |
| Bundling | **`go:embed`**, not a Docker `COPY`: one static binary, identical in dev, unit-testable. Embed file lives in `examples/embed.go`. |
| Materialization | On every boot in add-on mode, overwrite `/config/ha-lua/examples/` from the embedded FS. Read-only reference; user edits there are not meant to persist. |
| Running examples | **Never.** Examples are documentation. The customization path is copy `examples/foo.lua` → `scripts/foo.lua`, edit. Only `scripts/` is loaded and hot-watched (unchanged). |
| New user option | **None.** Materialization is automatic and contained; no `load_examples`, no schema change. |

## 3. What changes (small)

The Supervisor, watcher, Registry, Scheduler, Router, and per-script store are
**untouched**. The runtime user dir stays `/config/ha-lua/scripts`
(`config.go:68`). The only moving parts:

1. **Rename + sanitize.** `git mv scripts examples`; sanitize `lib/zones.lua` to
   placeholder entity IDs. Update the single test path coupling
   `repoScriptsDir = "../../scripts"` → `"../../examples"` (`scripts_test.go:26`).
   Tests already use an inline `testZonesLua` (`scripts_test.go:158`), not the
   real `zones.lua`, so sanitizing is **zero test churn**;
   `TestShippedScriptsCompile` keeps passing because the placeholder still
   compiles.
2. **Embed.** `examples/embed.go`, `package bundled`.
3. **Config.** One forced field, no user option.
4. **main.** One `Materialize` call at boot.

## 4. The embed package (`examples/embed.go`, `package bundled`)

```go
//go:embed *.lua lib/*.lua *.html
var FS embed.FS

// Materialize writes every embedded file under destDir (overwriting), creating
// parent dirs, and drops a generated README.txt.
func Materialize(destDir string) error
```

- The `*.lua / lib/*.lua / *.html` patterns exclude `embed.go`, so the `.go`
  file is never written out.
- `README.txt`: "Auto-generated each boot from the installed add-on version — do
  not edit. Copy a file into ../scripts/ to customize and run it."
- Test `examples/embed_test.go`: materialize into `t.TempDir()`, assert the
  files exist with content equal to `FS`, and that a second call overwrites a
  locally-modified file back to the embedded content.

## 5. Config (`internal/config/config.go`)

- Add `ExamplesDir string` — **not** a user option. In the `addon` block
  (`config.go:111-122`, alongside `LogDir`/`IngressPort`) set
  `cfg.ExamplesDir = "/config/ha-lua/examples"`. Dev mode leaves it empty unless
  set in YAML; empty ⇒ no materialization (dev safety: no `/config` to write).
- `config.yaml` `options:`/`schema:` are **unchanged** — no new user option.

## 6. main wiring (`cmd/ha-lua/main.go`)

Beside the existing scripts-dir setup (`main.go:128-133`), add:

```go
if cfg.ExamplesDir != "" {
    if err := bundled.Materialize(cfg.ExamplesDir); err != nil {
        slog.Warn("examples materialize failed", "dir", cfg.ExamplesDir, "err", err)
    }
}
```

Best-effort: a failure logs and the daemon continues. No `OpenRoot`, no second
script source, no Supervisor/watcher change.

## 7. Open decisions (consolidated)

### 7.1 A toggle to disable materialization — **default NO**
Writing six small files into a contained, documented reference dir each boot is
harmless. Add a `bundle_examples: false` option only if a user actually objects.

### 7.2 Keep `scripts/` name vs rename to `examples/` — **rename**
Renaming is the concrete realization of "make this not feel like it's purely for
me": the repo dir is unambiguously the project example set. Cost is one const +
a `git mv` (history preserved).

### 7.3 Locked defaults
- Examples are reference-only; never executed; only `/config/ha-lua/scripts` is
  loaded and watched.
- `examples/` is always materialized in add-on mode; overwritten each boot; the
  user `scripts/` dir is never touched.

## 8. Migration note (do this first)

Before sanitizing, the author must **save their real `lib/zones.lua`** (and any
other personal scripts) out of the repo into their `/config/ha-lua/scripts/`
(where they already run and persist), so the sanitization doesn't lose the real
entity IDs. The deployed copy on the HA box is unaffected by repo changes; this
is only about not losing the canonical copy from version control.

## 9. Implementation milestones (one commit each)

1. `examples: rename scripts/ to examples/` — `git mv`, update
   `repoScriptsDir`. `make test` green (mechanical).
2. `examples: sanitize zones.lua to placeholder entity ids` — generic IDs +
   "replace these" comment. `TestShippedScriptsCompile` still passes.
3. `examples: embed reference scripts via go:embed` — `examples/embed.go` +
   `Materialize` + `embed_test.go`.
4. `config: force examples dir in add-on mode` — `ExamplesDir` field +
   add-on forcing + `config_test.go`.
5. `cmd: materialize examples into /config on boot` — main.go wiring.
6. `docs` + release (minor bump): `DOCS.md` (what `examples/` is,
   copy-to-customize), `CHANGELOG.md`, `config.yaml` version bump, `AI.state`,
   push to both remotes per `CLAUDE.md`.

## 10. Verification

- `cd ha-lua && make check` (vet + staticcheck + lint + race tests).
- `embed_test.go` covers materialize/overwrite; `config_test.go` covers the
  forced `ExamplesDir`; the existing thermostat/schedule tests prove the renamed
  dir still feeds them.
- Manual dev run: set `examples_dir` in `config.dev.yaml`, run
  `./ha-lua --config config.dev.yaml`, confirm the dir is populated read-only and
  that nothing from it executes (only `scripts/` loads).
