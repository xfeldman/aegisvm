package harness

// Minimal netlink helpers for network configuration.
// Used by setupNetwork() to configure eth0 without shelling out to `ip`.
// This avoids requiring iproute2/busybox in the guest rootfs.

import (
	"encoding/binary"
	"fmt"
	"net"
	"syscall"
	"unsafe"
)

// Netlink message types and flags
const (
	rtmNewAddr  = 20
	rtmNewRoute = 24
	rtmNewLink  = 16

	nlmFRequest = 1
	nlmFAck     = 4
	nlmFCreate  = 0x400
	nlmFExcl    = 0x200

	iflaIfname = 3

	ifaAddress = 1
	ifaLocal   = 2

	rtaGateway = 5
)

// netlinkSetLinkUp brings an interface up by index.
func netlinkSetLinkUp(ifIndex int) error {
	s, err := syscall.Socket(syscall.AF_NETLINK, syscall.SOCK_RAW, syscall.NETLINK_ROUTE)
	if err != nil {
		return fmt.Errorf("netlink socket: %w", err)
	}
	defer syscall.Close(s)

	sa := &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}
	if err := syscall.Bind(s, sa); err != nil {
		return fmt.Errorf("netlink bind: %w", err)
	}

	// struct ifinfomsg
	type ifInfoMsg struct {
		Family uint8
		_      uint8
		Type   uint16
		Index  int32
		Flags  uint32
		Change uint32
	}

	info := ifInfoMsg{
		Index:  int32(ifIndex),
		Flags:  syscall.IFF_UP,
		Change: syscall.IFF_UP,
	}

	infoBytes := (*[unsafe.Sizeof(info)]byte)(unsafe.Pointer(&info))[:]

	return netlinkRequest(s, rtmNewLink, nlmFRequest|nlmFAck, infoBytes)
}

// netlinkAddAddr adds an IP address to an interface.
// ipStr is CIDR notation like "192.168.127.2/24".
func netlinkAddAddr(ifIndex int, ipStr string) error {
	ip, ipNet, err := net.ParseCIDR(ipStr)
	if err != nil {
		return fmt.Errorf("parse CIDR %q: %w", ipStr, err)
	}
	ip4 := ip.To4()
	if ip4 == nil {
		return fmt.Errorf("only IPv4 supported, got %s", ip)
	}
	prefixLen, _ := ipNet.Mask.Size()

	s, err := syscall.Socket(syscall.AF_NETLINK, syscall.SOCK_RAW, syscall.NETLINK_ROUTE)
	if err != nil {
		return fmt.Errorf("netlink socket: %w", err)
	}
	defer syscall.Close(s)

	sa := &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}
	if err := syscall.Bind(s, sa); err != nil {
		return fmt.Errorf("netlink bind: %w", err)
	}

	// struct ifaddrmsg
	type ifAddrMsg struct {
		Family    uint8
		PrefixLen uint8
		Flags     uint8
		Scope     uint8
		Index     uint32
	}

	msg := ifAddrMsg{
		Family:    syscall.AF_INET,
		PrefixLen: uint8(prefixLen),
		Scope:     0, // RT_SCOPE_UNIVERSE
		Index:     uint32(ifIndex),
	}
	msgBytes := (*[unsafe.Sizeof(msg)]byte)(unsafe.Pointer(&msg))[:]

	// Add IFA_LOCAL and IFA_ADDRESS attributes
	payload := append(msgBytes, rtaBytes(ifaLocal, ip4)...)
	payload = append(payload, rtaBytes(ifaAddress, ip4)...)

	return netlinkRequest(s, rtmNewAddr, nlmFRequest|nlmFAck|nlmFCreate|nlmFExcl, payload)
}

// netlinkAddDefaultRoute adds a default route via the given gateway.
func netlinkAddDefaultRoute(gwStr string) error {
	gw := net.ParseIP(gwStr).To4()
	if gw == nil {
		return fmt.Errorf("invalid gateway IP %q", gwStr)
	}

	s, err := syscall.Socket(syscall.AF_NETLINK, syscall.SOCK_RAW, syscall.NETLINK_ROUTE)
	if err != nil {
		return fmt.Errorf("netlink socket: %w", err)
	}
	defer syscall.Close(s)

	sa := &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}
	if err := syscall.Bind(s, sa); err != nil {
		return fmt.Errorf("netlink bind: %w", err)
	}

	// struct rtmsg
	type rtMsg struct {
		Family   uint8
		DstLen   uint8
		SrcLen   uint8
		TOS      uint8
		Table    uint8
		Protocol uint8
		Scope    uint8
		Type     uint8
		Flags    uint32
	}

	msg := rtMsg{
		Family:   syscall.AF_INET,
		Table:    syscall.RT_TABLE_MAIN,
		Protocol: 4, // RTPROT_STATIC
		Scope:    0, // RT_SCOPE_UNIVERSE
		Type:     1, // RTN_UNICAST
	}
	msgBytes := (*[unsafe.Sizeof(msg)]byte)(unsafe.Pointer(&msg))[:]

	// Add RTA_GATEWAY attribute
	payload := append(msgBytes, rtaBytes(rtaGateway, gw)...)

	return netlinkRequest(s, rtmNewRoute, nlmFRequest|nlmFAck|nlmFCreate|nlmFExcl, payload)
}

// netlinkRequest sends a netlink request and waits for ACK.
func netlinkRequest(s int, msgType uint16, flags uint16, payload []byte) error {
	// Build netlink message header
	hdrLen := 16 // sizeof(nlmsghdr)
	totalLen := hdrLen + len(payload)

	buf := make([]byte, totalLen)
	binary.LittleEndian.PutUint32(buf[0:4], uint32(totalLen))   // nlmsg_len
	binary.LittleEndian.PutUint16(buf[4:6], msgType)             // nlmsg_type
	binary.LittleEndian.PutUint16(buf[6:8], flags)               // nlmsg_flags
	binary.LittleEndian.PutUint32(buf[8:12], 1)                  // nlmsg_seq
	binary.LittleEndian.PutUint32(buf[12:16], 0)                 // nlmsg_pid
	copy(buf[hdrLen:], payload)

	dst := &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}
	if err := syscall.Sendto(s, buf, 0, dst); err != nil {
		return fmt.Errorf("sendto: %w", err)
	}

	// Wait for ACK/error
	resp := make([]byte, 4096)
	n, _, err := syscall.Recvfrom(s, resp, 0)
	if err != nil {
		return fmt.Errorf("recvfrom: %w", err)
	}

	if n < 16 {
		return fmt.Errorf("short netlink response (%d bytes)", n)
	}

	// Check for NLMSG_ERROR (type 2)
	respType := binary.LittleEndian.Uint16(resp[4:6])
	if respType == 2 { // NLMSG_ERROR
		if n < 20 {
			return fmt.Errorf("short error response")
		}
		errno := int32(binary.LittleEndian.Uint32(resp[16:20]))
		if errno != 0 {
			return fmt.Errorf("netlink error: %s", syscall.Errno(-errno))
		}
	}

	return nil
}

// rtaBytes builds a netlink route attribute (struct rtattr + data).
func rtaBytes(attrType uint16, data []byte) []byte {
	rtaLen := 4 + len(data)              // sizeof(rtattr) = 4
	padLen := (rtaLen + 3) & ^3          // align to 4 bytes
	buf := make([]byte, padLen)
	binary.LittleEndian.PutUint16(buf[0:2], uint16(rtaLen))
	binary.LittleEndian.PutUint16(buf[2:4], attrType)
	copy(buf[4:], data)
	return buf
}
