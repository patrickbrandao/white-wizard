package client

import (
	"bufio"
	"context"
	"crypto/tls"
	"net"
	"time"
)

// Dialer opens a TCP connection to addr; tests replace it. The default is a
// net.Dialer.
type Dialer func(ctx context.Context, network, addr string) (net.Conn, error)

// conn is one HTTP/1.1 connection written by hand: net/http canonicalizes
// and reorders headers, so it cannot send the bytes of [claudecode.Build].
type conn struct {
	nc net.Conn
	br *bufio.Reader
}

// dial connects to t (TLS for https) with d.
func dial(ctx context.Context, d Dialer, t *Target, tlsConf *tls.Config) (*conn, error) {
	if d == nil {
		d = (&net.Dialer{}).DialContext
	}
	nc, err := d(ctx, "tcp", t.addr)
	if err != nil {
		return nil, err
	}
	if t.tls {
		cfg := &tls.Config{MinVersion: tls.VersionTLS12}
		if tlsConf != nil {
			cfg = tlsConf.Clone()
		}
		cfg.ServerName = t.hostname
		cfg.NextProtos = []string{"http/1.1"}
		tc := tls.Client(nc, cfg)
		if err := tc.HandshakeContext(ctx); err != nil {
			_ = nc.Close()
			return nil, err
		}
		nc = tc
	}
	return &conn{nc: nc, br: bufio.NewReaderSize(nc, 32<<10)}, nil
}

// arm bounds the connection by ctx: its deadline becomes the connection
// deadline and its end interrupts a blocked read or write. The returned
// function disarms it.
func (c *conn) arm(ctx context.Context) func() bool {
	dl, _ := ctx.Deadline()
	_ = c.nc.SetDeadline(dl)
	return context.AfterFunc(ctx, func() { _ = c.nc.SetDeadline(time.Unix(1, 0)) })
}

// exchange writes raw (head and body as built) and reads the response head.
func (c *conn) exchange(raw []byte, head bool) (*Response, error) {
	if _, err := c.nc.Write(raw); err != nil {
		return nil, err
	}
	return readResponse(c.br, head)
}

func (c *conn) close() { _ = c.nc.Close() }
