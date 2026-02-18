package vmm

import (
	"bufio"
	"net"
)

// NetControlChannel implements ControlChannel over a net.Conn.
// Used by libkrun (TCP via TSI) and can be reused by any backend
// that provides a net.Conn (including vsock via unix socket).
type NetControlChannel struct {
	conn    net.Conn
	scanner *bufio.Scanner
}

// NewNetControlChannel wraps a net.Conn as a ControlChannel.
func NewNetControlChannel(conn net.Conn) *NetControlChannel {
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024) // 1MB max message
	return &NetControlChannel{
		conn:    conn,
		scanner: scanner,
	}
}

func (c *NetControlChannel) Send(msg []byte) error {
	// Append newline delimiter if not present
	if len(msg) == 0 || msg[len(msg)-1] != '\n' {
		msg = append(msg, '\n')
	}
	_, err := c.conn.Write(msg)
	return err
}

func (c *NetControlChannel) Recv() ([]byte, error) {
	if c.scanner.Scan() {
		// Return a copy so the caller owns the bytes
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
