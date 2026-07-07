package ha

import (
	"time"

	"github.com/go-json-experiment/json/jsontext"
)

// Outgoing message types
type authMsg struct {
	Type        string `json:"type"`
	AccessToken string `json:"access_token"`
}

type subscribeMsg struct {
	ID        int    `json:"id"`
	Type      string `json:"type"`
	EventType string `json:"event_type"`
}

type commandMsg struct {
	ID   int    `json:"id"`
	Type string `json:"type"`
}

// Incoming envelope — type-sniff before full decode
type incomingMsg struct {
	Type string `json:"type"`
	ID   int    `json:"id,omitempty"`
}

// resultMsg is HA's response frame to a command (call_service, subscribe, …):
// success plus, on failure, an error code/message.
type resultMsg struct {
	ID      int  `json:"id"`
	Success bool `json:"success"`
	Error   struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// Result of a get_states command
type getStatesResult struct {
	ID     int         `json:"id"`
	Type   string      `json:"type"`
	Result []StateData `json:"result"`
}

// StateData is the canonical state object returned by HA
type StateData struct {
	EntityID    string         `json:"entity_id"`
	State       string         `json:"state"`
	Attributes  jsontext.Value `json:"attributes"`
	LastChanged string         `json:"last_changed"`
	LastUpdated string         `json:"last_updated"`
}

// Event is the parsed event envelope delivered to consumers
type Event struct {
	Type      string         `json:"event_type"`
	TimeFired string         `json:"time_fired"`
	Data      jsontext.Value `json:"data"`
	// ReceivedAt is stamped by the read loop when the frame arrives, so
	// consumers can measure how long an event waited for its handler
	// (tracker write, channel, batch window, a parked event loop). Not a
	// wire field.
	ReceivedAt time.Time `json:"-"`
}

// eventEnvelope matches the outer "event" wrapper in a subscription message
type eventEnvelope struct {
	Type  string `json:"type"`
	ID    int    `json:"id"`
	Event Event  `json:"event"`
}
