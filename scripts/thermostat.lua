-- thermostat.lua
--
-- The heating controller. It owns the schedule / boost / manual-override
-- dimension of each zone's setpoint; heating_windows.lua owns the window
-- dimension. The split is clean: this script computes one desired setpoint per
-- zone, publishes it to global so the window script can restore it, and writes
-- it to the climate entity only while no window in the zone is open.
--
-- See thermostat-ui-spec.md for the full design. This file is the controller
-- (the 1-minute tick + desired() engine + manual-override detection); the HTTP
-- API and the single-page UI are added on top of it.

local zones = require "zones"
local schedule = require "schedule"

local Z = zones.zones

-- Per-zone store keys. Schedule/boost/override/comfort all live in this
-- script's KV store; the published desired lives in `global` (shared).
local function sched_key(z) return "schedule:" .. z end
local function boost_key(z) return "boost:" .. z end
local function override_key(z) return "override:" .. z end
local function comfort_key(z) return "comfort:" .. z end

-- now_parts returns the current time userdata plus the schedule's weekday
-- (0=Mon..6=Sun, converted from Go's Sunday-first weekday) and minute-of-day.
local function now_parts()
  local n = time.now()
  local dow = (n:weekday() + 6) % 7
  return n, dow, n:hour() * 60 + n:minute()
end

local function parse_time(s)
  if type(s) ~= "string" then return nil end
  local t = time.parse(time.RFC3339, s)
  return t -- nil on parse failure
end

-- comfort returns the zone's UI-settable boost temperature, seeding the default
-- the first time before the user touches the stepper.
local function comfort(z)
  local v = store.get(comfort_key(z))
  if type(v) == "number" then return v end
  return zones.default_comfort
end

-- mode returns the climate entity's hvac mode ("heat"/"off"/...) or nil if the
-- entity is not yet seeded.
local function mode(z)
  local c = ha.get_state(Z[z].climate)
  if c == nil then return nil end
  return c.state
end

local function current_target(z)
  local c = ha.get_state(Z[z].climate)
  if c and c.attributes then return c.attributes.temperature end
  return nil
end

-- any_window_open reports whether any window in the zone is definitely open.
-- A not-yet-seeded sensor (nil) counts as closed for the write decision.
local function any_window_open(z)
  for _, w in ipairs(Z[z].windows) do
    local s = ha.get_state(w)
    if s ~= nil and s.state == "on" then return true end
  end
  return false
end

-- any_window_unknown reports whether any window sensor has no state yet. Used
-- only to suppress manual-override detection until the sensors have seeded.
local function any_window_unknown(z)
  for _, w in ipairs(Z[z].windows) do
    if ha.get_state(w) == nil then return true end
  end
  return false
end

local function load_schedule(z)
  local s = store.get(sched_key(z))
  if type(s) == "table" and type(s.days) == "table" then return s.days end
  return {}
end

-- active_boost returns the live boost table for the zone, or nil. An expired
-- boost is cleared as a side effect so the zone reverts to schedule.
local function active_boost(z, now)
  local b = store.get(boost_key(z))
  if type(b) ~= "table" or not b.active or type(b.ends_at) ~= "string" then
    return nil
  end
  local ends = parse_time(b.ends_at)
  if ends == nil then
    store.delete(boost_key(z))
    return nil
  end
  if now:before(ends) then return b end
  store.delete(boost_key(z))
  return nil
end

-- active_override returns the live manual-override table, or nil, clearing it
-- once its `expires` instant has passed. (We avoid the field name "until"
-- because it is a Lua keyword.)
local function active_override(z, now)
  local o = store.get(override_key(z))
  if type(o) ~= "table" or type(o.temp) ~= "number" or type(o.expires) ~= "string" then
    return nil
  end
  local exp = parse_time(o.expires)
  if exp == nil then
    store.delete(override_key(z))
    return nil
  end
  if now:before(exp) then return o end
  store.delete(override_key(z))
  return nil
end

-- desired implements §4.1: boost beats override beats schedule. Returns the
-- temperature and its source string ("boost"/"override"/"schedule"), or nil if
-- the zone has no schedule at all.
local function desired(z, now, dow, minute)
  if active_boost(z, now) then return comfort(z), "boost" end
  local o = active_override(z, now)
  if o then return o.temp, "override" end
  local temp = schedule.resolve(load_schedule(z), dow, minute)
  if temp ~= nil then return temp, "schedule" end
  return nil, nil
end

local function set_temp(z, temp)
  ha.call_service("climate", "set_temperature", {
    entity_id = Z[z].climate,
    temperature = temp,
  })
end

-- apply_zone publishes the zone's desired setpoint and writes it to the climate
-- entity when the mode is heat and no window is open. The write is skipped when
-- the value is unchanged so we don't spam set_temperature.
local function apply_zone(z, now, dow, minute)
  local d = desired(z, now, dow, minute)
  if d == nil then return end
  global.set(zones.desired_key(z), d)
  if mode(z) ~= "heat" then return end
  if any_window_open(z) then return end -- window script's territory
  local cur = current_target(z)
  if cur == nil or math.abs(cur - d) > 0.05 then
    set_temp(z, d)
  end
end

-- The single tick that drives everything (§8): recompute, publish and (maybe)
-- write every zone. Boost/override expiry is handled inside desired().
local function tick()
  local now, dow, minute = now_parts()
  for z in pairs(Z) do
    apply_zone(z, now, dow, minute)
  end
end

ha.every("1m", tick)

-- Manual setpoint change detection (§9): the controller is the only thing that
-- writes `desired`, and it always writes exactly `desired`, so a climate target
-- that differs from the published desired is an external change by the user. It
-- becomes an ad-hoc override that holds until the next schedule transition.
for z, conf in pairs(Z) do
  ha.on_state_change(conf.climate, function(data)
    local ns = data.new_state
    if ns == nil or ns.attributes == nil then return end
    if ns.state ~= "heat" then return end
    local target = ns.attributes.temperature
    if type(target) ~= "number" then return end

    local now, dow, minute = now_parts()
    if active_boost(z, now) then return end -- boost wins; ignore dial nudges
    -- Window open or not-yet-seeded: that's the window script's 15°C territory.
    if any_window_open(z) or any_window_unknown(z) then return end

    local pub = global.get(zones.desired_key(z))
    -- Float tolerance: our own write (and the window restore) set target == pub
    -- exactly, but 21 vs 21.0 must not look like a manual change.
    if type(pub) == "number" and math.abs(target - pub) <= 0.1 then return end

    local _, _, mins_to_next = schedule.resolve(load_schedule(z), dow, minute)
    local hold = mins_to_next ~= nil and mins_to_next * 60 or 24 * 3600
    store.set(override_key(z), {
      temp = target,
      expires = now:add(hold):format(time.RFC3339),
    })
    apply_zone(z, now, dow, minute) -- republish the new desired immediately
  end)
end

-- ---------------------------------------------------------------------------
-- HTTP API (§6). Handlers run on this script's goroutine, so they may use any
-- ha.*/store.* call directly. All mutating endpoints re-apply the affected zone
-- and return the full state so the UI can refresh in one round-trip.
-- ---------------------------------------------------------------------------

local JSON_HDR = { ["Content-Type"] = "application/json" }
local TEXT_HDR = { ["Content-Type"] = "text/plain" }

local function json_ok(tbl)
  return 200, json.encode(tbl), JSON_HDR
end

local function bad(msg)
  return 400, msg, TEXT_HDR
end

-- zone_state builds the per-zone status block for GET /api/state.
local function zone_state(z, now, dow, minute)
  local c = ha.get_state(Z[z].climate)
  local md = c and c.state or "unknown"
  local cur, target
  if c and c.attributes then
    cur = c.attributes.current_temperature
    target = c.attributes.temperature
  end
  local days = load_schedule(z)
  local sched_temp, now_index = schedule.resolve(days, dow, minute)

  local boost_tbl
  local b = active_boost(z, now)
  if b then
    local rem = 0
    local ends = parse_time(b.ends_at)
    if ends ~= nil then
      local s = ends:sub(now)
      if s > 0 then rem = math.floor(s) end
    end
    boost_tbl = { active = true, ends_at = b.ends_at, remaining_s = rem }
  end

  return {
    mode = md,
    current_temp = cur,
    target = target,
    comfort_temp = comfort(z),
    window_open = any_window_open(z),
    scheduled_temp = sched_temp,
    today = schedule.day_list(days, dow),
    now_index = now_index,
    boost = boost_tbl,
  }
end

local function full_state()
  local now, dow, minute = now_parts()
  local zs = {}
  for z in pairs(Z) do
    zs[z] = zone_state(z, now, dow, minute)
  end
  return { zones = zs }
end

-- decode_body parses a JSON request body into a table, or returns nil.
local function decode_body(req)
  local ok, b = pcall(json.decode, req.body)
  if ok and type(b) == "table" then return b end
  return nil
end

ha.serve("GET", "/api/state", function()
  return json_ok(full_state())
end)

ha.serve("POST", "/api/boost", function(req)
  local b = decode_body(req)
  if b == nil then return bad("invalid JSON body") end
  local z = b.zone
  if type(z) ~= "string" or Z[z] == nil then return bad("unknown zone") end
  if type(b.minutes) ~= "number" or b.minutes <= 0 or b.minutes > 1440 then
    return bad("minutes must be 1..1440")
  end
  local now, dow, minute = now_parts()
  store.set(boost_key(z), {
    active = true,
    ends_at = now:add(b.minutes * 60):format(time.RFC3339),
  })
  store.delete(override_key(z)) -- a boost outranks and clears any override
  apply_zone(z, now, dow, minute)
  return json_ok(full_state())
end)

-- Registered after /api/boost; the router's longest-prefix match sends
-- /api/boost/cancel here and bare /api/boost to the boost handler.
ha.serve("POST", "/api/boost/cancel", function(req)
  local b = decode_body(req)
  if b == nil then return bad("invalid JSON body") end
  local z = b.zone
  if type(z) ~= "string" or Z[z] == nil then return bad("unknown zone") end
  store.delete(boost_key(z))
  local now, dow, minute = now_parts()
  apply_zone(z, now, dow, minute)
  return json_ok(full_state())
end)

ha.serve("PUT", "/api/settings", function(req)
  local b = decode_body(req)
  if b == nil then return bad("invalid JSON body") end
  local z = b.zone
  if type(z) ~= "string" or Z[z] == nil then return bad("unknown zone") end
  if type(b.comfort_temp) ~= "number" or b.comfort_temp < 5 or b.comfort_temp > 35 then
    return bad("comfort_temp out of range (5..35)")
  end
  store.set(comfort_key(z), b.comfort_temp)
  local now, dow, minute = now_parts()
  apply_zone(z, now, dow, minute) -- if a boost is active, the new comfort applies now
  return json_ok(full_state())
end)

ha.serve("GET", "/api/schedule", function(req)
  local z = req.query and req.query.zone
  if type(z) == "string" and z ~= "" then
    if Z[z] == nil then return bad("unknown zone") end
    return json_ok({ zone = z, days = load_schedule(z) })
  end
  local all = {}
  for zz in pairs(Z) do
    all[zz] = load_schedule(zz)
  end
  return json_ok({ schedules = all })
end)

ha.serve("PUT", "/api/schedule", function(req)
  local b = decode_body(req)
  if b == nil then return bad("invalid JSON body") end
  local z = b.zone
  if type(z) ~= "string" or Z[z] == nil then return bad("unknown zone") end
  local valid, msg = schedule.validate(b.days)
  if not valid then return bad("invalid schedule: " .. msg) end
  store.set(sched_key(z), { days = b.days })
  local now, dow, minute = now_parts()
  apply_zone(z, now, dow, minute)
  return json_ok(full_state())
end)

-- ---------------------------------------------------------------------------
-- The single-page UI (§7). One self-contained HTML document — inline vanilla
-- JS/CSS, no build step, no external assets. All fetches are RELATIVE
-- (./api/...) so the page works unchanged under both entry points: the stable
-- LAN port and the ingress base path the Supervisor rotates.
-- ---------------------------------------------------------------------------

local PAGE = [==[
<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1, viewport-fit=cover">
<title>Heating</title>
<style>
  :root { --bg:#f4f5f7; --card:#fff; --ink:#1c1e21; --muted:#76787d;
          --accent:#e8543f; --accent-ink:#fff; --line:#e3e5e8; --now:#fff1ee; }
  * { box-sizing: border-box; }
  body { margin:0; background:var(--bg); color:var(--ink);
         font:16px/1.4 system-ui,-apple-system,Segoe UI,Roboto,sans-serif;
         padding:12px; max-width:600px; margin:0 auto; }
  h1 { font-size:1.3rem; margin:8px 4px 16px; }
  .card { background:var(--card); border-radius:14px; padding:14px;
          margin-bottom:14px; box-shadow:0 1px 3px rgba(0,0,0,.08); }
  .head { display:flex; justify-content:space-between; align-items:baseline;
          margin-bottom:12px; }
  .zone { font-weight:600; font-size:1.1rem; }
  .status { color:var(--muted); font-size:.95rem; }
  .status b { color:var(--ink); }
  .boost-row { display:flex; gap:8px; margin-bottom:12px; }
  .boost-row button { flex:1; padding:16px 0; font-size:1.1rem; font-weight:600;
          border:none; border-radius:10px; background:var(--accent);
          color:var(--accent-ink); cursor:pointer; }
  .boost-row button.alt { background:#f0f1f3; color:var(--ink); flex:0 0 56px; }
  .boosting { display:flex; align-items:center; gap:12px; margin-bottom:12px;
          background:var(--now); border-radius:10px; padding:12px; }
  .boosting .cd { font-size:1.4rem; font-weight:700; font-variant-numeric:tabular-nums; }
  .boosting button { margin-left:auto; padding:10px 16px; border:none;
          border-radius:8px; background:#fff; cursor:pointer; }
  .stepper { display:flex; align-items:center; gap:14px; margin-bottom:12px; }
  .stepper .lbl { color:var(--muted); }
  .stepper button { width:44px; height:44px; font-size:1.4rem; border:1px solid var(--line);
          border-radius:10px; background:#fff; cursor:pointer; }
  .stepper .val { font-size:1.3rem; font-weight:600; min-width:60px; text-align:center; }
  .today { display:flex; flex-wrap:wrap; gap:6px 10px; font-size:.9rem;
           color:var(--muted); margin-bottom:8px; }
  .today .step.now { color:var(--accent); font-weight:700; }
  .muted { color:var(--muted); font-style:italic; margin-bottom:8px; }
  .editlink { display:block; text-align:right; font-size:.9rem; color:#2b6cff;
              cursor:pointer; user-select:none; }
  .editor { border-top:1px solid var(--line); margin-top:12px; padding-top:12px; }
  .day { margin-bottom:10px; }
  .day .dname { font-weight:600; margin-bottom:4px; }
  .row { display:flex; gap:8px; align-items:center; margin-bottom:6px; }
  .row input[type=time] { flex:1; padding:8px; border:1px solid var(--line); border-radius:8px; }
  .row input[type=number] { width:80px; padding:8px; border:1px solid var(--line); border-radius:8px; }
  .row .rm, .day .add { border:none; background:#f0f1f3; border-radius:8px;
            padding:8px 12px; cursor:pointer; }
  .editor-actions { display:flex; gap:8px; margin-top:8px; }
  .editor-actions button { flex:1; padding:12px; border:none; border-radius:10px;
            font-weight:600; cursor:pointer; }
  .editor-actions .save { background:var(--accent); color:var(--accent-ink); }
  .editor-actions .cancel { background:#f0f1f3; }
  #err { position:fixed; left:12px; right:12px; bottom:12px; background:#c0392b;
         color:#fff; padding:12px; border-radius:10px; display:none; }
</style>
</head>
<body>
<h1>Heating</h1>
<div id="app"></div>
<div id="err"></div>
<script>
const PRESETS = [10, 30, 60];
const DAYS = ["Mon","Tue","Wed","Thu","Fri","Sat","Sun"];
let editing = null;        // zone whose schedule editor is open (suppresses polling)
let lastState = null;

const $ = (h) => { const d = document.createElement("div"); d.innerHTML = h.trim(); return d.firstChild; };
const esc = (s) => String(s).replace(/[&<>"]/g, c => ({"&":"&amp;","<":"&lt;",">":"&gt;",'"':"&quot;"}[c]));
const title = (z) => z.charAt(0).toUpperCase() + z.slice(1).replace(/_/g, " ");

function showErr(msg) {
  const e = document.getElementById("err");
  e.textContent = msg; e.style.display = "block";
  setTimeout(() => { e.style.display = "none"; }, 4000);
}

async function send(path, method, payload) {
  const r = await fetch("./" + path, {
    method, headers: { "Content-Type": "application/json" },
    body: JSON.stringify(payload),
  });
  if (!r.ok) { showErr("Error " + r.status + ": " + (await r.text())); return null; }
  return r.json();
}

async function poll() {
  if (editing) return;                 // don't clobber an open editor
  try {
    const r = await fetch("./api/state");
    if (r.ok) { lastState = await r.json(); render(lastState); }
  } catch (e) { /* transient; next poll retries */ }
}

function fmtCountdown(sec) {
  sec = Math.max(0, Math.floor(sec));
  const m = Math.floor(sec / 60), s = sec % 60;
  return m + ":" + String(s).padStart(2, "0");
}

function render(state) {
  if (editing) return;
  const app = document.getElementById("app");
  app.innerHTML = "";
  Object.keys(state.zones).sort().forEach(z => app.appendChild(zoneCard(z, state.zones[z])));
}

function zoneCard(z, s) {
  const card = document.createElement("div");
  card.className = "card";

  const cur = (s.current_temp != null) ? s.current_temp.toFixed(1) + "°" : "";
  card.appendChild($(
    '<div class="head"><span class="zone">' + esc(title(z)) + '</span>' +
    '<span class="status"><b>' + esc(s.mode) + '</b> ' + cur + '</span></div>'
  ));

  const controllable = s.mode === "heat";
  if (!controllable) {
    card.appendChild($('<div class="muted">mode ' + esc(s.mode) + ' — not controlled</div>'));
  } else if (s.boost && s.boost.active) {
    const ends = Date.now() + (s.boost.remaining_s || 0) * 1000;
    const row = $('<div class="boosting"><span class="cd" data-ends="' + ends + '">' +
      fmtCountdown(s.boost.remaining_s || 0) + '</span><span>boosting to ' +
      esc(s.comfort_temp) + '°</span><button>cancel</button></div>');
    row.querySelector("button").onclick = () =>
      send("api/boost/cancel", "POST", { zone: z }).then(st => st && (lastState = st, render(st)));
    card.appendChild(row);
  } else {
    const row = document.createElement("div");
    row.className = "boost-row";
    PRESETS.forEach(min => {
      const b = $('<button>' + min + 'm</button>');
      b.onclick = () => send("api/boost", "POST", { zone: z, minutes: min })
        .then(st => st && (lastState = st, render(st)));
      row.appendChild(b);
    });
    const custom = $('<button class="alt" title="custom minutes">⌨</button>');
    custom.onclick = () => {
      const v = prompt("Boost for how many minutes?", "45");
      const n = parseInt(v, 10);
      if (n > 0) send("api/boost", "POST", { zone: z, minutes: n })
        .then(st => st && (lastState = st, render(st)));
    };
    row.appendChild(custom);
    card.appendChild(row);
  }

  if (controllable) {
    const step = document.createElement("div");
    step.className = "stepper";
    step.appendChild($('<span class="lbl">boost to</span>'));
    const minus = $('<button>−</button>'), plus = $('<button>+</button>');
    const val = $('<span class="val">' + esc(s.comfort_temp) + '°</span>');
    const setC = (delta) => {
      const nv = Math.round((Number(s.comfort_temp) + delta) * 2) / 2;
      send("api/settings", "PUT", { zone: z, comfort_temp: nv })
        .then(st => st && (lastState = st, render(st)));
    };
    minus.onclick = () => setC(-0.5);
    plus.onclick = () => setC(0.5);
    step.appendChild(minus); step.appendChild(val); step.appendChild(plus);
    card.appendChild(step);
  }

  const today = Array.isArray(s.today) ? s.today : [];
  if (today.length) {
    const strip = today.map((t, i) =>
      '<span class="step' + (i === s.now_index ? " now" : "") + '">' +
      (i === s.now_index ? "▶ " : "") + esc(t.time) + " " + esc(t.temp) + "°</span>"
    ).join("");
    card.appendChild($('<div class="today">' + strip + '</div>'));
  } else {
    card.appendChild($('<div class="today">no schedule set</div>'));
  }

  const edit = $('<span class="editlink">✎ schedule</span>');
  edit.onclick = () => openEditor(z, card);
  card.appendChild(edit);
  return card;
}

function normalizeDays(days) {
  const out = {};
  for (let d = 0; d < 7; d++) {
    const list = days && days[String(d)];
    out[d] = Array.isArray(list) ? list.slice() : [];
  }
  return out;
}

async function openEditor(z, card) {
  const r = await fetch("./api/schedule?zone=" + encodeURIComponent(z));
  if (!r.ok) { showErr("Could not load schedule"); return; }
  const data = await r.json();
  editing = z;
  renderEditor(z, normalizeDays(data.days), card);
}

function renderEditor(z, days, card) {
  let ed = card.querySelector(".editor");
  if (ed) ed.remove();
  ed = document.createElement("div");
  ed.className = "editor";

  for (let d = 0; d < 7; d++) {
    const day = document.createElement("div");
    day.className = "day";
    day.appendChild($('<div class="dname">' + DAYS[d] + '</div>'));
    days[d].forEach((t, i) => day.appendChild(transitionRow(days, d, i, z, card)));
    const add = $('<button class="add">+ add</button>');
    add.onclick = () => { days[d].push({ time: "07:00", temp: 21 }); renderEditor(z, days, card); };
    day.appendChild(add);
    ed.appendChild(day);
  }

  const actions = document.createElement("div");
  actions.className = "editor-actions";
  const save = $('<button class="save">Save</button>');
  const cancel = $('<button class="cancel">Cancel</button>');
  save.onclick = async () => {
    const payload = {};
    for (let d = 0; d < 7; d++) payload[String(d)] = days[d];
    const st = await send("api/schedule", "PUT", { zone: z, days: payload });
    if (st) { editing = null; lastState = st; render(st); }
  };
  cancel.onclick = () => { editing = null; if (lastState) render(lastState); else poll(); };
  actions.appendChild(save); actions.appendChild(cancel);
  ed.appendChild(actions);
  card.appendChild(ed);
}

function transitionRow(days, d, i, z, card) {
  const t = days[d][i];
  const row = document.createElement("div");
  row.className = "row";
  const time = $('<input type="time" value="' + esc(t.time) + '">');
  const temp = $('<input type="number" min="5" max="35" step="0.5" value="' + esc(t.temp) + '">');
  time.onchange = () => { t.time = time.value; };
  temp.onchange = () => { t.temp = Number(temp.value); };
  const rm = $('<button class="rm">✕</button>');
  rm.onclick = () => { days[d].splice(i, 1); renderEditor(z, days, card); };
  row.appendChild(time); row.appendChild(temp); row.appendChild(rm);
  return row;
}

// Tick the live boost countdowns once a second between polls.
setInterval(() => {
  document.querySelectorAll(".cd[data-ends]").forEach(el => {
    el.textContent = fmtCountdown((Number(el.dataset.ends) - Date.now()) / 1000);
  });
}, 1000);

poll();
setInterval(poll, 5000);
</script>
</body>
</html>
]==]

ha.serve("GET", "/", function()
  return 200, PAGE, { ["Content-Type"] = "text/html; charset=utf-8" }
end)

-- Publish each zone's desired once at load time so the window script has a
-- value to restore before the first tick fires.
do
  local now, dow, minute = now_parts()
  for z in pairs(Z) do
    local d = desired(z, now, dow, minute)
    if d ~= nil then global.set(zones.desired_key(z), d) end
  end
end

ha.on_exception(ha.exceptions.log_file("/addon_config/thermostat-errors.log"))
