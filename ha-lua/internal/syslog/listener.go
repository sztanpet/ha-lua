package syslog

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync/atomic"
	"time"
)

// maxDatagram is the largest UDP payload; a shorter buffer would truncate.
const maxDatagram = 64 << 10

// Listener reads syslog datagrams from one UDP socket.
type Listener struct {
	conn     *net.UDPConn
	received atomic.Uint64
}

// Listen binds addr (":5514"). Serve must be called to read from it.
func Listen(ctx context.Context, addr string) (*Listener, error) {
	var lc net.ListenConfig
	pc, err := lc.ListenPacket(ctx, "udp", addr)
	if err != nil {
		return nil, fmt.Errorf("syslog: listen %s: %w", addr, err)
	}
	return &Listener{conn: pc.(*net.UDPConn)}, nil
}

// Addr is the bound address; tests listen on port 0 and need the real one.
func (l *Listener) Addr() net.Addr { return l.conn.LocalAddr() }

// Received counts datagrams read since Listen.
func (l *Listener) Received() uint64 { return l.received.Load() }

// Serve hands each datagram to onMessage until ctx is done, then closes the
// socket. onMessage runs on this goroutine and must not block.
func (l *Listener) Serve(ctx context.Context, onMessage func(Message)) {
	stop := context.AfterFunc(ctx, func() { l.conn.Close() })
	defer stop()
	buf := make([]byte, maxDatagram)
	for {
		n, from, err := l.conn.ReadFromUDPAddrPort(buf)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			slog.Warn("syslog: read failed", "err", err)
			continue
		}
		l.received.Add(1)
		msg := Parse(buf[:n])
		msg.From = from.Addr().Unmap().String()
		msg.ReceivedAt = time.Now()
		onMessage(msg)
	}
}
