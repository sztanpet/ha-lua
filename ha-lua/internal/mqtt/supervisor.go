package mqtt

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"os"
	"time"
)

// supervisorURL is where the Supervisor publishes the credentials of
// the MQTT broker the user already configured (the Mosquitto add-on, usually).
// Reachable only from inside an add-on container, and only when config.yaml
// declares the mqtt service.
// A var, not a const, so tests can point it at a stub.
var supervisorURL = "http://supervisor/services/mqtt"

// Discover asks the Supervisor for the MQTT service. It is the reason the
// add-on needs no MQTT configuration at all in the common case: the user
// already told Home Assistant where their broker is.
//
// Returns a zero Config with no error when the Supervisor reports no MQTT
// service — that is "the user has no broker", not a failure.
func Discover(ctx context.Context) (Config, error) {
	token := os.Getenv("SUPERVISOR_TOKEN")
	if token == "" {
		return Config{}, fmt.Errorf("mqtt: no SUPERVISOR_TOKEN, not running as an add-on")
	}

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, supervisorURL, nil)
	if err != nil {
		return Config{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return Config{}, fmt.Errorf("mqtt: supervisor service query: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// 400 is what the Supervisor answers when no add-on provides the service.
	if resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusNotFound {
		return Config{}, nil
	}
	if resp.StatusCode != http.StatusOK {
		return Config{}, fmt.Errorf("mqtt: supervisor service query: %s", resp.Status)
	}

	var body struct {
		Result string `json:"result"`
		Data   struct {
			Host     string `json:"host"`
			Port     int    `json:"port"`
			SSL      bool   `json:"ssl"`
			Username string `json:"username"`
			Password string `json:"password"`
		} `json:"data"`
	}
	if err := json.UnmarshalRead(resp.Body, &body); err != nil {
		return Config{}, fmt.Errorf("mqtt: supervisor service reply: %w", err)
	}
	if body.Data.Host == "" {
		return Config{}, nil
	}

	scheme := "tcp"
	if body.Data.SSL {
		scheme = "ssl"
	}
	port := body.Data.Port
	if port == 0 {
		port = 1883
	}
	return Config{
		Broker:   fmt.Sprintf("%s://%s:%d", scheme, body.Data.Host, port),
		Username: body.Data.Username,
		Password: body.Data.Password,
	}, nil
}
