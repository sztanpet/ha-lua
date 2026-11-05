package ha

import (
	"context"
	"encoding/json/jsontext"
	"fmt"
	"sync"
	"testing"
	"time"
)

// registryFixture answers the three list commands the way HA 2026.11 does.
type registryFixture struct {
	mu    sync.Mutex
	calls map[string]int
	lists map[string]string
}

func newRegistryFixture() *registryFixture {
	return &registryFixture{
		calls: map[string]int{},
		lists: map[string]string{
			"config/entity_registry/list_for_display": `{"entity_categories":{"0":"config","1":"diagnostic"},"entities":[
				{"ei":"binary_sensor.halo_ablak","pl":"mqtt","di":"dev-window","np":"device"},
				{"ei":"sensor.halo_ablak_battery","pl":"mqtt","di":"dev-window","np":"device","en":"Battery","ec":1},
				{"ei":"sensor.lamp_power","pl":"mqtt","di":"dev-lamp","ai":"galeria","np":"area","en":"Power"},
				{"ei":"climate.halo_futes","pl":"esphome","di":"dev-zone","np":"device"},
				{"ei":"input_boolean.nighttime","pl":"input_boolean","en":"Nighttime"}]}`,
			"config/device_registry/list": `[
				{"id":"dev-window","name":"sonoff-door-1","name_by_user":"Ablak","area_id":"halo","parent_device_id":null},
				{"id":"dev-lamp","name":"Lámpa","name_by_user":null,"area_id":"halo","parent_device_id":null},
				{"id":"dev-board","name":"Fűtésvezérlő","name_by_user":null,"area_id":"konyha","parent_device_id":null},
				{"id":"dev-zone","name":"Fűtés","name_by_user":null,"area_id":null,"parent_device_id":"dev-board"}]`,
			"config/area_registry/list": `[
				{"area_id":"halo","name":"Háló","floor_id":null},
				{"area_id":"galeria","name":"Galéria","floor_id":null},
				{"area_id":"konyha","name":"Konyha","floor_id":null}]`,
		},
	}
}

func (f *registryFixture) query(_ context.Context, msgType string) (jsontext.Value, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[msgType]++
	list, ok := f.lists[msgType]
	if !ok {
		return nil, fmt.Errorf("unknown command %s", msgType)
	}
	return jsontext.Value(list), nil
}

func (f *registryFixture) count(msgType string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[msgType]
}

func TestNamesLookups(t *testing.T) {
	names := NewNames()
	if err := names.Refresh(context.Background(), newRegistryFixture().query); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	for _, tc := range []struct {
		lookup     func(string) (string, bool)
		what, id   string
		want       string
		wantListed bool
	}{
		{names.EntityName, "entity_name", "binary_sensor.halo_ablak", "", true}, // main entity
		{names.EntityName, "entity_name", "sensor.halo_ablak_battery", "Battery", true},
		{names.EntityName, "entity_name", "sensor.nope", "", false},
		{names.EntityName, "entity_name", "input_boolean.nighttime", "Nighttime", true},
		{names.DeviceName, "device_name", "sensor.halo_ablak_battery", "Ablak", true}, // name_by_user wins
		{names.DeviceName, "device_name", "dev-lamp", "Lámpa", true},
		{names.DeviceName, "device_name", "input_boolean.nighttime", "", false},
		{names.AreaName, "area_name", "sensor.halo_ablak_battery", "Háló", true},
		{names.AreaName, "area_name", "sensor.lamp_power", "Galéria", true}, // own area beats the device's
		{names.AreaName, "area_name", "climate.halo_futes", "Konyha", true}, // child device inherits
		{names.AreaName, "area_name", "dev-window", "Háló", true},
		{names.AreaName, "area_name", "konyha", "Konyha", true},
		{names.AreaName, "area_name", "input_boolean.nighttime", "", false},
	} {
		got, listed := tc.lookup(tc.id)
		if got != tc.want || listed != tc.wantListed {
			t.Errorf("%s(%q) = %q, %v; want %q, %v", tc.what, tc.id, got, listed, tc.want, tc.wantListed)
		}
	}
}

// A burst of registry events costs one refetch of the registry that changed,
// and none of the others.
func TestNamesRunCoalesces(t *testing.T) {
	fixture := newRegistryFixture()
	names := NewNames()
	names.settle = 20 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go names.Run(ctx, fixture.query)

	for range 50 {
		names.Changed("entity_registry_updated")
	}
	names.Changed("state_changed")
	deadline := time.Now().Add(2 * time.Second)
	for fixture.count("config/entity_registry/list_for_display") == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)

	if got := fixture.count("config/entity_registry/list_for_display"); got != 1 {
		t.Errorf("entity list fetched %d times, want 1", got)
	}
	if got := fixture.count("config/device_registry/list") + fixture.count("config/area_registry/list"); got != 0 {
		t.Errorf("unchanged registries fetched %d times, want 0", got)
	}
	if name, _ := names.EntityName("sensor.halo_ablak_battery"); name != "Battery" {
		t.Errorf("after the refetch EntityName = %q, want Battery", name)
	}
}
