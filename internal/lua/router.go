package lua

import (
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// defaultRequestTimeout bounds how long a client waits for a script to
	// accept and answer a request. It does NOT abort the handler: a hung
	// handler still occupies its script goroutine, exactly like a hung event
	// handler. The timeout only stops the HTTP side from waiting forever.
	defaultRequestTimeout = 5 * time.Second
	maxRequestBody        = 1 << 20 // 1 MiB
)

// request is a UI HTTP request marshaled onto a script goroutine. The body is
// already read into a string on the HTTP goroutine; the *http.Request never
// crosses into the LState's goroutine.
type request struct {
	method  string
	path    string
	query   map[string]string
	headers map[string]string
	body    string
	// reply is buffered (cap 1) so the run loop's send never blocks, even if
	// the client already gave up after the timeout.
	reply chan response
}

type response struct {
	status  int
	body    string
	headers map[string]string
}

// RouteSpec is a (method, prefix) pair a script registered via ha.serve.
type RouteSpec struct {
	Method string
	Prefix string
}

type routeBinding struct {
	prefix   string
	scriptID string
}

// Router is the http.Handler for the script-driven UI. The
// (method,prefix)->scriptID table is only a routing hint: the authoritative
// handler lookup happens in the script's run loop against its own routes, so a
// stale entry (e.g. mid-reload) self-heals to a 404 rather than serving a dead
// goroutine. The owning runner is resolved through the Registry at request
// time, so a stopped script yields an immediate 404 instead of a timeout.
type Router struct {
	reg     *Registry
	timeout time.Duration

	mu     sync.RWMutex
	routes map[string][]routeBinding // method -> bindings, longest prefix first
}

// NewRouter creates a Router that resolves scripts through reg.
func NewRouter(reg *Registry) *Router {
	return &Router{
		reg:     reg,
		timeout: defaultRequestTimeout,
		routes:  make(map[string][]routeBinding),
	}
}

// Register adds a script's routes. Safe to call while serving.
func (rt *Router) Register(scriptID string, specs []RouteSpec) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	for _, sp := range specs {
		m := append(rt.routes[sp.Method], routeBinding{prefix: sp.Prefix, scriptID: scriptID})
		sort.SliceStable(m, func(i, j int) bool { return len(m[i].prefix) > len(m[j].prefix) })
		rt.routes[sp.Method] = m
	}
}

// Unregister drops every route owned by scriptID.
func (rt *Router) Unregister(scriptID string) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	for method, bindings := range rt.routes {
		kept := bindings[:0]
		for _, b := range bindings {
			if b.scriptID != scriptID {
				kept = append(kept, b)
			}
		}
		if len(kept) == 0 {
			delete(rt.routes, method)
		} else {
			rt.routes[method] = kept
		}
	}
}

// match returns the owning scriptID for the longest registered prefix of path
// under method.
func (rt *Router) match(method, path string) (string, bool) {
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	for _, b := range rt.routes[method] {
		if strings.HasPrefix(path, b.prefix) {
			return b.scriptID, true
		}
	}
	return "", false
}

func (rt *Router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	scriptID, ok := rt.match(r.Method, r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}
	runner := rt.reg.Get(scriptID)
	if runner == nil {
		// Script stopped since the route was registered; self-heal.
		http.NotFound(w, r)
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBody))
	if err != nil {
		http.Error(w, "request too large", http.StatusRequestEntityTooLarge)
		return
	}

	req := &request{
		method:  r.Method,
		path:    r.URL.Path,
		query:   flattenValues(r.URL.Query()),
		headers: flattenValues(http.Header(r.Header)),
		body:    string(body),
		reply:   make(chan response, 1),
	}

	deadline := time.NewTimer(rt.timeout)
	defer deadline.Stop()

	// reqCh is never closed, so this send can only block (bounded by the
	// deadline), never panic.
	select {
	case runner.reqCh <- req:
	case <-deadline.C:
		http.Error(w, "script busy", http.StatusServiceUnavailable)
		return
	case <-r.Context().Done():
		return
	}

	select {
	case resp := <-req.reply:
		writeResponse(w, resp)
	case <-deadline.C:
		http.Error(w, "handler timeout", http.StatusServiceUnavailable)
	case <-r.Context().Done():
	}
}

func writeResponse(w http.ResponseWriter, resp response) {
	for k, v := range resp.headers {
		w.Header().Set(k, v)
	}
	status := resp.status
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	_, _ = io.WriteString(w, resp.body)
}

// flattenValues keeps the first value per key (url.Values and http.Header share
// the map[string][]string shape).
func flattenValues(v map[string][]string) map[string]string {
	out := make(map[string]string, len(v))
	for k, vs := range v {
		if len(vs) > 0 {
			out[k] = vs[0]
		}
	}
	return out
}
