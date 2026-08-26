package lua

import (
	"encoding/json/v2"
	"log/slog"

	lua "github.com/yuin/gopher-lua"

	"github.com/sztanpet/ha-lua/internal/mqtt"
)

// mqttHandler is one script's subscription to a topic filter.
type mqttHandler struct {
	filter string
	fn     *lua.LFunction
}

// registerMQTTAPI installs the `mqtt` module on L.
//
// The module is installed even with no broker configured, and every call then
// fails loudly: a script written against MQTT that silently does nothing is
// the exact failure this whole subsystem exists to end.
func (r *Runner) registerMQTTAPI(L *lua.LState, api *haAPI) {
	mod := L.NewTable()

	L.SetField(mod, "subscribe", L.NewFunction(func(L *lua.LState) int {
		filter := L.CheckString(1)
		fn := L.CheckFunction(2)
		if err := mqtt.ValidateFilter(filter); err != nil {
			L.RaiseError("mqtt.subscribe: %v", err)
			return 0
		}
		if r.mqttSubscribe == nil {
			L.RaiseError("mqtt.subscribe(%q): no broker configured", filter)
			return 0
		}
		if err := r.mqttSubscribe(filter); err != nil {
			L.RaiseError("mqtt.subscribe(%q): %v", filter, err)
			return 0
		}
		api.mqttHandlers = append(api.mqttHandlers, mqttHandler{filter: filter, fn: fn})
		return 0
	}))

	L.SetField(mod, "publish", L.NewFunction(func(L *lua.LState) int {
		topic := L.CheckString(1)
		payload, err := mqttPayload(L, L.CheckAny(2))
		if err != nil {
			L.RaiseError("mqtt.publish(%q): %v", topic, err)
			return 0
		}
		qos, retain := byte(0), false
		if opts := L.OptTable(3, nil); opts != nil {
			if v, ok := opts.RawGetString("qos").(lua.LNumber); ok {
				if v < 0 || v > 2 {
					L.RaiseError("mqtt.publish(%q): qos must be 0, 1 or 2", topic)
					return 0
				}
				qos = byte(v)
			}
			retain = lua.LVAsBool(opts.RawGetString("retain"))
		}
		if r.mqttPublish == nil {
			L.RaiseError("mqtt.publish(%q): no broker configured", topic)
			return 0
		}
		if err := r.mqttPublish(topic, payload, qos, retain); err != nil {
			L.RaiseError("mqtt.publish(%q): %v", topic, err)
			return 0
		}
		return 0
	}))

	L.SetGlobal("mqtt", mod)
}

// mqttPayload encodes what a script passed to mqtt.publish. Strings go out
// verbatim — Zigbee2MQTT's own action topics carry bare strings, and quoting
// them into JSON would break every such device — while tables, numbers and
// booleans are JSON, which is what a /set topic expects.
func mqttPayload(L *lua.LState, v lua.LValue) ([]byte, error) {
	if s, ok := v.(lua.LString); ok {
		return []byte(s), nil
	}
	val, err := luaToAny(L, v)
	if err != nil {
		return nil, err
	}
	return json.Marshal(val)
}

// handleMQTTMessage delivers one message to every handler of this script whose
// filter matches.
func (r *Runner) handleMQTTMessage(L *lua.LState, api *haAPI, msg mqtt.Message) {
	for _, h := range api.mqttHandlers {
		if !mqtt.Match(h.filter, msg.Topic) {
			continue
		}
		payload := string(msg.Payload)
		args := []lua.LValue{lua.LString(msg.Topic), lua.LString(payload)}
		// A JSON payload is handed over decoded as a third argument.
		// Zigbee2MQTT publishes both shapes — a bare action string on
		// <name>/action and a JSON object on <name> — so a script must not
		// have to guess which it got.
		if decoded, ok := decodeMQTTPayload(L, msg.Payload); ok {
			args = append(args, decoded)
		} else {
			args = append(args, lua.LNil)
		}
		callProtected(L, api, "mqtt_message", nil, h.fn, args...)
	}
}

// decodeMQTTPayload decodes a JSON object or array payload. Anything else
// (a bare word, a number, malformed JSON) is left to the raw string argument.
func decodeMQTTPayload(L *lua.LState, payload []byte) (lua.LValue, bool) {
	trimmed := trimSpace(payload)
	if len(trimmed) == 0 || (trimmed[0] != '{' && trimmed[0] != '[') {
		return lua.LNil, false
	}
	var v any
	if err := json.Unmarshal(trimmed, &v); err != nil {
		slog.Debug("mqtt: payload is not valid JSON", "err", err)
		return lua.LNil, false
	}
	return anyToLua(L, v), true
}

func trimSpace(b []byte) []byte {
	start := 0
	for start < len(b) && (b[start] == ' ' || b[start] == '\t' || b[start] == '\n' || b[start] == '\r') {
		start++
	}
	return b[start:]
}

// MQTTFilters returns the topic filters this script subscribed to. Only valid
// once LoadedCh is closed; used by the registry to fan a message out only to
// the scripts that asked for it.
func (r *Runner) MQTTFilters() []string {
	out := make([]string, 0, len(r.cachedMQTTHandlers))
	for _, h := range r.cachedMQTTHandlers {
		out = append(out, h.filter)
	}
	return out
}
