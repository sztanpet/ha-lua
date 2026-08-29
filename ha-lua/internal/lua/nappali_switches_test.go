package lua

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sztanpet/ha-lua/internal/ha"
	"github.com/sztanpet/ha-lua/internal/mqtt"
	"github.com/sztanpet/ha-lua/internal/scheduler"
	"github.com/sztanpet/ha-lua/internal/state"
	"github.com/sztanpet/ha-lua/internal/store"
	"github.com/sztanpet/ha-lua/internal/testutil"
)

// The topics and ids hardcoded in the shipped example. The topics carry the
// Zigbee2MQTT friendly name, not the underscored entity id, and getting that
// wrong means watching a topic that never publishes — so pin them here.
const (
	nappaliSwitch1 = "zigbee2mqtt/switch1/action"
	nappaliSwitch2 = "zigbee2mqtt/switch2/action"
	nappaliAblak   = "light.zbminir2_nappaliablak"
	nappaliCsillar = "light.zbminir2_nappalicsillar"
)

// nappaliHarness runs the real examples/nappali_switches.lua against a spy
// call service and the production tracker.
type nappaliHarness struct {
	t    *testing.T
	reg  *Registry
	cmds chan string // "toggle light.zbminir2_nappaliablak" / "turn_on light.a,light.b"
}

func newNappaliHarness(t *testing.T, ablak, csillar string) *nappaliHarness {
	dir := t.TempDir()
	copyRepoFile(t, filepath.Join(repoScriptsDir, "nappali_switches.lua"),
		filepath.Join(dir, "nappali_switches.lua"))

	writeDB, readDB := testutil.NewTestDB(t, nil)
	if err := state.Migrate(writeDB); err != nil {
		t.Fatal(err)
	}
	tracker := state.New(writeDB, readDB)
	tracker.Start(t.Context())
	global := store.NewGlobal(writeDB, readDB)
	reg := NewRegistry()
	sched := scheduler.New(writeDB, time.UTC, reg.DispatchToTimer)

	ctx, cancel := context.WithCancel(context.Background())
	h := &nappaliHarness{t: t, reg: reg, cmds: make(chan string, 32)}

	if err := tracker.Seed(ctx, []ha.StateData{
		seedEntity(nappaliAblak, ablak, `{}`),
		seedEntity(nappaliCsillar, csillar, `{}`),
	}); err != nil {
		t.Fatal(err)
	}
	if err := sched.Start(ctx); err != nil {
		t.Fatal(err)
	}

	r := NewRunner("nappali_switches", dir, openTestRoot(t, dir), nil,
		tracker, sched, store.New(writeDB, readDB, "nappali_switches"), global)
	r.SetCallServiceAsync(func(_ context.Context, _, service string, data jsontext.Value) (<-chan error, error) {
		var payload struct {
			EntityID any `json:"entity_id"`
		}
		if err := json.Unmarshal(data, &payload); err != nil {
			t.Errorf("bad service data %s: %v", data, err)
		}
		h.cmds <- service + " " + strings.Join(entityIDs(payload.EntityID), ",")
		verdict := make(chan error, 1)
		verdict <- nil
		return verdict, nil
	})
	r.SetMQTT((&fakeBroker{}).subscribe, (&fakeBroker{}).publish)
	reg.Add(r)

	done := make(chan struct{})
	go func() { defer close(done); r.Start(ctx, filepath.Join(dir, "nappali_switches.lua")) }()
	t.Cleanup(func() { cancel(); <-done })
	select {
	case <-r.LoadedCh:
	case <-time.After(3 * time.Second):
		t.Fatal("nappali_switches.lua did not finish loading")
	}
	return h
}

func entityIDs(raw any) []string {
	switch v := raw.(type) {
	case string:
		return []string{v}
	case []any:
		ids := make([]string, 0, len(v))
		for _, id := range v {
			ids = append(ids, id.(string))
		}
		return ids
	}
	return nil
}

// press feeds one gesture the way Zigbee2MQTT publishes it: a bare action
// word on the device's action topic.
func (h *nappaliHarness) press(topic, action string) {
	h.t.Helper()
	h.reg.DispatchMQTT(mqtt.Message{Topic: topic, Payload: []byte(action)})
}

func (h *nappaliHarness) expectCmd(want string) {
	h.t.Helper()
	select {
	case got := <-h.cmds:
		if got != want {
			h.t.Fatalf("service call = %q, want %q", got, want)
		}
	case <-time.After(3 * time.Second):
		h.t.Fatalf("no service call, want %q", want)
	}
}

func (h *nappaliHarness) expectSilence() {
	h.t.Helper()
	select {
	case got := <-h.cmds:
		h.t.Fatalf("unexpected service call %q", got)
	case <-time.After(300 * time.Millisecond):
	}
}

// TestNappaliSingleClickTogglesOwnLight: each button owns one light, and a
// single click touches only that one.
func TestNappaliSingleClickTogglesOwnLight(t *testing.T) {
	h := newNappaliHarness(t, "off", "off")

	h.press(nappaliSwitch1, "single")
	h.expectCmd("toggle " + nappaliAblak)
	h.press(nappaliSwitch2, "single")
	h.expectCmd("toggle " + nappaliCsillar)
}

// TestNappaliDoubleAndHoldTakeTheRoom is the reason the script exists rather
// than two one-liners: double and hold act on both lights as a group, from
// either button.
func TestNappaliDoubleAndHoldTakeTheRoom(t *testing.T) {
	h := newNappaliHarness(t, "off", "off")

	h.press(nappaliSwitch1, "double")
	h.expectCmd("turn_on " + nappaliAblak + "," + nappaliCsillar)
	h.press(nappaliSwitch2, "hold")
	h.expectCmd("turn_on " + nappaliAblak + "," + nappaliCsillar)
}

// TestNappaliGroupToggleTurnsOffWhenAnyIsOn: a half-lit room goes dark, it
// does not swap which lamp is on. Toggling the two independently is exactly
// the failure mode this guards.
func TestNappaliGroupToggleTurnsOffWhenAnyIsOn(t *testing.T) {
	h := newNappaliHarness(t, "off", "on")

	h.press(nappaliSwitch1, "double")
	h.expectCmd("turn_off " + nappaliAblak + "," + nappaliCsillar)
}

// TestNappaliIgnoresUnknownAction: the device grows a gesture, or another
// device shares the topic — either way, do nothing rather than guess.
func TestNappaliIgnoresUnknownAction(t *testing.T) {
	h := newNappaliHarness(t, "off", "off")

	h.press(nappaliSwitch1, "release")
	h.expectSilence()
}
