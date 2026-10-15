package lua

import (
	"net"
	"time"

	lua "github.com/yuin/gopher-lua"
)

const (
	probeDefaultTimeout = time.Second
	// A dial blocks the script goroutine, and every event queued behind it.
	probeMaxTimeout = 5 * time.Second
)

func registerNet(L *lua.LState) {
	L.RegisterModule("net", map[string]lua.LGFunction{
		"reachable": luaNetReachable,
	})
}

// luaNetReachable opens a TCP connection and closes it at once: a
// reachability probe that needs no raw socket, unlike ICMP.
func luaNetReachable(L *lua.LState) int {
	address := L.CheckString(1)
	timeout := probeDefaultTimeout
	if spec := L.OptString(2, ""); spec != "" {
		d, err := time.ParseDuration(spec)
		if err != nil || d <= 0 || d > probeMaxTimeout {
			L.ArgError(2, "want a duration in (0, "+probeMaxTimeout.String()+"]")
		}
		timeout = d
	}

	dialer := net.Dialer{Timeout: timeout}
	conn, err := dialer.DialContext(L.Context(), "tcp", address)
	if err != nil {
		L.Push(lua.LFalse)
		L.Push(lua.LString(err.Error()))
		return 2
	}
	conn.Close()
	L.Push(lua.LTrue)
	return 1
}
