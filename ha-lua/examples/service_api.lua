-- service_api.lua
--
-- One generic HTTP endpoint that calls any Home Assistant service, for shell
-- scripts. No per-service Lua code, no HA long-lived access token, no
-- automation to edit every time you want to poke something new:
--
--   curl -H "X-Auth-Token: $TOKEN" \
--     "http://homeassistant.local:8100/s/service_api/call/light/turn_on?entity_id=light.kitchen&brightness=200"
--
--   curl -H "X-Auth-Token: $TOKEN" -d 'entity_id=switch.pump' \
--     http://homeassistant.local:8100/s/service_api/call/switch/turn_off
--
--   curl -H "X-Auth-Token: $TOKEN" \
--     -d '{"service":"notify.mobile_app_phone","message":"backup done","title":"nas"}' \
--     http://homeassistant.local:8100/s/service_api/call
--
-- The service comes from the path, a dotted `service` field, or separate
-- `domain` + `service` fields. Every other field is passed to Home Assistant
-- verbatim, so nothing here needs updating when HA grows a new service.
--
-- Fields arrive as query parameters, a form-encoded body, or a JSON object body.
-- Query and form values are text, so obvious types are reconstructed:
-- `true`/`false` become booleans, a number becomes a number when the text
-- round-trips exactly (`0123` stays a string — alarm codes have leading zeros),
-- and a `[` or `{` value is parsed as JSON. Use a JSON body for exact control; on
-- a collision the body wins over the query.
--
-- Replies are always JSON: 200 with `{"ok":true,...}`, or `{"ok":false,...}` with
-- 400 (malformed), 401 (bad token) or 502 (HA refused). The reply waits for HA's
-- verdict, so a 200 means the service ran; pass `wait=false` for
-- fire-and-forget.
--
-- It also serves a **Service API** tab: a form that assembles a call from your
-- real entity ids and hands back the finished URL and curl command, token filled
-- in. It builds commands; it never fires one.
--
-- SECURITY. The LAN port serves all of this with no Home Assistant login, so a
-- shared token guards it — without one, anyone on the network could unlock your
-- doors. The token is generated on first load, logged, and **written into the
-- page**, so anyone who can open the page on the LAN port has it: it stops a
-- stranger who guesses the URL, not one who loads the page. That is the deal for
-- a builder you never paste a token into. Plain HTTP on a LAN is fine for a
-- script on the same network and is never something to port-forward.

-- Your own token instead of the generated one: `openssl rand -hex 16`.
local TOKEN = ""

-- Fields that configure the request itself and are never sent to HA.
local RESERVED = {
  token = true,
  wait = true,
  domain = true,
  service = true,
}

local JSON_HEADERS = { ["Content-Type"] = "application/json" }

local function resolve_token()
  if TOKEN ~= "" then
    return TOKEN
  end
  local stored = store.get("token")
  if stored then
    ha.log("info", "service_api: ready, token starts with " .. string.sub(stored, 1, 6) ..
      " (it is on the Service API tab in full)")
    return stored
  end
  local generated = crypto.random_hex(16)
  store.set("token", generated)
  ha.log("warn", "service_api: generated API token " .. generated ..
    " -- also shown on the Service API tab")
  return generated
end

local token = resolve_token()

local function reply(status, payload)
  return status, json.encode(payload), JSON_HEADERS
end

local function fail(status, message)
  return reply(status, { ok = false, error = message })
end

local function authorized(req)
  local supplied = req.headers["X-Auth-Token"] or req.query.token
  if not supplied then
    local bearer = req.headers["Authorization"]
    if bearer then
      supplied = string.match(bearer, "^[Bb]earer%s+(.+)$")
    end
  end
  -- Constant time: a plain == leaks the token a byte at a time.
  return supplied ~= nil and crypto.equal(supplied, token)
end

local function coerce(text)
  if text == "true" then return true end
  if text == "false" then return false end
  local first = string.sub(text, 1, 1)
  if first == "[" or first == "{" then
    local ok, decoded = pcall(json.decode, text)
    if ok then return decoded end
    return text
  end
  local number = tonumber(text)
  -- Only when it round-trips: "0123" is a code, not the number 123.
  if number and tostring(number) == text then return number end
  return text
end

local function percent_decode(text)
  text = string.gsub(text, "+", " ")
  return (string.gsub(text, "%%(%x%x)", function(hex)
    return string.char(tonumber(hex, 16))
  end))
end

local function decode_form(body)
  local fields = {}
  for pair in string.gmatch(body, "[^&]+") do
    local key, value = string.match(pair, "^([^=]*)=?(.*)$")
    if key and key ~= "" then
      fields[percent_decode(key)] = coerce(percent_decode(value))
    end
  end
  return fields
end

local function parse_body(body)
  local trimmed = strings.trim_space(body or "")
  if trimmed == "" then
    return {}
  end
  local first = string.sub(trimmed, 1, 1)
  if first == "[" then
    return nil, "body must be a JSON object, not an array"
  end
  if first == "{" then
    local ok, decoded = pcall(json.decode, trimmed)
    if not ok then
      return nil, "body is not valid JSON"
    end
    if type(decoded) ~= "table" then
      return nil, "body must be a JSON object"
    end
    return decoded
  end
  return decode_form(trimmed)
end

local function resolve_service(path, fields)
  local from_path_domain, from_path_service = string.match(path, "^/call/([^/]+)/([^/]+)/?$")
  if from_path_domain then
    return from_path_domain, from_path_service
  end
  if path ~= "/call" and path ~= "/call/" then
    return nil, nil, "path must be /call or /call/<domain>/<service>"
  end
  if type(fields.domain) == "string" and type(fields.service) == "string" then
    return fields.domain, fields.service
  end
  if type(fields.service) == "string" then
    local dotted_domain, dotted_service = string.match(fields.service, "^([^.]+)%.([^.]+)$")
    if dotted_domain then
      return dotted_domain, dotted_service
    end
  end
  return nil, nil, 'name the service as /call/<domain>/<service>, "service": "light.turn_on", or "domain" plus "service"'
end

-- gopher-lua prefixes a raised error with the script position, which means
-- nothing to whoever reads the JSON.
local function clean_error(err)
  return (string.gsub(tostring(err), "^.-:%d+:%s*", ""))
end

local function handle(req)
  if not authorized(req) then
    return fail(401, "missing or wrong token")
  end

  local fields, err = parse_body(req.body)
  if not fields then
    return fail(400, err)
  end
  for key, value in pairs(req.query) do
    if fields[key] == nil then
      fields[key] = coerce(value)
    end
  end

  local domain, service, why = resolve_service(req.path, fields)
  if not domain then
    return fail(400, why)
  end

  local data = {}
  for key, value in pairs(fields) do
    if not RESERVED[key] then
      data[key] = value
    end
  end
  -- Only entity_id: a comma cannot occur in an id, but is ordinary text in a
  -- message.
  if type(data.entity_id) == "string" and strings.contains(data.entity_id, ",") then
    data.entity_id = strings.split(data.entity_id, ",")
  end

  local wait = fields.wait ~= false
  local ok, call_err = pcall(ha.call_service, domain, service, data, { wait = wait })
  if not ok then
    return fail(502, clean_error(call_err))
  end
  return reply(200, {
    ok = true,
    domain = domain,
    service = service,
    data = data,
    waited = wait,
  })
end

ha.serve("POST", "/call", handle)
-- GET calls a service, which is not REST. Deliberate: quoting a JSON body in a
-- shell script is the friction this endpoint exists to remove.
ha.serve("GET", "/call", handle)

-- A probe that checks the token without switching anything on.
ha.serve("GET", "/ping", function(req)
  if not authorized(req) then
    return fail(401, "missing or wrong token")
  end
  return reply(200, { ok = true })
end)

-- Guarded like the rest: knowing what exists is halfway to controlling it.
ha.serve("GET", "/entities", function(req)
  if not authorized(req) then
    return fail(401, "missing or wrong token")
  end
  return reply(200, { ok = true, entity_ids = ha.get_entity_ids("*") })
end)

-- Nothing can authenticate a page load on the LAN port, so serving the page is
-- serving the token — see SECURITY above.
local PAGE = assert(fs.read("service_api.html"),
  "service_api.html missing next to service_api.lua")

-- The slash matters as much as the quotes: without it a token containing
-- "</script>" ends the script block.
local function js_escape(text)
  return (string.gsub(text, "[\\\"/]", function(char) return "\\" .. char end))
end

-- A replacement function, so a "%" in a hand-picked token is not a capture
-- reference.
PAGE = string.gsub(PAGE, "__SERVICE_API_TOKEN__", function() return js_escape(token) end, 1)

ha.ui("Service API")
ha.serve("GET", "/", function()
  return 200, PAGE, { ["Content-Type"] = "text/html; charset=utf-8" }
end)
