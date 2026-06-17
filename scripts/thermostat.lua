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

local zone_defs = zones.zones

-- Per-zone store keys. Schedule/boost/override/comfort all live in this
-- script's KV store; the published desired lives in `global` (shared).
local function sched_key(zone) return "schedule:" .. zone end
local function boost_key(zone) return "boost:" .. zone end
local function override_key(zone) return "override:" .. zone end
local function comfort_key(zone) return "comfort:" .. zone end

-- now_parts returns the current time userdata plus the schedule's weekday
-- (0=Mon..6=Sun, converted from Go's Sunday-first weekday) and minute-of-day.
local function now_parts()
  local now = time.now()
  local dow = (now:weekday() + 6) % 7
  return now, dow, now:hour() * 60 + now:minute()
end

local function parse_time(text)
  if type(text) ~= "string" then return nil end
  local parsed = time.parse(time.RFC3339, text)
  return parsed -- nil on parse failure
end

-- comfort returns the zone's UI-settable boost temperature, seeding the default
-- the first time before the user touches the stepper.
local function comfort(zone)
  local value = store.get(comfort_key(zone))
  if type(value) == "number" then return value end
  return zones.default_comfort
end

-- mode returns the climate entity's hvac mode ("heat"/"off"/...) or nil if the
-- entity is not yet seeded.
local function mode(zone)
  local state = ha.get_state(zone_defs[zone].climate)
  if state == nil then return nil end
  return state.state
end

local function current_target(zone)
  local state = ha.get_state(zone_defs[zone].climate)
  if state and state.attributes then return state.attributes.temperature end
  return nil
end

-- any_window_open reports whether any window in the zone is definitely open.
-- A not-yet-seeded sensor (nil) counts as closed for the write decision.
local function any_window_open(zone)
  for _, window in ipairs(zone_defs[zone].windows) do
    local state = ha.get_state(window)
    if state ~= nil and state.state == "on" then return true end
  end
  return false
end

-- any_window_unknown reports whether any window sensor has no state yet. Used
-- only to suppress manual-override detection until the sensors have seeded.
local function any_window_unknown(zone)
  for _, window in ipairs(zone_defs[zone].windows) do
    if ha.get_state(window) == nil then return true end
  end
  return false
end

local function load_schedule(zone)
  local stored = store.get(sched_key(zone))
  if type(stored) == "table" and type(stored.days) == "table" then return stored.days end
  return {}
end

-- active_boost returns the live boost table for the zone, or nil. An expired
-- boost is cleared as a side effect so the zone reverts to schedule.
local function active_boost(zone, now)
  local boost = store.get(boost_key(zone))
  if type(boost) ~= "table" or not boost.active or type(boost.ends_at) ~= "string" then
    return nil
  end
  local ends = parse_time(boost.ends_at)
  if ends == nil then
    store.delete(boost_key(zone))
    return nil
  end
  if now:before(ends) then return boost end
  store.delete(boost_key(zone))
  return nil
end

-- active_override returns the live manual-override table, or nil, clearing it
-- once its `expires` instant has passed. (We avoid the field name "until"
-- because it is a Lua keyword.)
local function active_override(zone, now)
  local override = store.get(override_key(zone))
  if type(override) ~= "table" or type(override.temp) ~= "number" or type(override.expires) ~= "string" then
    return nil
  end
  local exp = parse_time(override.expires)
  if exp == nil then
    store.delete(override_key(zone))
    return nil
  end
  if now:before(exp) then return override end
  store.delete(override_key(zone))
  return nil
end

-- desired implements §4.1: boost beats override beats schedule. Returns the
-- temperature and its source string ("boost"/"override"/"schedule"), or nil if
-- the zone has no schedule at all.
local function desired(zone, now, dow, minute)
  if active_boost(zone, now) then return comfort(zone), "boost" end
  local override = active_override(zone, now)
  if override then return override.temp, "override" end
  local temp = schedule.resolve(load_schedule(zone), dow, minute)
  if temp ~= nil then return temp, "schedule" end
  return nil, nil
end

local function set_temp(zone, temp)
  ha.call_service("climate", "set_temperature", {
    entity_id = zone_defs[zone].climate,
    temperature = temp,
  })
end

-- apply_zone publishes the zone's desired setpoint and writes it to the climate
-- entity when the mode is heat and no window is open. The write is skipped when
-- the value is unchanged so we don't spam set_temperature.
local function apply_zone(zone, now, dow, minute)
  local desired_temp = desired(zone, now, dow, minute)
  if desired_temp == nil then return end
  global.set(zones.desired_key(zone), desired_temp)
  if mode(zone) ~= "heat" then return end
  if any_window_open(zone) then return end -- window script's territory
  local current = current_target(zone)
  if current == nil or math.abs(current - desired_temp) > 0.05 then
    set_temp(zone, desired_temp)
  end
end

-- The single tick that drives everything (§8): recompute, publish and (maybe)
-- write every zone. Boost/override expiry is handled inside desired().
local function tick()
  local now, dow, minute = now_parts()
  for zone in pairs(zone_defs) do
    apply_zone(zone, now, dow, minute)
  end
end

ha.every("1m", tick)

-- Manual setpoint change detection (§9): the controller is the only thing that
-- writes `desired`, and it always writes exactly `desired`, so a climate target
-- that differs from the published desired is an external change by the user. It
-- becomes an ad-hoc override that holds until the next schedule transition.
for zone, conf in pairs(zone_defs) do
  ha.on_state_change(conf.climate, function(data)
    local new_state = data.new_state
    if new_state == nil or new_state.attributes == nil then return end
    if new_state.state ~= "heat" then return end
    local target = new_state.attributes.temperature
    if type(target) ~= "number" then return end

    local now, dow, minute = now_parts()
    if active_boost(zone, now) then return end -- boost wins; ignore dial nudges
    -- Window open or not-yet-seeded: that's the window script's 15°C territory.
    if any_window_open(zone) or any_window_unknown(zone) then return end

    local published = global.get(zones.desired_key(zone))
    -- Float tolerance: our own write (and the window restore) set target ==
    -- published exactly, but 21 vs 21.0 must not look like a manual change.
    if type(published) == "number" and math.abs(target - published) <= 0.1 then return end

    local _, _, mins_to_next = schedule.resolve(load_schedule(zone), dow, minute)
    local hold = mins_to_next ~= nil and mins_to_next * 60 or 24 * 3600
    store.set(override_key(zone), {
      temp = target,
      expires = now:add(hold):format(time.RFC3339),
    })
    apply_zone(zone, now, dow, minute) -- republish the new desired immediately
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
local function zone_state(zone, now, dow, minute)
  local state = ha.get_state(zone_defs[zone].climate)
  local hvac_mode = state and state.state or "unknown"
  local current, target
  if state and state.attributes then
    current = state.attributes.current_temperature
    target = state.attributes.temperature
  end
  local days = load_schedule(zone)
  local sched_temp, now_index = schedule.resolve(days, dow, minute)

  local boost_tbl
  local boost = active_boost(zone, now)
  if boost then
    local remaining = 0
    local ends = parse_time(boost.ends_at)
    if ends ~= nil then
      local secs = ends:sub(now)
      if secs > 0 then remaining = math.floor(secs) end
    end
    boost_tbl = { active = true, ends_at = boost.ends_at, remaining_s = remaining }
  end

  return {
    mode = hvac_mode,
    current_temp = current,
    target = target,
    comfort_temp = comfort(zone),
    window_open = any_window_open(zone),
    scheduled_temp = sched_temp,
    today = schedule.day_list(days, dow),
    now_index = now_index,
    boost = boost_tbl,
  }
end

local function full_state()
  local now, dow, minute = now_parts()
  local zone_states = {}
  for zone in pairs(zone_defs) do
    zone_states[zone] = zone_state(zone, now, dow, minute)
  end
  return { zones = zone_states }
end

-- decode_body parses a JSON request body into a table, or returns nil.
local function decode_body(req)
  local ok, decoded = pcall(json.decode, req.body)
  if ok and type(decoded) == "table" then return decoded end
  return nil
end

ha.serve("GET", "/api/state", function()
  return json_ok(full_state())
end)

ha.serve("POST", "/api/boost", function(req)
  local body = decode_body(req)
  if body == nil then return bad("invalid JSON body") end
  local zone = body.zone
  if type(zone) ~= "string" or zone_defs[zone] == nil then return bad("unknown zone") end
  if type(body.minutes) ~= "number" or body.minutes <= 0 or body.minutes > 1440 then
    return bad("minutes must be 1..1440")
  end
  local now, dow, minute = now_parts()
  store.set(boost_key(zone), {
    active = true,
    ends_at = now:add(body.minutes * 60):format(time.RFC3339),
  })
  store.delete(override_key(zone)) -- a boost outranks and clears any override
  apply_zone(zone, now, dow, minute)
  return json_ok(full_state())
end)

-- Registered after /api/boost; the router's longest-prefix match sends
-- /api/boost/cancel here and bare /api/boost to the boost handler.
ha.serve("POST", "/api/boost/cancel", function(req)
  local body = decode_body(req)
  if body == nil then return bad("invalid JSON body") end
  local zone = body.zone
  if type(zone) ~= "string" or zone_defs[zone] == nil then return bad("unknown zone") end
  store.delete(boost_key(zone))
  local now, dow, minute = now_parts()
  apply_zone(zone, now, dow, minute)
  return json_ok(full_state())
end)

ha.serve("PUT", "/api/settings", function(req)
  local body = decode_body(req)
  if body == nil then return bad("invalid JSON body") end
  local zone = body.zone
  if type(zone) ~= "string" or zone_defs[zone] == nil then return bad("unknown zone") end
  if type(body.comfort_temp) ~= "number" or body.comfort_temp < 5 or body.comfort_temp > 35 then
    return bad("comfort_temp out of range (5..35)")
  end
  store.set(comfort_key(zone), body.comfort_temp)
  local now, dow, minute = now_parts()
  apply_zone(zone, now, dow, minute) -- if a boost is active, the new comfort applies now
  return json_ok(full_state())
end)

ha.serve("GET", "/api/schedule", function(req)
  local zone = req.query and req.query.zone
  if type(zone) == "string" and zone ~= "" then
    if zone_defs[zone] == nil then return bad("unknown zone") end
    return json_ok({ zone = zone, days = load_schedule(zone) })
  end
  local all = {}
  for other_zone in pairs(zone_defs) do
    all[other_zone] = load_schedule(other_zone)
  end
  return json_ok({ schedules = all })
end)

ha.serve("PUT", "/api/schedule", function(req)
  local body = decode_body(req)
  if body == nil then return bad("invalid JSON body") end
  local zone = body.zone
  if type(zone) ~= "string" or zone_defs[zone] == nil then return bad("unknown zone") end
  local valid, msg = schedule.validate(body.days)
  if not valid then return bad("invalid schedule: " .. msg) end
  store.set(sched_key(zone), { days = body.days })
  local now, dow, minute = now_parts()
  apply_zone(zone, now, dow, minute)
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

const $ = (html) => { const div = document.createElement("div"); div.innerHTML = html.trim(); return div.firstChild; };
const esc = (value) => String(value).replace(/[&<>"]/g, ch => ({"&":"&amp;","<":"&lt;",">":"&gt;",'"':"&quot;"}[ch]));
const title = (zone) => zone.charAt(0).toUpperCase() + zone.slice(1).replace(/_/g, " ");

function showErr(msg) {
  const errEl = document.getElementById("err");
  errEl.textContent = msg; errEl.style.display = "block";
  setTimeout(() => { errEl.style.display = "none"; }, 4000);
}

async function send(path, method, payload) {
  const resp = await fetch("./" + path, {
    method, headers: { "Content-Type": "application/json" },
    body: JSON.stringify(payload),
  });
  if (!resp.ok) { showErr("Error " + resp.status + ": " + (await resp.text())); return null; }
  return resp.json();
}

async function poll() {
  if (editing) return;                 // don't clobber an open editor
  try {
    const resp = await fetch("./api/state");
    if (resp.ok) { lastState = await resp.json(); render(lastState); }
  } catch (err) { /* transient; next poll retries */ }
}

function fmtCountdown(sec) {
  sec = Math.max(0, Math.floor(sec));
  const minutes = Math.floor(sec / 60), seconds = sec % 60;
  return minutes + ":" + String(seconds).padStart(2, "0");
}

function render(state) {
  if (editing) return;
  const app = document.getElementById("app");
  app.innerHTML = "";
  Object.keys(state.zones).sort().forEach(zone => app.appendChild(zoneCard(zone, state.zones[zone])));
}

function zoneCard(zone, status) {
  const card = document.createElement("div");
  card.className = "card";

  const current = (status.current_temp != null) ? status.current_temp.toFixed(1) + "°" : "";
  card.appendChild($(
    '<div class="head"><span class="zone">' + esc(title(zone)) + '</span>' +
    '<span class="status"><b>' + esc(status.mode) + '</b> ' + current + '</span></div>'
  ));

  const controllable = status.mode === "heat";
  if (!controllable) {
    card.appendChild($('<div class="muted">mode ' + esc(status.mode) + ' — not controlled</div>'));
  } else if (status.boost && status.boost.active) {
    const ends = Date.now() + (status.boost.remaining_s || 0) * 1000;
    const row = $('<div class="boosting"><span class="cd" data-ends="' + ends + '">' +
      fmtCountdown(status.boost.remaining_s || 0) + '</span><span>boosting to ' +
      esc(status.comfort_temp) + '°</span><button>cancel</button></div>');
    row.querySelector("button").onclick = () =>
      send("api/boost/cancel", "POST", { zone: zone }).then(newState => newState && (lastState = newState, render(newState)));
    card.appendChild(row);
  } else {
    const row = document.createElement("div");
    row.className = "boost-row";
    PRESETS.forEach(min => {
      const btn = $('<button>' + min + 'm</button>');
      btn.onclick = () => send("api/boost", "POST", { zone: zone, minutes: min })
        .then(newState => newState && (lastState = newState, render(newState)));
      row.appendChild(btn);
    });
    const custom = $('<button class="alt" title="custom minutes">⌨</button>');
    custom.onclick = () => {
      const minutesInput = prompt("Boost for how many minutes?", "45");
      const mins = parseInt(minutesInput, 10);
      if (mins > 0) send("api/boost", "POST", { zone: zone, minutes: mins })
        .then(newState => newState && (lastState = newState, render(newState)));
    };
    row.appendChild(custom);
    card.appendChild(row);
  }

  if (controllable) {
    const step = document.createElement("div");
    step.className = "stepper";
    step.appendChild($('<span class="lbl">boost to</span>'));
    const minus = $('<button>−</button>'), plus = $('<button>+</button>');
    const val = $('<span class="val">' + esc(status.comfort_temp) + '°</span>');
    const setComfort = (delta) => {
      const newVal = Math.round((Number(status.comfort_temp) + delta) * 2) / 2;
      send("api/settings", "PUT", { zone: zone, comfort_temp: newVal })
        .then(newState => newState && (lastState = newState, render(newState)));
    };
    minus.onclick = () => setComfort(-0.5);
    plus.onclick = () => setComfort(0.5);
    step.appendChild(minus); step.appendChild(val); step.appendChild(plus);
    card.appendChild(step);
  }

  const today = Array.isArray(status.today) ? status.today : [];
  if (today.length) {
    const strip = today.map((step, i) =>
      '<span class="step' + (i === status.now_index ? " now" : "") + '">' +
      (i === status.now_index ? "▶ " : "") + esc(step.time) + " " + esc(step.temp) + "°</span>"
    ).join("");
    card.appendChild($('<div class="today">' + strip + '</div>'));
  } else {
    card.appendChild($('<div class="today">no schedule set</div>'));
  }

  const edit = $('<span class="editlink">✎ schedule</span>');
  edit.onclick = () => openEditor(zone, card);
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

async function openEditor(zone, card) {
  const resp = await fetch("./api/schedule?zone=" + encodeURIComponent(zone));
  if (!resp.ok) { showErr("Could not load schedule"); return; }
  const data = await resp.json();
  editing = zone;
  renderEditor(zone, normalizeDays(data.days), card);
}

function renderEditor(zone, days, card) {
  let ed = card.querySelector(".editor");
  if (ed) ed.remove();
  ed = document.createElement("div");
  ed.className = "editor";

  for (let d = 0; d < 7; d++) {
    const day = document.createElement("div");
    day.className = "day";
    day.appendChild($('<div class="dname">' + DAYS[d] + '</div>'));
    days[d].forEach((transition, i) => day.appendChild(transitionRow(days, d, i, zone, card)));
    const add = $('<button class="add">+ add</button>');
    add.onclick = () => { days[d].push({ time: "07:00", temp: 21 }); renderEditor(zone, days, card); };
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
    const newState = await send("api/schedule", "PUT", { zone: zone, days: payload });
    if (newState) { editing = null; lastState = newState; render(newState); }
  };
  cancel.onclick = () => { editing = null; if (lastState) render(lastState); else poll(); };
  actions.appendChild(save); actions.appendChild(cancel);
  ed.appendChild(actions);
  card.appendChild(ed);
}

function transitionRow(days, d, i, zone, card) {
  const transition = days[d][i];
  const row = document.createElement("div");
  row.className = "row";
  const time = $('<input type="time" value="' + esc(transition.time) + '">');
  const temp = $('<input type="number" min="5" max="35" step="0.5" value="' + esc(transition.temp) + '">');
  time.onchange = () => { transition.time = time.value; };
  temp.onchange = () => { transition.temp = Number(temp.value); };
  const rm = $('<button class="rm">✕</button>');
  rm.onclick = () => { days[d].splice(i, 1); renderEditor(zone, days, card); };
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
  for zone in pairs(zone_defs) do
    local desired_temp = desired(zone, now, dow, minute)
    if desired_temp ~= nil then global.set(zones.desired_key(zone), desired_temp) end
  end
end

ha.on_exception(ha.exceptions.log_file("/addon_config/thermostat-errors.log"))
