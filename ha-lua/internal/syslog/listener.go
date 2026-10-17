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

// A shorter buffer would silently truncate a long datagram.
const maxDatagram = 64 << 10

type Listener struct {
	conn     *net.UDPConn
	received atomic.Uint64
}

func Listen(ctx context.Context, addr string) (*Listener, error) {
	var lc net.ListenConfig
	pc, err := lc.ListenPacket(ctx, "udp", addr)
	if err != nil {
		return nil, fmt.Errorf("syslog: listen %s: %w", addr, err)
	}
	return &Listener{conn: pc.(*net.UDPConn)}, nil
}

func (l *Listener) Addr() net.Addr { return l.conn.LocalAddr() }

func (l *Listener) Received() uint64 { return l.received.Load() }

// Serve calls onMessage on the read loop; a blocking one loses datagrams.
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
