package lua

import (
	"bytes"
	"io"
	"net/http"
	"time"

	lua "github.com/yuin/gopher-lua"
)

// httpTimeout bounds a single http.get/http.post round-trip. The request also
// carries L.Context(), but that is the script's *lifetime* context — without
// this cap a wedged remote pins the script goroutine until the script is
// stopped, stalling every event behind it.
const httpTimeout = 30 * time.Second

// httpClient is shared by all scripts: it only carries the timeout and the
// default transport's connection pool, both of which are goroutine-safe.
var httpClient = &http.Client{Timeout: httpTimeout}

func registerHTTP(L *lua.LState) {
	mod := L.RegisterModule("http", httpFuncs)
	L.Push(mod)
}

var httpFuncs = map[string]lua.LGFunction{
	"get":  luaHTTPGet,
	"post": luaHTTPPost,
}

func luaHTTPGet(L *lua.LState) int {
	url := L.CheckString(1)
	headers := L.OptTable(2, nil)

	req, err := http.NewRequestWithContext(L.Context(), "GET", url, nil)
	if err != nil {
		L.Push(lua.LNil)
		L.Push(lua.LString(err.Error()))
		return 2
	}

	if headers != nil {
		headers.ForEach(func(k, v lua.LValue) {
			req.Header.Set(k.String(), v.String())
		})
	}

	return doRequest(L, req)
}

func luaHTTPPost(L *lua.LState) int {
	url := L.CheckString(1)
	body := L.CheckString(2)
	contentType := L.CheckString(3)
	headers := L.OptTable(4, nil)

	req, err := http.NewRequestWithContext(L.Context(), "POST", url, bytes.NewReader([]byte(body)))
	if err != nil {
		L.Push(lua.LNil)
		L.Push(lua.LString(err.Error()))
		return 2
	}

	req.Header.Set("Content-Type", contentType)
	if headers != nil {
		headers.ForEach(func(k, v lua.LValue) {
			req.Header.Set(k.String(), v.String())
		})
	}

	return doRequest(L, req)
}

func doRequest(L *lua.LState, req *http.Request) int {
	res, err := httpClient.Do(req)
	if err != nil {
		L.Push(lua.LNil)
		L.Push(lua.LString(err.Error()))
		return 2
	}
	defer res.Body.Close()

	body, err := io.ReadAll(res.Body)
	if err != nil {
		L.Push(lua.LNil)
		L.Push(lua.LString(err.Error()))
		return 2
	}

	tbl := L.NewTable()
	tbl.RawSetString("status", lua.LNumber(res.StatusCode))
	tbl.RawSetString("body", lua.LString(body))

	respHeaders := L.NewTable()
	for k, v := range res.Header {
		if len(v) > 0 {
			respHeaders.RawSetString(k, lua.LString(v[0]))
		}
	}
	tbl.RawSetString("headers", respHeaders)

	L.Push(tbl)
	return 1
}
