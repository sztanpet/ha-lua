package mqtt

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// withSupervisor points the discovery at a stub and supplies a token.
func withSupervisor(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	t.Setenv("SUPERVISOR_TOKEN", "tok")
	old := supervisorURL
	supervisorURL = srv.URL
	t.Cleanup(func() { supervisorURL = old })
}

func TestDiscoverBuildsBrokerURL(t *testing.T) {
	withSupervisor(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("Authorization = %q", got)
		}
		_, _ = w.Write([]byte(`{"result":"ok","data":{"host":"core-mosquitto","port":1883,"ssl":false,"username":"addons","password":"s3cr3t"}}`))
	})

	cfg, err := Discover(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Broker != "tcp://core-mosquitto:1883" || cfg.Username != "addons" || cfg.Password != "s3cr3t" {
		t.Errorf("Discover = %+v", cfg)
	}
}

func TestDiscoverSSL(t *testing.T) {
	withSupervisor(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"result":"ok","data":{"host":"broker","port":8883,"ssl":true}}`))
	})
	cfg, err := Discover(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Broker != "ssl://broker:8883" {
		t.Errorf("Broker = %q, want ssl://broker:8883", cfg.Broker)
	}
}

// No MQTT add-on installed: the Supervisor answers 400. That is "the user has
// no broker", not an error to log every boot.
func TestDiscoverNoService(t *testing.T) {
	withSupervisor(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	})
	cfg, err := Discover(t.Context())
	if err != nil {
		t.Fatalf("Discover = %v, want no error when no service is provided", err)
	}
	if cfg.Broker != "" {
		t.Errorf("Broker = %q, want empty", cfg.Broker)
	}
}

func TestDiscoverOutsideAddon(t *testing.T) {
	t.Setenv("SUPERVISOR_TOKEN", "")
	if _, err := Discover(t.Context()); err == nil {
		t.Error("Discover succeeded with no SUPERVISOR_TOKEN")
	}
}
