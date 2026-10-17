package lua

import (
	"errors"

	lua "github.com/yuin/gopher-lua"

	"github.com/sztanpet/ha-lua/internal/syslog"
)

var errSyslogDisabled = errors.New("syslog input disabled (set the syslog_port option)")

// registerSyslogAPI installs the `syslog` module on L. Like mqtt, it is
// installed when disabled and raises then: a script waiting on a port
// nothing listens on would otherwise wait forever, silently.
func (r *Runner) registerSyslogAPI(L *lua.LState, api *haAPI) {
	mod := L.NewTable()
	L.SetField(mod, "on", L.NewFunction(func(L *lua.LState) int {
		fn := L.CheckFunction(1)
		if !r.syslogEnabled {
			L.RaiseError("syslog.on: %v", errSyslogDisabled)
			return 0
		}
		api.syslogHandlers = append(api.syslogHandlers, fn)
		r.wantsSyslog.Store(true)
		return 0
	}))
	L.SetGlobal("syslog", mod)
}

func (r *Runner) handleSyslogMessage(L *lua.LState, api *haAPI, msg syslog.Message) {
	tbl := L.NewTable()
	tbl.RawSetString("from", lua.LString(msg.From))
	if msg.Host != "" {
		tbl.RawSetString("host", lua.LString(msg.Host))
	}
	if msg.Priority >= 0 {
		tbl.RawSetString("facility", lua.LNumber(msg.Facility()))
		tbl.RawSetString("severity", lua.LNumber(msg.Severity()))
	}
	tbl.RawSetString("message", lua.LString(msg.Text))
	tbl.RawSetString("raw", lua.LString(msg.Raw))
	for _, fn := range api.syslogHandlers {
		callProtected(L, api, "syslog_message", nil, fn, tbl)
	}
}
