package lua

import (
	"context"
	"io"
	"net/http"
	"runtime/pprof"
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

// Mount is the path every script's routes live under: script <id>'s routes are
// served at /s/<id>/... and the daemon owns everything else.
const Mount = "/s/"

// Router is the http.Handler for the script-driven UI, mounted at /s/. Each
// script gets its own path namespace, so two scripts may both serve "/" — before
// v4.0.0 they shared one flat table and load order silently decided the winner.
//
// The scriptID->method->prefix table is only a routing hint: the authoritative
// handler lookup happens in the script's run loop against its own routes, so a
// stale entry (e.g. mid-reload) self-heals to a 404 rather than serving a dead
// goroutine. The owning runner is resolved through the Registry at request
// time, so a stopped script yields an immediate 404 instead of a timeout.
type Router struct {
	reg     *Registry
	timeout time.Duration

	mu     sync.RWMutex
	routes map[string]map[string][]string // scriptID -> method -> prefixes, longest first
}

// NewRouter creates a Router that resolves scripts through reg.
func NewRouter(reg *Registry) *Router {
	return &Router{
		reg:     reg,
		timeout: defaultRequestTimeout,
		routes:  make(map[string]map[string][]string),
	}
}

// Register replaces a script's routes. Safe to call while serving.
func (rt *Router) Register(scriptID string, specs []RouteSpec) {
	byMethod := make(map[string][]string, len(specs))
	for _, sp := range specs {
		byMethod[sp.Method] = append(byMethod[sp.Method], sp.Prefix)
	}
	for _, prefixes := range byMethod {
		sort.SliceStable(prefixes, func(i, j int) bool { return len(prefixes[i]) > len(prefixes[j]) })
	}

	rt.mu.Lock()
	defer rt.mu.Unlock()
	if len(byMethod) == 0 {
		delete(rt.routes, scriptID)
		return
	}
	rt.routes[scriptID] = byMethod
}

// Unregister drops every route owned by scriptID.
func (rt *Router) Unregister(scriptID string) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	delete(rt.routes, scriptID)
}

// match reports whether scriptID registered a prefix of path under method.
func (rt *Router) match(scriptID, method, path string) bool {
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	for _, prefix := range rt.routes[scriptID][method] {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

// splitMount splits /s/<id>/<rest> into the script id and the path the script
// sees. hasSlash is false for a bare /s/<id>, which needs the 308.
func splitMount(path string) (scriptID, stripped string, hasSlash bool) {
	rest, ok := strings.CutPrefix(path, Mount)
	if !ok {
		return "", "", false
	}
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		return rest[:i], rest[i:], true
	}
	return rest, "/", false
}

func (rt *Router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	scriptID, path, hasSlash := splitMount(r.URL.Path)
	if scriptID == "" {
		http.NotFound(w, r)
		return
	}
	if !hasSlash {
		// Pages fetch with relative URLs ("./api/state"), which resolve one
		// segment too high without the trailing slash. The Location is
		// relative on purpose: under HA ingress an absolute path would escape
		// the /api/hassio_ingress/<token>/ prefix — which is also why this
		// sets the header itself instead of calling http.Redirect, which
		// resolves a relative target back to an absolute one.
		target := scriptID + "/"
		if r.URL.RawQuery != "" {
			target += "?" + r.URL.RawQuery
		}
		w.Header().Set("Location", target)
		w.WriteHeader(http.StatusPermanentRedirect)
		return
	}
	if !rt.match(scriptID, r.Method, path) {
		http.NotFound(w, r)
		return
	}
	runner := rt.reg.Get(scriptID)
	if runner == nil {
		// Script stopped since the route was registered; self-heal.
		http.NotFound(w, r)
		return
	}

	// A request spends nearly all its life blocked on the script goroutine.
	// Labelling it by owning script is what makes the block profile readable:
	// "requests queued behind thermostat" instead of an anonymous count of
	// goroutines parked in a channel send. Do restores the connection
	// goroutine's previous labels on return, so keep-alive reuse is clean.
	pprof.Do(r.Context(), pprof.Labels("goroutine", "web-request", "script", scriptID),
		func(context.Context) { rt.serve(w, r, runner, path) })
}

// serve forwards a matched request to its script goroutine and writes the
// reply, giving up after rt.timeout in either direction. path is the mount-
// stripped path: a script sees "/api/state", never "/s/<id>/api/state".
func (rt *Router) serve(w http.ResponseWriter, r *http.Request, runner *Runner, path string) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBody))
	if err != nil {
		http.Error(w, "request too large", http.StatusRequestEntityTooLarge)
		return
	}

	req := &request{
		method:  r.Method,
		path:    path,
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
