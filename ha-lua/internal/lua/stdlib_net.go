package lua

import (
	"net"
	"time"

	lua "github.com/yuin/gopher-lua"
)

const (
	dialDefaultTimeout = time.Second
	// A dial blocks the script goroutine, and every event queued behind it.
	dialMaxTimeout = 5 * time.Second
)

func registerNet(L *lua.LState) {
	L.RegisterModule("net", map[string]lua.LGFunction{
		"dial": luaNetDial,
	})
}

// luaNetDial opens a TCP connection and closes it at once: a reachability
// probe that needs no raw socket, unlike ICMP.
func luaNetDial(L *lua.LState) int {
	address := L.CheckString(1)
	timeout := dialDefaultTimeout
	if spec := L.OptString(2, ""); spec != "" {
		d, err := time.ParseDuration(spec)
		if err != nil || d <= 0 || d > dialMaxTimeout {
			L.ArgError(2, "want a duration in (0, "+dialMaxTimeout.String()+"]")
		}
		timeout = d
	}

	dialer := net.Dialer{Timeout: timeout}
	conn, err := dialer.DialContext(L.Context(), "tcp", address)
	if err != nil {
		L.Push(lua.LNil)
		L.Push(lua.LString(err.Error()))
		return 2
	}
	conn.Close()
	L.Push(lua.LTrue)
	return 1
}
