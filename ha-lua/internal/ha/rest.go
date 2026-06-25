package ha

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-json-experiment/json"
	"github.com/go-json-experiment/json/jsontext"
)

// restTimeout bounds a single core-REST request. State publishing rides the
// per-minute control loop, so a stuck call must not pin a script goroutine.
const restTimeout = 10 * time.Second

// deriveRESTURL turns a HA WebSocket URL into the core REST API base. The WS
// and REST endpoints share a host but differ in scheme (ws→http, wss→https)
// and in their path tail. Two real forms must both normalise to a base ending
// in /api:
//
//	ws://supervisor/core/websocket        -> http://supervisor/core/api  (add-on)
//	ws://localhost:8123/api/websocket     -> http://localhost:8123/api   (dev)
//
// The Supervisor's WS path (/core/websocket) lacks the /api segment the REST
// API lives under (/core/api), while a direct HA URL already carries it
// (/api/websocket). So after dropping the trailing /websocket we ensure the
// base ends in /api. An empty url yields an empty base (REST disabled).
func deriveRESTURL(wsURL string) string {
	if wsURL == "" {
		return ""
	}
	rest := wsURL
	switch {
	case strings.HasPrefix(rest, "wss://"):
		rest = "https://" + strings.TrimPrefix(rest, "wss://")
	case strings.HasPrefix(rest, "ws://"):
		rest = "http://" + strings.TrimPrefix(rest, "ws://")
	}
	rest = strings.TrimSuffix(rest, "/websocket")
	if !strings.HasSuffix(rest, "/api") {
		rest += "/api"
	}
	return rest
}

// setStateBody is the POST /states/{id} payload. Attributes are omitted when
// empty so HA keeps whatever it had.
type setStateBody struct {
	State      string         `json:"state"`
	Attributes jsontext.Value `json:"attributes,omitempty"`
}

// SetState creates or updates an entity through the core REST API
// (POST /api/states/{id}). created is true when HA registered a new entity
// (HTTP 201) versus updating an existing one (HTTP 200). These states are not
// integration-backed, so an HA restart drops them — callers re-publish to
// self-heal.
func (c *Client) SetState(ctx context.Context, entityID, state string, attrs jsontext.Value) (created bool, err error) {
	if c.restURL == "" {
		return false, fmt.Errorf("ha: REST API base URL not configured")
	}
	raw, err := json.Marshal(setStateBody{State: state, Attributes: attrs})
	if err != nil {
		return false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.restURL+"/states/"+entityID, bytes.NewReader(raw))
	if err != nil {
		return false, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")

	res, err := c.httpClient.Do(req)
	if err != nil {
		return false, err
	}
	defer res.Body.Close()
	switch res.StatusCode {
	case http.StatusOK:
		return false, nil
	case http.StatusCreated:
		return true, nil
	default:
		return false, fmt.Errorf("ha: set_state %s: status %d", entityID, res.StatusCode)
	}
}

// RemoveState deletes an entity previously set via SetState
// (DELETE /api/states/{id}). A 404 counts as success: the entity is already
// gone, which is the intended end state.
func (c *Client) RemoveState(ctx context.Context, entityID string) error {
	if c.restURL == "" {
		return fmt.Errorf("ha: REST API base URL not configured")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.restURL+"/states/"+entityID, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)

	res, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	switch res.StatusCode {
	case http.StatusOK, http.StatusNotFound:
		return nil
	default:
		return fmt.Errorf("ha: remove_state %s: status %d", entityID, res.StatusCode)
	}
}
