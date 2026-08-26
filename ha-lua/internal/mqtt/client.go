package mqtt

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"
)

// connectTimeout bounds the initial connect. MQTT is optional — every other
// subsystem works without it — so a broker that is down must not hold up
// startup; paho keeps retrying in the background afterwards.
const connectTimeout = 10 * time.Second

// opTimeout bounds a subscribe or publish round trip.
const opTimeout = 5 * time.Second

// ErrDisabled is returned by every operation when no broker is configured.
var ErrDisabled = errors.New("mqtt: no broker configured")

// retryInterval is how often a failed initial connect is retried.
const retryInterval = 30 * time.Second

// pahoLog routes the client library's own diagnostics into slog. Without it
// paho's errors go nowhere, and "the broker rejected our password" reaches
// the user as silence.
type pahoLog struct {
	level slog.Level
}

func (l pahoLog) Println(v ...any) {
	slog.Log(context.Background(), l.level, "mqtt: "+strings.TrimSpace(fmt.Sprintln(v...)))
}
func (l pahoLog) Printf(format string, v ...any) {
	slog.Log(context.Background(), l.level, "mqtt: "+strings.TrimSpace(fmt.Sprintf(format, v...)))
}

var pahoLogOnce sync.Once

// Config is the broker connection. Broker empty disables MQTT entirely.
type Config struct {
	Broker   string // tcp://host:1883, ssl://host:8883, ws://host:9001
	Username string
	Password string
	ClientID string
}

// Message is one inbound publication.
type Message struct {
	Topic      string
	Payload    []byte
	Retained   bool
	ReceivedAt time.Time
}

// Client is the daemon's single broker connection, shared by every script.
// Subscriptions are deduplicated across scripts (the broker gets one
// SUBSCRIBE per distinct filter) and re-sent on every reconnect; routing a
// message to the scripts that asked for it is the caller's job, which keeps
// this type ignorant of scripts entirely.
type Client struct {
	cfg       Config
	onMessage func(Message)

	// subsReady closes once the first OnConnect pass has finished sending the
	// subscriptions the broker had not seen yet. Start waits on it, so a
	// publish issued right after Start cannot beat its own SUBSCRIBE to the
	// broker and vanish.
	subsReady     chan struct{}
	subsReadyOnce sync.Once

	mu          sync.Mutex
	cli         paho.Client
	filters     map[string]struct{}
	connected   bool
	reconnects  int
	lastErr     string
	lastErrAt   time.Time
	connectedAt time.Time
}

// New builds a client. onMessage is called from paho's goroutines, once per
// inbound message, and must not block.
func New(cfg Config, onMessage func(Message)) *Client {
	return &Client{cfg: cfg, onMessage: onMessage, filters: make(map[string]struct{}),
		subsReady: make(chan struct{})}
}

// Enabled reports whether a broker is configured.
func (c *Client) Enabled() bool { return c.cfg.Broker != "" }

// Start connects to the broker. A connect failure is returned but is not
// fatal to the caller: paho retries in the background and re-subscribes
// through the OnConnect handler.
func (c *Client) Start(ctx context.Context) error {
	if !c.Enabled() {
		return ErrDisabled
	}

	pahoLogOnce.Do(func() {
		paho.ERROR = pahoLog{slog.LevelWarn}
		paho.CRITICAL = pahoLog{slog.LevelError}
	})

	opts := paho.NewClientOptions().
		AddBroker(c.cfg.Broker).
		SetClientID(c.clientID()).
		SetUsername(c.cfg.Username).
		SetPassword(c.cfg.Password).
		SetAutoReconnect(true).
		// Deliberately NOT SetConnectRetry: with it, a rejected CONNECT
		// leaves the connect token pending forever, so a wrong password
		// surfaces as "connect timed out" and the broker's actual verdict
		// ("not Authorized") is never seen. Retrying is done below instead,
		// where the error can be logged every time.
		SetConnectTimeout(connectTimeout).
		SetMaxReconnectInterval(1 * time.Minute).
		SetCleanSession(true).
		SetOrderMatters(false)

	opts.SetDefaultPublishHandler(func(_ paho.Client, m paho.Message) {
		c.onMessage(Message{Topic: m.Topic(), Payload: m.Payload(),
			Retained: m.Retained(), ReceivedAt: time.Now()})
	})
	// CleanSession drops the broker-side subscription state on every
	// reconnect, so restoring it here is not an optimization — without it a
	// reconnect silently stops delivering everything.
	opts.SetOnConnectHandler(func(cli paho.Client) {
		c.mu.Lock()
		c.connected = true
		c.connectedAt = time.Now()
		filters := c.filterList()
		c.mu.Unlock()

		slog.Info("mqtt: connected", "broker", c.cfg.Broker, "filters", len(filters))
		for _, f := range filters {
			if err := wait(cli.Subscribe(f, 0, nil)); err != nil {
				slog.Warn("mqtt: resubscribe failed", "filter", f, "err", err)
			}
		}
		c.subsReadyOnce.Do(func() { close(c.subsReady) })
	})
	opts.SetConnectionLostHandler(func(_ paho.Client, err error) {
		c.mu.Lock()
		c.connected = false
		c.reconnects++
		c.lastErr, c.lastErrAt = err.Error(), time.Now()
		c.mu.Unlock()
		slog.Warn("mqtt: connection lost", "err", err)
	})

	cli := paho.NewClient(opts)
	c.mu.Lock()
	c.cli = cli
	c.mu.Unlock()

	if err := connect(cli, c.cfg.Broker); err != nil {
		// Not fatal to the daemon: every other subsystem works without MQTT,
		// and a broker that is down at boot usually comes back.
		go c.retryConnect(ctx, cli)
		return err
	}

	select {
	case <-c.subsReady:
	case <-time.After(connectTimeout):
		slog.Warn("mqtt: subscriptions not confirmed before the connect deadline")
	case <-ctx.Done():
		return ctx.Err()
	}

	go func() {
		<-ctx.Done()
		cli.Disconnect(250)
	}()
	return nil
}

// connect performs one connect attempt and reports the broker's own verdict.
func connect(cli paho.Client, broker string) error {
	token := cli.Connect()
	if !token.WaitTimeout(connectTimeout + time.Second) {
		return fmt.Errorf("mqtt: connect to %s timed out", broker)
	}
	if err := token.Error(); err != nil {
		return fmt.Errorf("mqtt: connect to %s: %w", broker, err)
	}
	return nil
}

// retryConnect keeps trying after a failed initial connect, logging the
// broker's reason each time — a wrong password does not fix itself, and the
// user needs to see why nothing is arriving.
func (c *Client) retryConnect(ctx context.Context, cli paho.Client) {
	ticker := time.NewTicker(retryInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			cli.Disconnect(250)
			return
		case <-ticker.C:
			if cli.IsConnected() {
				return
			}
			if err := connect(cli, c.cfg.Broker); err != nil {
				slog.Warn("mqtt: connect failed, still retrying", "err", err)
				continue
			}
			return
		}
	}
}

// Subscribe registers a topic filter. Idempotent: the broker sees one
// SUBSCRIBE per distinct filter no matter how many scripts ask for it.
func (c *Client) Subscribe(filter string) error {
	if !c.Enabled() {
		return ErrDisabled
	}
	if err := ValidateFilter(filter); err != nil {
		return err
	}

	c.mu.Lock()
	if _, dup := c.filters[filter]; dup {
		c.mu.Unlock()
		return nil
	}
	c.filters[filter] = struct{}{}
	cli, connected := c.cli, c.connected
	c.mu.Unlock()

	// Not connected yet: OnConnect subscribes the whole set, so recording it
	// above is all that is needed.
	if cli == nil || !connected {
		return nil
	}
	return wait(cli.Subscribe(filter, 0, nil))
}

// Publish sends payload to topic.
func (c *Client) Publish(topic string, payload []byte, qos byte, retain bool) error {
	if !c.Enabled() {
		return ErrDisabled
	}
	c.mu.Lock()
	cli := c.cli
	c.mu.Unlock()
	if cli == nil {
		return errors.New("mqtt: not started")
	}
	if !cli.IsConnected() {
		return errors.New("mqtt: not connected")
	}
	return wait(cli.Publish(topic, qos, retain, payload))
}

// Stats reports the link's health for the debug page.
type Stats struct {
	Broker      string    `json:"broker"`
	Connected   bool      `json:"connected"`
	ConnectedAt time.Time `json:"connected_at,omitzero"`
	Reconnects  int       `json:"reconnects"`
	LastError   string    `json:"last_error,omitempty"`
	LastErrorAt time.Time `json:"last_error_at,omitzero"`
	Filters     []string  `json:"filters"`
}

func (c *Client) Stats() Stats {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := Stats{
		Broker:      c.cfg.Broker,
		Connected:   c.connected,
		Reconnects:  c.reconnects,
		LastError:   c.lastErr,
		LastErrorAt: c.lastErrAt,
		Filters:     c.filterList(),
	}
	if c.connected {
		st.ConnectedAt = c.connectedAt
	}
	return st
}

// filterList returns the subscribed filters, sorted. Caller holds c.mu.
func (c *Client) filterList() []string {
	out := make([]string, 0, len(c.filters))
	for f := range c.filters {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

func (c *Client) clientID() string {
	if c.cfg.ClientID != "" {
		return c.cfg.ClientID
	}
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "addon"
	}
	return "ha-lua-" + host
}

func wait(t paho.Token) error {
	if !t.WaitTimeout(opTimeout) {
		return errors.New("mqtt: operation timed out")
	}
	return t.Error()
}
