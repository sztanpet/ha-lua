package lua

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/go-json-experiment/json/jsontext"

	"github.com/sztanpet/ha-lua/internal/ha"
	"github.com/sztanpet/ha-lua/internal/scheduler"
	"github.com/sztanpet/ha-lua/internal/state"
	"github.com/sztanpet/ha-lua/internal/store"
	"github.com/sztanpet/ha-lua/internal/testutil"
)

// errRejected stands in for Home Assistant refusing a call outright.
var errRejected = errors.New("value 99.0 for climate.set_temperature is above max_temp 30.0")

// serviceCall is one call the example asked Home Assistant to perform.
type serviceCall struct {
	domain  string
	service string
	data    map[string]any
	waited  bool
}

// serveServiceAPI boots the real examples/service_api.lua and returns its
// router, the token it generated on first load, and the recorded calls.
// callErr, when non-nil, is what the fake HA returns for every call.
func serveServiceAPI(t *testing.T, callErr error) (*Router, string, *[]serviceCall) {
	t.Helper()
	dir := t.TempDir()
	copyRepoFile(t, filepath.Join(repoScriptsDir, "service_api.lua"), filepath.Join(dir, "service_api.lua"))
	copyRepoFile(t, filepath.Join(repoScriptsDir, "service_api.html"), filepath.Join(dir, "service_api.html"))

	writeDB, readDB := testutil.NewTestDB(t, nil)
	if err := state.Migrate(writeDB); err != nil {
		t.Fatal(err)
	}
	tracker := state.New(writeDB, readDB)
	kv := store.New(writeDB, readDB, "service_api")
	global := store.NewGlobal(writeDB, readDB)
	reg := NewRegistry()
	router := NewRouter(reg)
	sched := scheduler.New(writeDB, time.UTC, reg.DispatchToTimer)

	// The builder page picks its entity ids out of the state mirror.
	if err := tracker.Seed(context.Background(), []ha.StateData{
		{EntityID: "light.kitchen", State: "off"},
		{EntityID: "switch.pump", State: "on"},
	}); err != nil {
		t.Fatal(err)
	}

	calls := &[]serviceCall{}
	record := func(domain, service string, data jsontext.Value, waited bool) {
		fields := map[string]any{}
		if err := json.Unmarshal([]byte(data), &fields); err != nil {
			t.Errorf("service data %q: %v", data, err)
		}
		*calls = append(*calls, serviceCall{domain: domain, service: service, data: fields, waited: waited})
	}
	r := NewRunner("service_api", dir, openTestRoot(t, dir), openTestRoot(t, t.TempDir()), tracker, sched, kv, global)
	r.SetCallService(func(_ context.Context, domain, service string, data jsontext.Value) error {
		record(domain, service, data, true)
		return callErr
	})
	r.SetCallServiceAsync(func(_ context.Context, domain, service string, data jsontext.Value) (<-chan error, error) {
		record(domain, service, data, false)
		verdict := make(chan error, 1)
		verdict <- callErr
		return verdict, nil
	})
	reg.Add(r)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); r.Start(ctx, filepath.Join(dir, "service_api.lua")) }()
	t.Cleanup(func() { cancel(); <-done })

	select {
	case <-r.LoadedCh:
	case <-time.After(3 * time.Second):
		t.Fatal("service_api.lua did not finish loading")
	}
	router.Register("service_api", r.Routes())

	stored, err := kv.Get(context.Background(), "token")
	if err != nil {
		t.Fatal(err)
	}
	token, ok := stored.(string)
	if !ok || token == "" {
		t.Fatalf("script stored no API token, got %#v", stored)
	}
	return router, token, calls
}

type apiReply struct {
	OK      bool           `json:"ok"`
	Error   string         `json:"error"`
	Domain  string         `json:"domain"`
	Service string         `json:"service"`
	Data    map[string]any `json:"data"`
	Waited  bool           `json:"waited"`
	Entity  []string       `json:"entity_ids"`
}

// doAPI sends a request with the token in the X-Auth-Token header.
func doAPI(t *testing.T, router *Router, token, method, target, body string) (int, apiReply) {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), method, Mount+"service_api"+target, strings.NewReader(body))
	req.Header.Set("X-Auth-Token", token)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	var reply apiReply
	if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil {
		t.Fatalf("%s %s: body %q is not JSON: %v", method, target, rec.Body.String(), err)
	}
	return rec.Code, reply
}

// Every way of naming a service must reach the same call, and query values
// must arrive typed the way the header comment promises.
func TestServiceAPICallForms(t *testing.T) {
	tests := []struct {
		name   string
		method string
		target string
		body   string
		want   serviceCall
	}{
		{
			name:   "path and query",
			method: "GET",
			target: "/call/light/turn_on?entity_id=light.kitchen&brightness=200",
			want: serviceCall{domain: "light", service: "turn_on", data: map[string]any{
				"entity_id": "light.kitchen", "brightness": float64(200),
			}},
		},
		{
			name:   "form body",
			method: "POST",
			target: "/call/switch/turn_off",
			body:   "entity_id=switch.pump",
			want:   serviceCall{domain: "switch", service: "turn_off", data: map[string]any{"entity_id": "switch.pump"}},
		},
		{
			name:   "dotted service in json body",
			method: "POST",
			target: "/call",
			body:   `{"service":"notify.phone","message":"backup done"}`,
			want:   serviceCall{domain: "notify", service: "phone", data: map[string]any{"message": "backup done"}},
		},
		{
			name:   "separate domain and service fields",
			method: "POST",
			target: "/call",
			body:   `{"domain":"lock","service":"unlock","entity_id":"lock.front"}`,
			want:   serviceCall{domain: "lock", service: "unlock", data: map[string]any{"entity_id": "lock.front"}},
		},
		{
			name:   "json value in a query parameter",
			method: "GET",
			target: "/call/light/turn_on?entity_id=light.strip&rgb_color=%5B255,0,0%5D",
			want: serviceCall{domain: "light", service: "turn_on", data: map[string]any{
				"entity_id": "light.strip", "rgb_color": []any{float64(255), float64(0), float64(0)},
			}},
		},
		{
			name:   "booleans and percent-encoded text",
			method: "POST",
			target: "/call/notify/phone",
			body:   "message=hello%2C+world&data=%7B%22ttl%22%3A0%7D&important=true",
			want: serviceCall{domain: "notify", service: "phone", data: map[string]any{
				"message": "hello, world", "data": map[string]any{"ttl": float64(0)}, "important": true,
			}},
		},
		{
			// Leading zeros mean it is a code, not a number.
			name:   "numeric-looking code stays a string",
			method: "GET",
			target: "/call/alarm_control_panel/alarm_disarm?entity_id=alarm_control_panel.home&code=0123",
			want: serviceCall{domain: "alarm_control_panel", service: "alarm_disarm", data: map[string]any{
				"entity_id": "alarm_control_panel.home", "code": "0123",
			}},
		},
		{
			name:   "comma-separated entity_id becomes a list",
			method: "GET",
			target: "/call/light/turn_off?entity_id=light.a,light.b",
			want: serviceCall{domain: "light", service: "turn_off", data: map[string]any{
				"entity_id": []any{"light.a", "light.b"},
			}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			router, token, calls := serveServiceAPI(t, nil)
			code, reply := doAPI(t, router, token, tc.method, tc.target, tc.body)
			if code != 200 || !reply.OK {
				t.Fatalf("status %d reply %+v", code, reply)
			}
			if len(*calls) != 1 {
				t.Fatalf("want 1 service call, got %d: %+v", len(*calls), *calls)
			}
			got := (*calls)[0]
			if got.domain != tc.want.domain || got.service != tc.want.service {
				t.Errorf("called %s.%s, want %s.%s", got.domain, got.service, tc.want.domain, tc.want.service)
			}
			if !sameJSON(t, got.data, tc.want.data) {
				t.Errorf("service data = %#v, want %#v", got.data, tc.want.data)
			}
		})
	}
}

func sameJSON(t *testing.T, got, want any) bool {
	t.Helper()
	gotBytes, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	wantBytes, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	return string(gotBytes) == string(wantBytes)
}

// An unauthenticated LAN port is the whole reason the token exists: no token,
// wrong token, and a token in the wrong place must never reach a service.
func TestServiceAPIRejectsBadToken(t *testing.T) {
	router, token, calls := serveServiceAPI(t, nil)

	for _, tc := range []struct {
		name  string
		token string
	}{
		{name: "no token"},
		{name: "wrong token", token: "deadbeef"},
		{name: "right token, one char short", token: token[:len(token)-1]},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequestWithContext(context.Background(), "GET",
				Mount+"service_api/call/light/turn_on?entity_id=light.kitchen", nil)
			if tc.token != "" {
				req.Header.Set("X-Auth-Token", tc.token)
			}
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != 401 {
				t.Fatalf("status %d, want 401 (body %q)", rec.Code, rec.Body.String())
			}
		})
	}
	if len(*calls) != 0 {
		t.Fatalf("unauthorized requests reached Home Assistant: %+v", *calls)
	}

	// The same token in the query or as a bearer works — shell scripts do not
	// all have a convenient way to set a header.
	code, reply := doAPI(t, router, token, "GET", "/ping", "")
	if code != 200 || !reply.OK {
		t.Fatalf("ping with header: status %d reply %+v", code, reply)
	}
	req := httptest.NewRequestWithContext(context.Background(), "GET",
		Mount+"service_api/ping?token="+token, nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("ping with query token: status %d body %q", rec.Code, rec.Body.String())
	}
	req = httptest.NewRequestWithContext(context.Background(), "GET", Mount+"service_api/ping", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("ping with bearer token: status %d body %q", rec.Code, rec.Body.String())
	}
}

// A shell script only ever sees the status code and the error string, so both
// have to say something true about what went wrong.
func TestServiceAPIErrors(t *testing.T) {
	router, token, calls := serveServiceAPI(t, nil)

	for _, tc := range []struct {
		name   string
		method string
		target string
		body   string
		want   int
	}{
		{name: "no service named", method: "POST", target: "/call", body: `{"entity_id":"light.a"}`, want: 400},
		{name: "service without a domain", method: "POST", target: "/call", body: `{"service":"turn_on"}`, want: 400},
		{name: "too many path segments", method: "GET", target: "/call/light/turn_on/extra", want: 400},
		{name: "broken json body", method: "POST", target: "/call/light/turn_on", body: `{"entity_id":`, want: 400},
		{name: "json array body", method: "POST", target: "/call/light/turn_on", body: `[{"entity_id":"light.a"}]`, want: 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, reply := doAPI(t, router, token, tc.method, tc.target, tc.body)
			if code != tc.want {
				t.Fatalf("status %d, want %d (reply %+v)", code, tc.want, reply)
			}
			if reply.OK || reply.Error == "" {
				t.Errorf("reply carries no error: %+v", reply)
			}
		})
	}
	if len(*calls) != 0 {
		t.Fatalf("malformed requests reached Home Assistant: %+v", *calls)
	}
}

// A service Home Assistant refuses must not be reported as success, and its
// message must survive to the client without gopher-lua's script position.
func TestServiceAPIReportsHARejection(t *testing.T) {
	router, token, _ := serveServiceAPI(t, errRejected)
	code, reply := doAPI(t, router, token, "GET", "/call/climate/set_temperature?entity_id=climate.x&temperature=99", "")
	if code != 502 {
		t.Fatalf("status %d, want 502 (reply %+v)", code, reply)
	}
	if !strings.Contains(reply.Error, "max_temp") {
		t.Errorf("error %q does not carry HA's message", reply.Error)
	}
	if strings.Contains(reply.Error, ".lua:") {
		t.Errorf("error %q leaks the script position", reply.Error)
	}
}

// wait=false must reach ha.call_service as such: a script firing a dozen
// notifications should not pay a device round trip for each one.
func TestServiceAPIWaitFalse(t *testing.T) {
	// Rejected by HA, yet the client still gets a 200: with wait=false nobody
	// is listening for the verdict any more. That is the trade being made.
	router, token, calls := serveServiceAPI(t, errRejected)
	code, reply := doAPI(t, router, token, "GET", "/call/switch/turn_on?entity_id=switch.a&wait=false", "")
	if code != 200 || !reply.OK {
		t.Fatalf("status %d reply %+v", code, reply)
	}
	if reply.Waited {
		t.Errorf("reply says it waited, want waited=false")
	}
	if _, ok := reply.Data["wait"]; ok {
		t.Errorf("wait leaked into the service data: %#v", reply.Data)
	}
	if len(*calls) != 1 || (*calls)[0].waited {
		t.Fatalf("want one call on the async path, got %+v", *calls)
	}
}

// The builder page is served to anyone who asks, with the token baked in --
// that is the deliberate trade for a builder nobody has to paste a token into.
// The routes it drives still check that token.
func TestServiceAPIPageAndEntities(t *testing.T) {
	router, token, _ := serveServiceAPI(t, nil)

	rec := doReqID(router, "service_api", "GET", "/", "")
	if rec.Code != 200 {
		t.Fatalf("GET / status %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "<title>Service API</title>") {
		t.Errorf("GET / did not serve the builder page: %.120q", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), token) {
		t.Error("the page was served without the token baked in")
	}

	if rec := doReqID(router, "service_api", "GET", "/entities", ""); rec.Code != 401 {
		t.Fatalf("GET /entities without a token: status %d, want 401", rec.Code)
	}

	_, reply := doAPI(t, router, token, "GET", "/entities", "")
	if len(reply.Entity) != 2 || reply.Entity[0] != "light.kitchen" {
		t.Errorf("entity_ids = %v, want the two seeded ids sorted", reply.Entity)
	}
}

// TestServiceAPIBuilderPage drives the builder in a browser: the token has to
// verify against the live endpoint, the entity pickers have to fill from the
// state mirror, and both outputs have to match what the endpoint parses --
// this page is worthless if it hands out a command that does not work.
func TestServiceAPIBuilderPage(t *testing.T) {
	ctx := newBrowserCtx(t)
	router, token, calls := serveServiceAPI(t, nil)
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)

	var url, curl, badge string
	var entityOptions int
	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/s/service_api/"),
		chromedp.WaitVisible("#url", chromedp.ByQuery),
		// No typing: the page arrives with the token already in the field.
		chromedp.WaitVisible("#token-status.ok", chromedp.ByQuery),
		chromedp.SendKeys("#domain", "light", chromedp.ByQuery),
		chromedp.SendKeys("#service", "turn_on", chromedp.ByQuery),
		chromedp.SendKeys(".row .value", "light.kitchen", chromedp.ByQuery),
		chromedp.Evaluate(`document.querySelectorAll("#entities option").length`, &entityOptions),
		chromedp.Text("#url", &url, chromedp.ByQuery),
		chromedp.Text("#curl", &curl, chromedp.ByQuery),
	); err != nil {
		t.Fatal(err)
	}

	// The light.* filter is what keeps the picker usable in a real house.
	if entityOptions != 1 {
		t.Errorf("entity picker offered %d options, want only the seeded light", entityOptions)
	}
	wantURL := srv.URL + "/s/service_api/call/light/turn_on?token=" + token + "&entity_id=light.kitchen"
	if url != wantURL {
		t.Errorf("URL = %q, want %q", url, wantURL)
	}
	if !strings.Contains(curl, "X-Auth-Token: "+token) {
		t.Errorf("curl carries no token header: %q", curl)
	}
	// The header is the point of the curl form; repeating it in the query
	// would put the token in every shell history and access log twice over.
	if strings.Contains(curl, "token="+token) {
		t.Errorf("curl repeats the token in the query: %q", curl)
	}

	// The generated GET must actually reach the service it claims to.
	generated, err := http.NewRequestWithContext(context.Background(), "GET", url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := srv.Client().Do(generated)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("generated URL returned %d", resp.StatusCode)
	}
	if len(*calls) != 1 || (*calls)[0].domain != "light" || (*calls)[0].service != "turn_on" {
		t.Fatalf("generated URL did not call light.turn_on: %+v", *calls)
	}

	// Switching to POST · JSON moves the data out of the query and into a
	// typed body -- and the type badge must warn that 0123 is not a number.
	if err := chromedp.Run(ctx,
		chromedp.Click(`#style button[data-style="json"]`, chromedp.ByQuery),
		chromedp.Click("#add", chromedp.ByQuery),
		chromedp.SendKeys(".row:last-child .key", "code", chromedp.ByQuery),
		chromedp.SendKeys(".row:last-child .value", "0123", chromedp.ByQuery),
		chromedp.Text(".row:last-child .type", &badge, chromedp.ByQuery),
		chromedp.Text("#url", &url, chromedp.ByQuery),
		chromedp.Text("#curl", &curl, chromedp.ByQuery),
	); err != nil {
		t.Fatal(err)
	}
	if badge != "text" {
		t.Errorf("0123 shown as %q, want text", badge)
	}
	if strings.Contains(url, "entity_id") {
		t.Errorf("POST URL still carries the data: %q", url)
	}
	if !strings.Contains(curl, `{"entity_id":"light.kitchen","code":"0123"}`) {
		t.Errorf("curl body = %q, want a JSON body keeping 0123 a string", curl)
	}
}
