package harness

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"syscall"
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

	f := os.NewFile(uintptr(fd), fmt.Sprintf("vsock:%d:%d", cid, port))
	conn, err := net.FileConn(f)
	f.Close() // FileConn dups the fd
	if err != nil {
		return nil, fmt.Errorf("FileConn from vsock fd: %w", err)
	}

	return conn, nil
}
