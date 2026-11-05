package ha

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// QueryFunc sends an argument-less command and returns its result; Client.Query.
type QueryFunc func(ctx context.Context, msgType string) (jsontext.Value, error)

const nameSourceCount = 3

// nameSources are the registries Names mirrors: the event that says one
// changed, the command that lists it, and what to keep from the list.
var nameSources = [nameSourceCount]struct {
	event, list string
	apply       func(*Names, jsontext.Value) error
}{
	{"entity_registry_updated", "config/entity_registry/list_for_display", (*Names).applyEntities},
	{"device_registry_updated", "config/device_registry/list", (*Names).applyDevices},
	{"area_registry_updated", "config/area_registry/list", (*Names).applyAreas},
}

// NameEvents are the event types that make the mirror stale.
var NameEvents = []string{nameSources[0].event, nameSources[1].event, nameSources[2].event}

// A bulk rename fires one event per entity; refetch once the burst is over.
const namesSettle = time.Second

// Names mirrors the naming half of HA's entity, device and area registries:
// enough to answer what HA's entity_name, device_name and area_name templates
// answer. A registry is refetched whole when it changes, as HA's frontend does.
type Names struct {
	mu       sync.RWMutex
	entities map[string]namedEntity
	devices  map[string]namedDevice
	areas    map[string]string

	stale  [nameSourceCount]atomic.Bool
	signal chan struct{}
	settle time.Duration
}

type namedEntity struct {
	name     string
	deviceID string
	areaID   string
}

type namedDevice struct {
	name     string
	areaID   string
	parentID string
}

// NewNames returns an empty mirror; Refresh fills it.
func NewNames() *Names {
	return &Names{
		entities: map[string]namedEntity{},
		devices:  map[string]namedDevice{},
		areas:    map[string]string{},
		signal:   make(chan struct{}, 1),
		settle:   namesSettle,
	}
}

// Refresh refetches every registry, stopping at the first failure.
func (n *Names) Refresh(ctx context.Context, query QueryFunc) error {
	for i := range nameSources {
		if err := n.fetch(ctx, query, i); err != nil {
			return err
		}
	}
	return nil
}

// Changed marks the registry behind eventType stale for Run. Other event types
// are ignored. It never blocks.
func (n *Names) Changed(eventType string) {
	for i, src := range nameSources {
		if src.event == eventType {
			n.stale[i].Store(true)
			select {
			case n.signal <- struct{}{}:
			default:
			}
			return
		}
	}
}

// ChangedAll marks every registry stale: across a reconnect, any of them may
// have changed unseen.
func (n *Names) ChangedAll() {
	for _, src := range nameSources {
		n.Changed(src.event)
	}
}

// Run refetches the stale registries once changes stop arriving for the
// settle period. A failed fetch is logged and left for the next change or
// reconnect.
func (n *Names) Run(ctx context.Context, query QueryFunc) {
	var settled <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-n.signal:
			settled = time.After(n.settle)
		case <-settled:
			settled = nil
			for i := range nameSources {
				if !n.stale[i].Swap(false) {
					continue
				}
				if err := n.fetch(ctx, query, i); err != nil {
					slog.Warn("ha: registry refresh failed", "list", nameSources[i].list, "err", err)
				}
			}
		}
	}
}

func (n *Names) fetch(ctx context.Context, query QueryFunc, source int) error {
	src := nameSources[source]
	raw, err := query(ctx, src.list)
	if err != nil {
		return err
	}
	if err := src.apply(n, raw); err != nil {
		return fmt.Errorf("%s: %w", src.list, err)
	}
	return nil
}

func (n *Names) applyEntities(raw jsontext.Value) error {
	var res struct {
		Entities []struct {
			EntityID string `json:"ei"`
			DeviceID string `json:"di"`
			AreaID   string `json:"ai"`
			Name     string `json:"en"`
		} `json:"entities"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return err
	}
	entities := make(map[string]namedEntity, len(res.Entities))
	for _, e := range res.Entities {
		entities[e.EntityID] = namedEntity{name: e.Name, deviceID: e.DeviceID, areaID: e.AreaID}
	}
	n.mu.Lock()
	n.entities = entities
	n.mu.Unlock()
	return nil
}

func (n *Names) applyDevices(raw jsontext.Value) error {
	var res []struct {
		ID         string `json:"id"`
		Name       string `json:"name"`
		NameByUser string `json:"name_by_user"`
		AreaID     string `json:"area_id"`
		ParentID   string `json:"parent_device_id"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return err
	}
	devices := make(map[string]namedDevice, len(res))
	for _, d := range res {
		name := d.NameByUser
		if name == "" {
			name = d.Name
		}
		devices[d.ID] = namedDevice{name: name, areaID: d.AreaID, parentID: d.ParentID}
	}
	n.mu.Lock()
	n.devices = devices
	n.mu.Unlock()
	return nil
}

func (n *Names) applyAreas(raw jsontext.Value) error {
	var res []struct {
		AreaID string `json:"area_id"`
		Name   string `json:"name"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return err
	}
	areas := make(map[string]string, len(res))
	for _, a := range res {
		areas[a.AreaID] = a.Name
	}
	n.mu.Lock()
	n.areas = areas
	n.mu.Unlock()
	return nil
}

// EntityName is the entity's own name, without its device's: "" for a
// device's main entity. ok is false for an entity the registry does not list
// (disabled, or without a unique_id). The lookups are safe on a nil mirror,
// which lists nothing.
func (n *Names) EntityName(entityID string) (name string, ok bool) {
	if n == nil {
		return "", false
	}
	n.mu.RLock()
	defer n.mu.RUnlock()
	e, ok := n.entities[entityID]
	return e.name, ok
}

// DeviceName names the device behind a device id or one of its entities' ids.
func (n *Names) DeviceName(lookup string) (string, bool) {
	if n == nil {
		return "", false
	}
	n.mu.RLock()
	defer n.mu.RUnlock()
	if d, ok := n.devices[lookup]; ok {
		return d.name, true
	}
	if e, ok := n.entities[lookup]; ok {
		if d, ok := n.devices[e.deviceID]; ok {
			return d.name, true
		}
	}
	return "", false
}

// AreaName resolves an area id, an entity id or a device id, in that order, to
// an area name. An entity's own area wins over its device's.
func (n *Names) AreaName(lookup string) (string, bool) {
	if n == nil {
		return "", false
	}
	n.mu.RLock()
	defer n.mu.RUnlock()
	if name, ok := n.areas[lookup]; ok {
		return name, true
	}
	if e, ok := n.entities[lookup]; ok {
		if e.areaID != "" {
			name, ok := n.areas[e.areaID]
			return name, ok
		}
		return n.deviceArea(e.deviceID)
	}
	return n.deviceArea(lookup)
}

// deviceArea is the device's area, inherited from the parent by a child device
// that has none of its own. The caller holds mu.
func (n *Names) deviceArea(deviceID string) (string, bool) {
	d, ok := n.devices[deviceID]
	if !ok {
		return "", false
	}
	areaID := d.areaID
	if areaID == "" && d.parentID != "" {
		areaID = n.devices[d.parentID].areaID
	}
	name, ok := n.areas[areaID]
	return name, ok
}
