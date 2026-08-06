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
-- The service can come from the path (`/call/<domain>/<service>`), from a
-- dotted `service` field, or from separate `domain` + `service` fields. Every
-- other field is passed to Home Assistant as service data verbatim, so the
-- endpoint needs no knowledge of the service being called and never needs
-- updating when Home Assistant grows a new one.
--
-- Fields may arrive as URL query parameters, as a form-encoded body (what
-- `curl -d key=value` sends), or as a JSON object body. Query and form values
-- are text, so obvious types are reconstructed: `true`/`false` become booleans,
-- a number becomes a number when the text round-trips exactly (`0123` stays a
-- string — alarm codes have leading zeros), and a value starting with `[` or
-- `{` is parsed as JSON (`rgb_color=[255,0,0]`). Use a JSON body when you need
-- exact control; on a collision the body wins over the query.
--
-- Replies are always JSON: `{"ok":true,...}` with 200, or `{"ok":false,
-- "error":"..."}` with 400 (malformed request), 401 (bad token) or 502 (Home
-- Assistant refused the call). By default the reply waits for HA's verdict, so
-- a shell script that gets a 200 knows the service actually ran; pass
-- `wait=false` for fire-and-forget.
--
-- SECURITY. The LAN port serves this without any Home Assistant login, so the
-- endpoint is guarded by a shared token — without one, anyone on your network
-- could unlock your doors. The token is generated on first load and written to
-- the daemon log once; copy it from there. Lost it? Put your own in TOKEN
-- below. It is plain HTTP on your LAN: fine for a script on the same network,
-- never something to port-forward.

-- Your own token, if you would rather not use the generated one. Anything
-- unguessable will do: `openssl rand -hex 16`.
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
      " (set TOKEN in the script to use your own)")
    return stored
  end
  local generated = crypto.random_hex(16)
  store.set("token", generated)
  ha.log("warn", "service_api: generated API token " .. generated ..
    " -- copy it now, it will not be logged again")
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
  -- crypto.equal is constant time; a plain == leaks the token a byte at a time.
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
  -- Only when the text round-trips: "0123" is a code, not the number 123.
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

-- Returns the request's fields, or nil plus a message the caller can hand back
-- to the shell script that got it wrong.
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

-- gopher-lua prefixes raised errors with the script position, which means
-- nothing to whoever is reading the JSON on the other end.
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
  -- Only entity_id: a comma is a separator here and cannot occur in an id,
  -- while it is ordinary text in a notification message.
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
-- GET calls a service too, which no REST purist would allow. It is here
-- because `curl "…?entity_id=x"` from a shell needs no quoting gymnastics,
-- and nothing but a script with the token ever reaches this endpoint.
ha.serve("GET", "/call", handle)

-- Somewhere to check the token and the plumbing without switching anything on.
ha.serve("GET", "/ping", function(req)
  if not authorized(req) then
    return fail(401, "missing or wrong token")
  end
  return reply(200, { ok = true })
end)
