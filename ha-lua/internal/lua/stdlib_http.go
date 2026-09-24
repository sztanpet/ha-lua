package lua

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	lua "github.com/yuin/gopher-lua"
)

// httpTimeout bounds a single http.get/http.post round-trip. The request also
// carries L.Context(), but that is the script's *lifetime* context — without
// this cap a wedged remote pins the script goroutine until the script is
// stopped, stalling every event behind it.
const httpTimeout = 30 * time.Second

// maxResponseBytes caps a response body, like fs.read caps a file: the body
// becomes one Lua string in a VM shared with every other handler in the script,
// and a remote we do not control decides its size.
const maxResponseBytes = 8 << 20 // 8 MiB

// httpClient is shared by all scripts: it only carries the timeout and the
// default transport's connection pool, both of which are goroutine-safe.
var httpClient = &http.Client{Timeout: httpTimeout}

func registerHTTP(L *lua.LState) {
	L.RegisterModule("http", httpFuncs)
}

var httpFuncs = map[string]lua.LGFunction{
	"get":  luaHTTPGet,
	"post": luaHTTPPost,
}

// httpErr pushes the (nil, message) failure pair the module answers with.
func httpErr(L *lua.LState, err error) int {
	L.Push(lua.LNil)
	L.Push(lua.LString(err.Error()))
	return 2
}

func luaHTTPGet(L *lua.LState) int {
	url := L.CheckString(1)
	headers := L.OptTable(2, nil)

	req, err := http.NewRequestWithContext(L.Context(), http.MethodGet, url, nil)
	if err != nil {
		return httpErr(L, err)
	}
	setHeaders(req, headers)
	return doRequest(L, req)
}

func luaHTTPPost(L *lua.LState) int {
	url := L.CheckString(1)
	body := L.CheckString(2)
	contentType := L.CheckString(3)
	headers := L.OptTable(4, nil)

	req, err := http.NewRequestWithContext(L.Context(), http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		return httpErr(L, err)
	}
	req.Header.Set("Content-Type", contentType)
	setHeaders(req, headers)
	return doRequest(L, req)
}

func setHeaders(req *http.Request, headers *lua.LTable) {
	if headers == nil {
		return
	}
	headers.ForEach(func(k, v lua.LValue) {
		req.Header.Set(k.String(), v.String())
	})
}

func doRequest(L *lua.LState, req *http.Request) int {
	res, err := httpClient.Do(req)
	if err != nil {
		return httpErr(L, err)
	}
	defer res.Body.Close()

	// One byte past the cap, so an oversized body is an error rather than a
	// silent truncation the script would parse as the real answer.
	body, err := io.ReadAll(io.LimitReader(res.Body, maxResponseBytes+1))
	if err != nil {
		return httpErr(L, err)
	}
	if len(body) > maxResponseBytes {
		return httpErr(L, fmt.Errorf("response body larger than %d bytes", maxResponseBytes))
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
