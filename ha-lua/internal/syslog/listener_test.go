package syslog

import (
	"context"
	"net"
	"testing"
	"time"
)

func TestListenerDelivers(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	l, err := Listen(ctx, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan Message, 1)
	done := make(chan struct{})
	go func() {
		l.Serve(ctx, func(msg Message) { got <- msg })
		close(done)
	}()

	var d net.Dialer
	conn, err := d.DialContext(ctx, "udp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("<30>Oct 17 19:14:53 AmonHen interface,info ether3 link down")); err != nil {
		t.Fatal(err)
	}

	select {
	case msg := <-got:
		if msg.From != "127.0.0.1" || msg.Host != "AmonHen" || msg.Text != "interface,info ether3 link down" {
			t.Errorf("got from %q host %q text %q", msg.From, msg.Host, msg.Text)
		}
		if msg.ReceivedAt.IsZero() {
			t.Error("ReceivedAt not set")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no datagram delivered")
	}
	if l.Received() != 1 {
		t.Errorf("Received = %d, want 1", l.Received())
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return on cancel")
	}
}

func TestListenBindFailure(t *testing.T) {
	l, err := Listen(t.Context(), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.conn.Close()
	if _, err := Listen(t.Context(), l.Addr().String()); err == nil {
		t.Fatal("second bind of the same port succeeded")
	}
}
