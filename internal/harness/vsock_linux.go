package harness

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"syscall"
	"time"
	"unsafe"
)

// AF_VSOCK is the address family for vsock (not in Go's syscall package).
const afVSOCK = 40

// sockaddrVM is the C struct sockaddr_vm for AF_VSOCK.
// Must match the kernel's struct layout exactly.
type sockaddrVM struct {
	family    uint16
	reserved1 uint16
	port      uint32
	cid       uint32
	flags     uint8
	zeroPad   [3]uint8 // alignment padding
}

// dialVsock connects to the host via AF_VSOCK.
// portStr is the vsock port number (from AEGIS_VSOCK_PORT env var).
// cidStr is the vsock CID (from AEGIS_VSOCK_CID env var, default "2" = host).
func dialVsock(portStr, cidStr string) (net.Conn, error) {
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return nil, fmt.Errorf("invalid vsock port %q: %w", portStr, err)
	}

	cid := uint32(2) // VMADDR_CID_HOST
	if cidStr != "" {
		c, err := strconv.ParseUint(cidStr, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("invalid vsock CID %q: %w", cidStr, err)
		}
		cid = uint32(c)
	}

	fd, err := syscall.Socket(afVSOCK, syscall.SOCK_STREAM, 0)
	if err != nil {
		return nil, fmt.Errorf("socket(AF_VSOCK): %w", err)
	}

	addr := sockaddrVM{
		family: afVSOCK,
		port:   uint32(port),
		cid:    cid,
	}

	_, _, errno := syscall.RawSyscall(
		syscall.SYS_CONNECT,
		uintptr(fd),
		uintptr(unsafe.Pointer(&addr)),
		unsafe.Sizeof(addr),
	)
	if errno != 0 {
		syscall.Close(fd)
		return nil, fmt.Errorf("connect(AF_VSOCK, cid=%d, port=%d): %w", cid, port, errno)
	}

	// net.FileConn doesn't understand AF_VSOCK (getsockname fails).
	// Wrap the fd in os.File which provides Read/Write/Close, then
	// wrap that in vsockConn to satisfy net.Conn.
	f := os.NewFile(uintptr(fd), fmt.Sprintf("vsock:%d:%d", cid, port))
	return &vsockConn{file: f, cid: cid, port: uint32(port)}, nil
}

// vsockConn wraps an os.File over a vsock fd to implement net.Conn.
// Go's net.FileConn doesn't support AF_VSOCK, so we provide a minimal wrapper.
type vsockConn struct {
	file *os.File
	cid  uint32
	port uint32
}

func (c *vsockConn) Read(b []byte) (int, error)  { return c.file.Read(b) }
func (c *vsockConn) Write(b []byte) (int, error) { return c.file.Write(b) }
func (c *vsockConn) Close() error                { return c.file.Close() }

func (c *vsockConn) LocalAddr() net.Addr {
	return &vsockAddr{cid: 3, port: 0} // CID 3 = guest
}

func (c *vsockConn) RemoteAddr() net.Addr {
	return &vsockAddr{cid: c.cid, port: c.port}
}

func (c *vsockConn) SetDeadline(t time.Time) error      { return c.file.SetDeadline(t) }
func (c *vsockConn) SetReadDeadline(t time.Time) error   { return c.file.SetReadDeadline(t) }
func (c *vsockConn) SetWriteDeadline(t time.Time) error  { return c.file.SetWriteDeadline(t) }

// vsockAddr implements net.Addr for AF_VSOCK.
type vsockAddr struct {
	cid  uint32
	port uint32
}

func (a *vsockAddr) Network() string { return "vsock" }
func (a *vsockAddr) String() string  { return fmt.Sprintf("vsock:%d:%d", a.cid, a.port) }
