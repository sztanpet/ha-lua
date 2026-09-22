---
name: release
description: Release a new ha-lua version — changelog, version bump, annotated tag, working-state update, and the explicit two-remote push. Use when cutting a release, tagging a version, or preparing CHANGELOG.md for a new version.
---

# Release process

Versions follow **SemVer**: a backwards-incompatible Lua API or add-on change
is a **major** bump, new features are **minor**, fixes are **patch**.

The single source of truth for the version is `ha-lua/config.yaml`'s `version:`
field — no other file repeats it. Tag, `config.yaml`, and `CHANGELOG.md` must
all agree before tagging.

Steps for releasing `vX.Y.Z` (do not skip the per-step commits):

1. **Changelog.** Prepend a `## X.Y.Z - YYYY-MM-DD` section to
   `ha-lua/CHANGELOG.md` (Keep a Changelog format: `### Added` / `### Changed`
   / `### Fixed` / `### Security`). Mark breaking changes with a bold
   `**BREAKING: …**` lead. Commit as `docs: changelog for vX.Y.Z`.
2. **Version bump.** Edit only `version:` in `ha-lua/config.yaml`. Commit as
   `release: vX.Y.Z` — config.yaml only, nothing else in that commit.
3. **Tag.** Annotated tag on the `release:` commit:
   `git tag -a vX.Y.Z <release-commit> -m "vX.Y.Z"` (message is just the tag).
   Later docs commits may sit on top of the tagged commit; that's fine.
4. **Update the working state** to record the release: refresh the `## Latest` pointer in `ha-lua/AI.state` and note the release detail in the track's `ha-lua/state/<track>.md`. The changelog already carries what shipped — do not repeat it in `AI.state`.
5. **Push.** There is **no auto-push** — push explicitly. Two remotes:
   `origin` (private mirror) and `github` (github.com). Push `main` and the tag
   to **both**: `git push origin main && git push github main`, then
   `git push origin vX.Y.Z && git push github vX.Y.Z`.

Pushing the `v*` tag to **`github`** triggers `.github/workflows/release.yml`
(at the git root, not in `ha-lua/`), which builds the multi-arch images and
pushes them to GHCR (`ghcr.io/sztanpet/{arch}-ha-lua`). The workflow reads the
version from `config.yaml` at the tagged commit, so the tag must point at a
commit whose `config.yaml` already carries `X.Y.Z`.
