package vmm

import (
	"bufio"
	"context"
	"net"
	"time"
)

// NetControlChannel implements ControlChannel over a net.Conn.
// Used by libkrun (TCP via TSI) and reusable by any backend that provides
// a net.Conn (including Firecracker vsock via unix socket).
//
// Framing: newline-delimited JSON. Send appends '\n', Recv strips it.
type NetControlChannel struct {
	conn    net.Conn
	scanner *bufio.Scanner
}

func NewNetControlChannel(conn net.Conn) *NetControlChannel {
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024) // 1MB max message
	return &NetControlChannel{
		conn:    conn,
		scanner: scanner,
	}
}

func (c *NetControlChannel) Send(ctx context.Context, msg []byte) error {
	if deadline, ok := ctx.Deadline(); ok {
		c.conn.SetWriteDeadline(deadline)
		defer c.conn.SetWriteDeadline(time.Time{})
	}

	// Append newline delimiter if not present
	if len(msg) == 0 || msg[len(msg)-1] != '\n' {
		msg = append(msg, '\n')
	}
	_, err := c.conn.Write(msg)
	return err
}

func (c *NetControlChannel) Recv(ctx context.Context) ([]byte, error) {
	if deadline, ok := ctx.Deadline(); ok {
		c.conn.SetReadDeadline(deadline)
		defer c.conn.SetReadDeadline(time.Time{})
	}

	if c.scanner.Scan() {
		line := c.scanner.Bytes()
		out := make([]byte, len(line))
		copy(out, line)
		return out, nil
	}
	if err := c.scanner.Err(); err != nil {
		return nil, err
	}
	return nil, net.ErrClosed
}

func (c *NetControlChannel) Close() error {
	return c.conn.Close()
}
