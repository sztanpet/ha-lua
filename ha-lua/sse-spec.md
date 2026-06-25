# Server-Sent Events (serving) — Specification (draft)

> **Working state:** none yet — not started. Create `state/sse.md` when work begins (and note it in AI.state).

Status: **design complete, build gated on §0.** Read §0 first — it argues the
whole feature may not be worth building yet. If we proceed, open decisions are
in §10 and the Last-Event-ID sub-analysis is §9.

## 0. Is this feature worth building at all?

The §9 analysis is about one mechanism *inside* SSE. This section is the bigger
question the rest of the spec presumes a "yes" to: **should we implement SSE
serving at all, or is the 5 s poll good enough?** Honest answer: **not yet —
defer until §0.3's two conditions are met.** The reasoning:

### 0.1 What it actually costs

This is not a one-binding feature. It adds, permanently:

- **A second mode in the Router.** Today `ServeHTTP` does one thing: a brief,
  deadline-bounded round-trip through `reqCh` (`router.go:118-167`). SSE bolts on
  a structurally different path — subscribe, stream, heartbeat, evict, hold open
  for hours. The handler that was easy to reason about now has two lifetimes.
- **A new concurrency surface outside the LState.** The broker, per-subscriber
  buffered channels, slow-client eviction, heartbeat tickers, goroutine-leak
  avoidance on client disconnect. None of it is hard in isolation; all of it is
  new state to keep correct under `-race`, reload, and stop.
- **A second code path we have to keep forever**, because of §0.2.

Against the project's stated rule — *avoid needless complexity* — that is a real
debit, not a rounding error.

### 0.2 What it buys — and the ingress trap

The payoff for the **known** use case is thin. This is a single-user home system
with a handful of browser tabs. A 5 s poll of `/api/state` is negligible load,
and for a thermostat, "the setpoint updates within 5 s" is already fine — nobody
is watching a radiator change temperature in real time. The win SSE offers
(instant push, no wasted requests) is a nice-to-have here, not a need.

Worse, the **primary deployment can't be guaranteed to support it.** Ingress is
unverified (§8), and buffering proxies silently break SSE. If streaming doesn't
survive ingress, we ship SSE *and keep the poll as a fallback* — now we maintain
**both** paths to gain nothing on the deployment most users run. That is the
worst outcome, and it's a live possibility, not a tail risk.

### 0.3 When it flips to "yes"

Two conditions, both required:

1. **A use case the poll genuinely can't serve.** Not the thermostat — something
   latency- or volume-sensitive: a live sensor dashboard, instant button/feedback
   UI, a log/notification tail, anything where 5 s lag or constant re-polling is
   visibly wrong. As long as the thermostat is the only consumer, the poll wins
   on cost.
2. **Ingress streaming is confirmed** by a real-deploy smoke test (§8). Cheap to
   check, and it gates everything: if frames don't flush through ingress, the
   feature can't replace polling and shouldn't be built to sit beside it.

### 0.4 Verdict

**Defer.** The architecture in this spec is sound and the broker is small, but
sound-and-small is not the same as worth-it. For the only consumer we have, the
5 s poll is simpler, already works, and works *through ingress* — which SSE may
not. Keep this spec as the ready-to-execute design, run the §8 ingress
experiment when convenient, and build §11's milestones the day a script appears
that the poll actually fails. Until then, building it is paying real complexity
for marginal, possibly-undeliverable gain.

(The §8 ingress smoke test is worth doing **independently** of SSE — it also
de-risks the existing request/response UI under ingress, which is likewise
unverified.)

## 1. Goal

Let a Lua script **push** events out to connected browser/HTTP clients over a
long-lived [Server-Sent Events](https://developer.mozilla.org/en-US/docs/Web/API/Server-sent_events)
stream, instead of the client polling. The motivating case is the thermostat
UI, which today re-fetches `/api/state` every 5 seconds (`AI.state`, M6). SSE
replaces that poll with server push: the script publishes when state actually
changes, the browser receives it immediately.

The browser side is stock and requires no library:

```js
const stream = new EventSource("./api/events");   // relative → works LAN + ingress
stream.onmessage = (msg) => render(JSON.parse(msg.data));
```

## 2. The invariant this must not break

Everything in the daemon rests on one rule: **one `*lua.LState` per script,
owned by exactly one goroutine, and that goroutine must never block.** The
existing HTTP request path honors it by keeping the round-trip brief — the body
is read on the HTTP goroutine, marshaled across `reqCh`, handled *synchronously*
by the run loop, answered with one `response`, done (`router.go:118-167`,
`runner.go:269-285`). A 5 s deadline bounds the client's wait.

An SSE connection is the opposite: it is held open for minutes or hours,
dribbling out frames as things happen. If that connection lived on the script
goroutine, the run loop would block forever and every event, timer, and request
behind it would starve. **The connection therefore lives entirely on the HTTP
goroutine and never touches the LState.** The script only ever *publishes* — a
cheap, non-blocking enqueue. Go owns the open socket and the fan-out.

```
browser ──GET /api/events──▶ HTTP goroutine ──subscribe──▶ Broker (Go, shared)
                                   ▲                            ▲
                              streams frames               Publish(topic, bytes)
                              until disconnect                  │
                                                          script run loop
                                                   ha.sse_publish("/api/events", {...})
```

The script never holds a `ResponseWriter`, never sees a connection open or
close, and never learns how many clients are attached. That asymmetry —
fire-and-forget publish, Go-owned streaming — is what preserves the invariant.

## 3. Locked decisions

| Decision | Choice |
|----------|--------|
| Connection ownership | **HTTP goroutine only.** Never crosses into the LState. The script publishes; Go streams. |
| Topic model | **One endpoint = one broadcast topic.** Every client on a prefix gets every message. Topic key is `scriptID\|prefix`. Per-client targeting is out of scope (§10.1). |
| Reload behavior | **Reload/stop drops live streams** (`DropScript`). The new script re-registers; browsers auto-reconnect via SSE's built-in retry within ~1 s. Keeping sockets open across a topic-identity change is where bugs live. |
| Reconnect freshness | **Last-value cache, not Last-Event-ID resumption.** The broker keeps only the *last* message per topic and sends it to each new subscriber. No backlog, no `id:`, no `Last-Event-ID` honoring. Rationale + complexity analysis in §9. |
| Slow-client policy | **Close-on-full.** Per-subscriber buffered channel (cap 16); when it fills, the subscriber is disconnected rather than dropping frames into a gappy stream. The browser reconnects and gets fresh state. Deliberately *different* from the event channel's drop-and-warn (`runner.go:112-118`), because there the consumer is the trusted run loop; here it is an arbitrary slow browser. |
| Heartbeats | **Mandatory.** `: keepalive\n\n` comment frame on a 20 s ticker, plus `X-Accel-Buffering: no` and `Cache-Control: no-cache` headers. Required because the ingress proxy path is unverified (§8) and buffering proxies silently break SSE. |
| Payload encoding | **Always JSON.** `sse_publish` runs the Lua value through `luaMarshal` (`json.Deterministic(true)`, same path as `json`/route bodies). The browser always `JSON.parse`es. |
| Undeclared publish | **Lua error.** `serve_sse` registers the topic, so `sse_publish` to a prefix that was never declared is a typo and raises. Publishing to a declared topic with zero subscribers is a no-op. |
| Method | **GET only.** SSE is a GET by definition. |

## 4. Lua API

```lua
-- Declare a broadcast SSE endpoint. GET only. Call at load time, like ha.serve.
ha.serve_sse("/api/events")

-- Publish to every client connected to that prefix. Call from any run-loop
-- context: a timer, a state-change handler, a request handler.
ha.sse_publish("/api/events", { temp = 21.5, zone = "living", mode = "heat" })
```

- `serve_sse(prefix)` records an SSE route (cached like normal routes, registered
  with the Router on `afterLoad`, unregistered on stop) **and** registers the
  topic `scriptID|prefix` with the broker.
- `sse_publish(prefix, value)` marshals `value` to JSON and hands `(topic, bytes)`
  to the broker. Non-blocking. Errors only on an undeclared prefix.
- Multiple independent streams = multiple `serve_sse` prefixes.
- No `event:` name or explicit `id:` in v1 — every frame is a default-typed
  `message` event carrying JSON in `data:`. (Adding a named-event variant later
  is additive and cheap; see §10.2.)

## 5. New Go pieces

### 5.1 `internal/lua/broker.go`

```go
type Broker struct {
    mu     sync.Mutex
    topics map[string]*topic        // key: scriptID|prefix
}

type topic struct {
    subs map[*subscriber]struct{}
    last []byte                     // last-value cache (§9)
}

type subscriber struct {
    ch chan []byte                  // buffered, cap 16
}
```

- `Subscribe(key string) (*subscriber, []byte)` — registers a subscriber, returns
  it plus the cached last value (may be nil) for immediate replay.
- `Publish(key string, payload []byte)` — stores `last`, then non-blocking send to
  each subscriber; **a full channel evicts that subscriber** (close + delete).
- `Unsubscribe(key, *subscriber)` — on client disconnect.
- `DropScript(scriptID string)` — close every subscriber of that script's topics
  and delete the topics; called on stop/reload.
- `Declare(key string)` / `Declared(key) bool` — so `sse_publish` can reject an
  undeclared prefix.

Process-wide singleton, wired in `main` and shared across scripts, exactly like
`Router`. Its mutex is the only synchronization; `sse_publish` is already
serialized per script (it runs on the run loop), but the broker fans out across
scripts and HTTP goroutines, so it owns the lock.

### 5.2 Router SSE branch (`router.go`)

`routeBinding` gains an `sse bool`; `match` returns the matched binding (prefix +
sse flag), not just the scriptID. In `ServeHTTP`, an SSE match **skips the
`reqCh` round-trip** and calls:

```go
func (rt *Router) streamSSE(w http.ResponseWriter, r *http.Request, key string)
```

which:
1. Sets headers: `Content-Type: text/event-stream`, `Cache-Control: no-cache`,
   `Connection: keep-alive`, `X-Accel-Buffering: no`. Writes 200 + flush.
2. `sub, last := broker.Subscribe(key)`; `defer broker.Unsubscribe`.
3. If `last != nil`, writes it as the first frame.
4. Loops on `select`:
   - `payload := <-sub.ch` → write `data: <line>\n` per line + `\n`, flush.
   - heartbeat ticker → write `: keepalive\n\n`, flush.
   - `<-r.Context().Done()` → return (client gone).
   - a write error → return (also client gone).

Flushing uses `http.ResponseController{w}.Flush()`. The `http.Server` already
sets only `ReadHeaderTimeout` (no `WriteTimeout`, `server.go`), so long streams
are not killed server-side — nothing to change there.

### 5.3 `haAPI` + bindings (`api_ha.go`)

`haAPI` gains a `broker *Broker` field (wired in `NewRunner`/`Deps`, alongside
`callService`/`fireEvent`). `serve_sse` and `sse_publish` are registered next to
`ha.serve`. `routeSpecs()` carries the `sse` flag through to `RouteSpec` so the
supervisor registers SSE routes with the Router.

### 5.4 Supervisor wiring (`supervisor.go`)

- `afterLoad` already registers routes; SSE routes ride the same `RouteSpec`
  slice (now flag-bearing). No new call site.
- `StopScript` (and thus `Reload`) calls `broker.DropScript(id)` alongside
  `Router.Unregister(id)`, under the same `s.mu`, so a live stream is torn down
  in lockstep with the route removal — no window where a connection outlives its
  topic.

## 6. Frame format

JSON is compact and single-line, so the common frame is just:

```
data: {"temp":21.5,"zone":"living"}\n\n
```

`streamSSE` still splits the payload on `\n` and prefixes each line with `data: `
(per the SSE spec), so a multi-line string payload is also correct. No `id:` and
no `event:` line in v1.

## 7. Worked example — thermostat

```lua
ha.serve_sse("/api/events")

local function publish_state()
  ha.sse_publish("/api/events", build_state_table())  -- same table /api/state returns
end

ha.every("1m", publish_state)        -- existing desired() tick already runs here
-- also call publish_state() right after a boost/override/schedule change
```

Browser drops the 5 s poll loop and renders on `stream.onmessage`. The
last-value cache means a freshly-opened tab paints immediately instead of waiting
up to a minute for the next tick. The 5 s poll stays in the page as a documented
fallback until ingress streaming is confirmed (§8).

## 8. Biggest risk — ingress

`AI.state` flags that **ingress has never been verified against a live
Supervisor**. SSE is more sensitive to proxy buffering than the request/response
UI: if HA's ingress buffers the response body, frames never reach the browser
and the stream looks dead. Mitigations are baked in (heartbeats,
`X-Accel-Buffering: no`, `Cache-Control: no-cache`), but this **must** get a
real-deploy smoke test before we trust it, and the polling fallback stays until
then. The LAN port (`http_port`) has no proxy in front of it and is the
known-good path.

## 9. Should we do Last-Event-ID resumption? (the complexity question)

The "proper" SSE reconnection mechanism is **Last-Event-ID**: the server tags
each event with `id: <n>`; on reconnect the browser *automatically* sends a
`Last-Event-ID: <n>` request header; the server is then expected to **replay
every event after `<n>`**. It is the spec's intended way to make a stream
gap-free across drops. The temptation is real — it's built into `EventSource`,
costs nothing on the client, and "feels like the right way."

We are **not** implementing it. Last-value cache only. Here is the reasoning,
because this is the one place the design could plausibly go either way.

**What Last-Event-ID actually costs us:**

1. **A per-topic ordered backlog.** "Replay everything after N" requires keeping
   the last *K* messages per topic in a ring buffer, not just the last one —
   more memory and more bookkeeping, sized by a *K* we'd have to guess.
2. **Monotonic IDs with a lifetime problem.** IDs must be monotonic, but our
   topics are not durable. A daemon restart resets them; a script **reload**
   calls `DropScript` and recreates the topic, so the backlog is gone and the
   ID space restarts. So the resumption guarantee would hold *only within a
   single uninterrupted topic lifetime* — precisely the window in which drops
   are least likely. We'd be paying for a guarantee we can't actually keep
   across the events (restart, reload) that most often cause a reconnect.
3. **Wrong semantics for this system.** ha-lua's streams are **state
   snapshots**, not append-only event logs. After a 30 s dropout the thermostat
   UI does not want the five stale setpoints it missed — it wants the *current*
   one. Replaying intermediate states is not just wasted work, it's actively
   the wrong answer; the client would animate through history before landing on
   "now." The last-value cache delivers exactly the right thing on reconnect:
   "here is where things stand," in one frame.

**What we lose by skipping it:** nothing this system needs. SSE's built-in
auto-reconnect still fires (EventSource retries after ~3 s on its own); the
reconnect is a fresh GET that immediately receives the last-value frame. So
reconnect-to-fresh-state already works end to end — it just routes through the
last-value cache instead of an ID-keyed backlog.

**When the calculus would flip:** if a future endpoint is a genuine
*append-only event log* where each message is independent data and a gap is data
loss — an activity feed, an audit trail, a chat transcript. That is the use case
Last-Event-ID was designed for, and none of the current or planned scripts are
it. If one appears, add resumption *for that endpoint* behind an opt-in flag;
don't retrofit the whole broker.

**Verdict: not worth the extra complexity.** The last-value cache is strictly
simpler (one `[]byte` per topic vs. a sized ring buffer + ID allocator + replay
path) *and* gives better behavior for a state-broadcast UI. It is one of the
rare cases where the cheaper option is also the more correct one. Revisit only
when a real append-only stream lands.

## 10. Open / deferred (do not build without a use case)

1. **Per-client targeting / unicast.** v1 is broadcast only. Targeting a
   specific connection would require delivering connect/disconnect to the script
   as (lossy) events on `ch` and tracking client identity — real complexity for
   a case no current script needs.
2. **Named events (`event:`) and explicit IDs.** Additive later; v1 sends
   default `message` frames only.
3. **`ha.sse_clients(prefix)` count** for debugging — trivial to add to the
   broker if it proves useful; omitted to keep the surface minimal.
4. **Last-Event-ID resumption** — see §9; deferred until a true event-log
   endpoint exists.

## 11. Milestones (bisectable commits)

1. `lua: add SSE broker` — `broker.go` + tests (fan-out, last-value replay,
   close-on-full eviction, `DropScript`), pure Go, `-race`.
2. `lua: stream SSE responses from the router` — Router SSE branch + `streamSSE`
   (headers, flush, heartbeat, disconnect). `httptest` against a stub broker.
3. `lua: add ha.serve_sse and ha.sse_publish` — bindings + topic declaration +
   `routeSpecs` plumbing. Load a real script, connect via `httptest`, publish,
   assert the frame.
4. `lua: drop SSE subscribers on script stop/reload` — `DropScript` in
   `StopScript`; `TestSSEReloadDropsConnections` mirroring
   `TestRouterReloadReRegisters`.
5. `thermostat: push state over SSE instead of polling` *(separate track)* —
   convert the UI to `EventSource`, publish on tick + on boost/override change,
   keep the poll as fallback.
6. `docs: SSE serving` — DOCS.md (Web UIs), CHANGELOG, `config.yaml` version →
   **2.1.0** (new feature, backwards-compatible minor bump).

## 12. Test plan

- **Broker:** table-driven fan-out; last-value replay to a late subscriber;
  slow-subscriber eviction (fill the buffer, assert close); concurrent
  publish/subscribe/drop under `-race`.
- **Stream:** `httptest.Server`; assert `Content-Type: text/event-stream`, a
  published frame arrives, a heartbeat arrives, client-cancel ends the streaming
  goroutine (no leak — verify the broker has zero subscribers after).
- **Bindings:** real script with `serve_sse` + a timer that publishes; connect,
  read a frame, assert JSON shape. `sse_publish` to an undeclared prefix raises.
- **Reload:** `TestSSEReloadDropsConnections` — open a stream, reload the script,
  assert the old connection ends and a re-connect hits the new topic.
</content>
</invoke>
