# State: core daemon (plan.md)

Spec: `plan.md`. Global decisions live in `../AI.state`.

Status: **COMPLETE** — milestones 1–12, add-on 1.0.0, stable since. Later
tracks build on it; code review rounds are in `code-review.md`.

## Gotchas not visible in the code
- **honnef.co/go/tools sits on v0.8.0-rc.1.** v0.7.0's IR builder panics on the
  Go 1.27 stdlib (`unexpected expr: *ast.KeyValueExpr`), taking down both
  `make staticcheck` and `make lint` (golangci-lint vendors the same pass).
  Drop the `-rc` once v0.8.0 is out.
- `go-json-experiment/json` still shows up as an indirect dep: chromedp pulls it
  in. Ours is the stdlib `encoding/json/v2`.
- **Dockerfile: no GOARCH mapping.** home-assistant/builder builds emulated
  under the target arch, so plain `go build` is right; mapping aarch64→arm64 by
  hand is the classic broken-image trap.
- The Go const `ingressPort = 8099` must equal `config.yaml`'s `ingress_port`;
  it is forced in add-on mode only, dev mode binds just the LAN port.
- Release CI is tag-only, by user decision: no PR test/lint workflow.
- golangci-lint's `install.sh` has a checksum bug (greps the `.sbom.json`
  line). Verify the release tarball against its sha256 by hand when bumping.
- The daemon blocks on the first HA state seed, so it serves nothing — UI
  included — without a reachable HA.
- Purge runs once at Start before its first tick; on the 1 h default a
  frequently restarted daemon would otherwise never purge.
- Hot-reload stop: Registry.Remove → Runner.Close (drain) → wait 5 s, then
  cancel the script ctx (aborts the VM). OnLoaded adds each script's event
  types to the HA subscription and never unsubscribes.
- The per-script event buffer is 256; batching, not the buffer, is what stopped
  the drops.

## Benchmarks
- `make bench-update` re-RUNS the suite; to promote an existing run,
  `cp benchmarks/current.txt benchmarks/baseline.txt`.
- One `make bench-compare` against an old baseline is noise-prone (a +4.7 %
  "regression" from pprof labels was machine noise). Re-measure both sides in
  one session before believing a delta.
- pprof goroutine labels: every goroutine runs under `pprof.Do`; the one that
  matters is `goroutine=script, script=<id>`, since every script runs the same
  interpreter loop and a stack alone cannot say which one burns CPU.
