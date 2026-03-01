package vmm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xfeldman/aegisvm/internal/config"
)

// CloudHypervisorVMM implements the VMM interface using Cloud Hypervisor on Linux.
// Communication with CH is via its unix socket REST API — no cgo, no external SDK.
type CloudHypervisorVMM struct {
	mu        sync.Mutex
	instances map[string]*chInstance

	chBin        string // path to cloud-hypervisor binary
	virtiofsdBin string // path to virtiofsd binary
	kernelPath   string // path to vmlinux
	cfg          *config.Config

	subnetCounter uint32 // monotonic counter for /30 subnet allocation
}

// chInstance holds per-VM state for a Cloud Hypervisor instance.
type chInstance struct {
	id     string
	config VMConfig

	// Process handles
	chCmd        *exec.Cmd // cloud-hypervisor process
	virtiofsdCmd *exec.Cmd // virtiofsd sidecar (nil if no workspace)
	done         chan struct{}

	// Paths
	apiSocket      string // CH REST API unix socket
	vsockSocket    string // vsock unix socket path (without _PORT suffix)
	virtiofsdSocket string // virtiofsd socket path

	// Networking
	tapName string // tap device name (e.g. "aegis0")
	guestIP string // guest IP (e.g. "172.16.0.2")
	hostIP  string // host-side tap IP (e.g. "172.16.0.1")

	// Resolved endpoints
	endpoints []HostEndpoint
}

// chClient is an HTTP client that dials a unix socket for the CH REST API.
type chClient struct {
	client *http.Client
	base   string // e.g. "http://localhost"
}

func newCHClient(socketPath string) *chClient {
	return &chClient{
		client: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return net.DialTimeout("unix", socketPath, 5*time.Second)
				},
			},
			Timeout: 30 * time.Second,
		},
		base: "http://localhost",
	}
}

func (c *chClient) put(path string, body interface{}) (*http.Response, error) {
	var bodyReader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal request body: %w", err)
		}
		bodyReader = strings.NewReader(string(data))
	}

	req, err := http.NewRequest("PUT", c.base+path, bodyReader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.client.Do(req)
}

func (c *chClient) get(path string) (*http.Response, error) {
	return c.client.Get(c.base + path)
}

// NewCloudHypervisorVMM creates a new Cloud Hypervisor VMM backend.
// Requires root or CAP_NET_ADMIN for tap networking.
func NewCloudHypervisorVMM(cfg *config.Config) (*CloudHypervisorVMM, error) {
	// Fail fast if not root
	if os.Geteuid() != 0 {
		return nil, fmt.Errorf("cloud-hypervisor backend requires root for tap networking")
	}

	// Check cloud-hypervisor binary (resolved by cfg.ResolveBinaries)
	chBin := cfg.CloudHypervisorBin
	if chBin == "" {
		return nil, fmt.Errorf("cloud-hypervisor not found (install via: make cloud-hypervisor)")
	}

	// Check virtiofsd binary (resolved by cfg.ResolveBinaries)
	virtiofsdBin := cfg.VirtiofsdBin
	if virtiofsdBin == "" {
		return nil, fmt.Errorf("virtiofsd not found (install via: apt install virtiofsd)")
	}

	// Check kernel exists
	if _, err := os.Stat(cfg.KernelPath); err != nil {
		return nil, fmt.Errorf("kernel not found at %s (build via 'make kernel'): %w", cfg.KernelPath, err)
	}

	// Clean up orphaned tap devices and NAT rules from a previous crash.
	// On clean shutdown, StopVM removes these. On crash, they leak.
	cleanupOrphanedTaps()

	return &CloudHypervisorVMM{
		instances:    make(map[string]*chInstance),
		chBin:        chBin,
		virtiofsdBin: virtiofsdBin,
		kernelPath:   cfg.KernelPath,
		cfg:          cfg,
	}, nil
}

func (v *CloudHypervisorVMM) CreateVM(cfg VMConfig) (Handle, error) {
	if cfg.Rootfs.Type != RootFSBlockImage {
		return Handle{}, fmt.Errorf("cloud-hypervisor backend requires RootFSBlockImage, got %s", cfg.Rootfs.Type)
	}

	id := fmt.Sprintf("vm-%d", time.Now().UnixNano())

	// Allocate /30 subnet
	idx := atomic.AddUint32(&v.subnetCounter, 1) - 1
	// 172.16.0.0/12 → use third and fourth octets from counter
	// Each /30 gives 4 IPs: .0 = network, .1 = host, .2 = guest, .3 = broadcast
	thirdOctet := idx / 64
	fourthBase := (idx % 64) * 4
	if thirdOctet > 255 {
		return Handle{}, fmt.Errorf("subnet space exhausted (over 16384 VMs)")
	}
	hostIP := fmt.Sprintf("172.16.%d.%d", thirdOctet, fourthBase+1)
	guestIP := fmt.Sprintf("172.16.%d.%d", thirdOctet, fourthBase+2)
	tapName := fmt.Sprintf("aegis%d", idx)

	sockDir := filepath.Join(v.cfg.DataDir, "sockets")
	inst := &chInstance{
		id:              id,
		config:          cfg,
		done:            make(chan struct{}),
		apiSocket:       filepath.Join(sockDir, fmt.Sprintf("ch-api-%s.sock", id)),
		vsockSocket:     filepath.Join(sockDir, fmt.Sprintf("ch-vsock-%s.sock", id)),
		virtiofsdSocket: filepath.Join(sockDir, fmt.Sprintf("ch-virtiofsd-%s.sock", id)),
		tapName:         tapName,
		guestIP:         guestIP,
		hostIP:          hostIP,
	}

	// Build endpoints — for tap networking, router dials guestIP:guestPort directly
	for _, ep := range cfg.ExposePorts {
		inst.endpoints = append(inst.endpoints, HostEndpoint{
			GuestPort:   ep.GuestPort,
			HostPort:    ep.GuestPort, // same port — no random allocation needed
			Protocol:    ep.Protocol,
			BackendAddr: guestIP,
		})
	}

	v.mu.Lock()
	defer v.mu.Unlock()
	v.instances[id] = inst

	return Handle{ID: id}, nil
}

func (v *CloudHypervisorVMM) StartVM(h Handle) (ControlChannel, error) {
	v.mu.Lock()
	inst, ok := v.instances[h.ID]
	if !ok {
		v.mu.Unlock()
		return nil, fmt.Errorf("vm %s not found", h.ID)
	}
	cfg := inst.config
	v.mu.Unlock()

	// 1. Enable IP forwarding
	if err := enableIPForward(); err != nil {
		return nil, fmt.Errorf("enable ip_forward: %w", err)
	}

	// 2. Create tap device + NAT rules
	if err := createTap(inst.tapName, inst.hostIP); err != nil {
		return nil, fmt.Errorf("create tap %s: %w", inst.tapName, err)
	}
	if err := setupNAT(inst.tapName, inst.guestIP); err != nil {
		destroyTap(inst.tapName)
		return nil, fmt.Errorf("setup NAT: %w", err)
	}

	// 3. Spawn virtiofsd if workspace is configured
	if cfg.WorkspacePath != "" {
		if err := v.startVirtiofsd(inst); err != nil {
			removeNAT(inst.tapName, inst.guestIP)
			destroyTap(inst.tapName)
			return nil, fmt.Errorf("start virtiofsd: %w", err)
		}
	}

	// 4. Pre-create vsock unix socket listener for harness connection.
	vsockListenPath := fmt.Sprintf("%s_%d", inst.vsockSocket, harnessVsockPort)
	os.Remove(vsockListenPath) // clean stale
	os.Remove(inst.vsockSocket) // clean base socket (CH binds this)
	vsockLn, err := net.Listen("unix", vsockListenPath)
	if err != nil {
		v.cleanupInstance(inst)
		return nil, fmt.Errorf("listen vsock unix socket: %w", err)
	}

	// 5. Spawn cloud-hypervisor process
	os.Remove(inst.apiSocket) // clean stale
	chCmd := exec.Command(v.chBin, "--api-socket", inst.apiSocket)
	chCmd.Stdout = os.Stdout
	chCmd.Stderr = os.Stderr
	if err := chCmd.Start(); err != nil {
		vsockLn.Close()
		v.cleanupInstance(inst)
		return nil, fmt.Errorf("start cloud-hypervisor: %w", err)
	}

	v.mu.Lock()
	inst.chCmd = chCmd
	v.mu.Unlock()

	go func() {
		_ = chCmd.Wait()
		close(inst.done)
	}()

	// 6. Wait for API socket to appear
	if err := waitForSocket(inst.apiSocket, 10*time.Second); err != nil {
		vsockLn.Close()
		v.cleanupInstance(inst)
		return nil, fmt.Errorf("cloud-hypervisor API socket: %w", err)
	}

	client := newCHClient(inst.apiSocket)

	// 7. Create and boot VM
	ch, err := v.freshBoot(client, inst, cfg, vsockLn)
	if err != nil {
		vsockLn.Close()
		v.cleanupInstance(inst)
		return nil, fmt.Errorf("fresh boot: %w", err)
	}
	return ch, nil
}

func (v *CloudHypervisorVMM) freshBoot(client *chClient, inst *chInstance, cfg VMConfig, vsockLn net.Listener) (ControlChannel, error) {
	// Build kernel cmdline
	cmdlineParts := []string{
		"console=hvc0",
		"root=/dev/vda",
		"rw",
		"init=/usr/bin/aegis-harness",
		"AEGIS_VSOCK_PORT=" + strconv.Itoa(harnessVsockPort),
		"AEGIS_VSOCK_CID=2",
		fmt.Sprintf("AEGIS_NET_IP=%s/30", inst.guestIP),
		fmt.Sprintf("AEGIS_NET_GW=%s", inst.hostIP),
		"AEGIS_NET_DNS=8.8.8.8",
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"HOME=/root",
		"TERM=linux",
	}
	if cfg.WorkspacePath != "" {
		cmdlineParts = append(cmdlineParts, "AEGIS_WORKSPACE=1")
	}
	cmdline := strings.Join(cmdlineParts, " ")

	memBytes := int64(cfg.MemoryMB) * 1024 * 1024

	// Build vm.create payload
	createPayload := map[string]interface{}{
		"payload": map[string]interface{}{
			"kernel":  v.kernelPath,
			"cmdline": cmdline,
		},
		"cpus": map[string]interface{}{
			"boot_vcpus": cfg.VCPUs,
			"max_vcpus":  cfg.VCPUs,
		},
		"memory": map[string]interface{}{
			"size":   memBytes,
			"shared": true,
		},
		"disks": []map[string]interface{}{
			{"path": cfg.Rootfs.Path},
		},
		"net": []map[string]interface{}{
			{"tap": inst.tapName},
		},
		"vsock": map[string]interface{}{
			"cid":    3,
			"socket": inst.vsockSocket,
		},
	}

	// Add virtiofs if workspace configured
	if cfg.WorkspacePath != "" {
		createPayload["fs"] = []map[string]interface{}{
			{
				"tag":        "workspace",
				"socket":     inst.virtiofsdSocket,
				"num_queues": 1,
				"queue_size": 512,
			},
		}
	}

	// PUT /api/v1/vm.create
	resp, err := client.put("/api/v1/vm.create", createPayload)
	if err != nil {
		return nil, fmt.Errorf("vm.create: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("vm.create returned %d: %s", resp.StatusCode, body)
	}

	// PUT /api/v1/vm.boot
	resp, err = client.put("/api/v1/vm.boot", nil)
	if err != nil {
		return nil, fmt.Errorf("vm.boot: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("vm.boot returned %d: %s", resp.StatusCode, body)
	}

	// 8. Accept harness connection on vsock (90s timeout)
	return v.acceptHarness(vsockLn, 90*time.Second)
}

func (v *CloudHypervisorVMM) acceptHarness(ln net.Listener, timeout time.Duration) (ControlChannel, error) {
	if unixLn, ok := ln.(*net.UnixListener); ok {
		unixLn.SetDeadline(time.Now().Add(timeout))
	}
	conn, err := ln.Accept()
	ln.Close()
	if err != nil {
		return nil, fmt.Errorf("harness did not connect within %v: %w", timeout, err)
	}
	return NewNetControlChannel(conn), nil
}

func (v *CloudHypervisorVMM) PauseVM(h Handle) error {
	v.mu.Lock()
	inst, ok := v.instances[h.ID]
	v.mu.Unlock()
	if !ok {
		return fmt.Errorf("vm %s not found", h.ID)
	}

	client := newCHClient(inst.apiSocket)
	resp, err := client.put("/api/v1/vm.pause", nil)
	if err != nil {
		return fmt.Errorf("vm.pause: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("vm.pause returned %d: %s", resp.StatusCode, body)
	}
	return nil
}

func (v *CloudHypervisorVMM) ResumeVM(h Handle) error {
	v.mu.Lock()
	inst, ok := v.instances[h.ID]
	v.mu.Unlock()
	if !ok {
		return fmt.Errorf("vm %s not found", h.ID)
	}

	client := newCHClient(inst.apiSocket)
	resp, err := client.put("/api/v1/vm.resume", nil)
	if err != nil {
		return fmt.Errorf("vm.resume: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("vm.resume returned %d: %s", resp.StatusCode, body)
	}
	return nil
}

func (v *CloudHypervisorVMM) StopVM(h Handle) error {
	v.mu.Lock()
	inst, ok := v.instances[h.ID]
	if !ok {
		v.mu.Unlock()
		return fmt.Errorf("vm %s not found", h.ID)
	}
	v.mu.Unlock()

	v.cleanupInstance(inst)

	v.mu.Lock()
	delete(v.instances, h.ID)
	v.mu.Unlock()

	return nil
}

func (v *CloudHypervisorVMM) HostEndpoints(h Handle) ([]HostEndpoint, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	inst, ok := v.instances[h.ID]
	if !ok {
		return nil, fmt.Errorf("vm %s not found", h.ID)
	}
	eps := make([]HostEndpoint, len(inst.endpoints))
	copy(eps, inst.endpoints)
	return eps, nil
}

// DynamicExposePort registers a new port endpoint at runtime.
// With tap networking, the router dials the guest IP directly — no port
// forwarding setup needed, just add the endpoint so GetEndpoint finds it.
func (v *CloudHypervisorVMM) DynamicExposePort(h Handle, guestPort int) (int, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	inst, ok := v.instances[h.ID]
	if !ok {
		return 0, fmt.Errorf("vm %s not found", h.ID)
	}

	inst.endpoints = append(inst.endpoints, HostEndpoint{
		GuestPort:   guestPort,
		HostPort:    guestPort,
		Protocol:    "tcp",
		BackendAddr: inst.guestIP,
	})

	log.Printf("vmm: dynamic expose guest:%d (vm %s, guest %s)", guestPort, h.ID, inst.guestIP)
	return guestPort, nil
}

func (v *CloudHypervisorVMM) Capabilities() BackendCaps {
	return BackendCaps{
		Pause:           true,
		PersistentPause: false, // lifecycle manager starts stop-after-idle timer
		RootFSType:      RootFSBlockImage,
		Name:            "cloud-hypervisor",
		GuestArch:       runtime.GOARCH,
		NetworkBackend:  "tap",
	}
}

// --- Sidecar management ---

func (v *CloudHypervisorVMM) startVirtiofsd(inst *chInstance) error {
	os.Remove(inst.virtiofsdSocket) // clean stale

	cmd := exec.Command(v.virtiofsdBin,
		"--socket-path="+inst.virtiofsdSocket,
		"--shared-dir="+inst.config.WorkspacePath,
		"--cache=never",
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("spawn virtiofsd: %w", err)
	}

	inst.virtiofsdCmd = cmd

	// Wait for virtiofsd socket to appear
	if err := waitForSocket(inst.virtiofsdSocket, 10*time.Second); err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		return fmt.Errorf("virtiofsd socket not ready: %w", err)
	}

	log.Printf("vmm: virtiofsd started (socket=%s, shared=%s)", inst.virtiofsdSocket, inst.config.WorkspacePath)
	return nil
}

// cleanupInstance kills processes, destroys tap, removes NAT, cleans sockets.
func (v *CloudHypervisorVMM) cleanupInstance(inst *chInstance) {
	// Kill cloud-hypervisor
	if inst.chCmd != nil && inst.chCmd.Process != nil {
		inst.chCmd.Process.Kill()
		inst.chCmd.Wait()
	}

	// Kill virtiofsd
	if inst.virtiofsdCmd != nil && inst.virtiofsdCmd.Process != nil {
		inst.virtiofsdCmd.Process.Kill()
		inst.virtiofsdCmd.Wait()
	}

	// Remove NAT rules
	removeNAT(inst.tapName, inst.guestIP)

	// Destroy tap device
	destroyTap(inst.tapName)

	// Clean up socket files
	os.Remove(inst.apiSocket)
	os.Remove(inst.vsockSocket)
	os.Remove(fmt.Sprintf("%s_%d", inst.vsockSocket, harnessVsockPort))
	os.Remove(inst.virtiofsdSocket)
}

// --- Networking helpers ---

// cleanupOrphanedTaps removes any aegis* tap devices and their NAT rules
// left over from a previous daemon crash. Called once at startup.
func cleanupOrphanedTaps() {
	ifaces, err := net.Interfaces()
	if err != nil {
		return
	}
	for _, iface := range ifaces {
		if strings.HasPrefix(iface.Name, "aegis") {
			log.Printf("vmm: cleaning up orphaned tap %s", iface.Name)
			// Best-effort: remove NAT + FORWARD rules, then delete the tap.
			// Derive the guest IP from the tap index (same allocation scheme as CreateVM).
			var idx uint32
			fmt.Sscanf(iface.Name, "aegis%d", &idx)
			thirdOctet := idx / 64
			fourthBase := (idx % 64) * 4
			guestIP := fmt.Sprintf("172.16.%d.%d", thirdOctet, fourthBase+2)
			removeNAT(iface.Name, guestIP)
			destroyTap(iface.Name)
		}
	}
}

// enableIPForward enables IPv4 packet forwarding.
func enableIPForward() error {
	return os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1"), 0644)
}

// createTap creates a tap device, assigns an IP address, and brings it up.
func createTap(name, hostIP string) error {
	if err := runCmd("ip", "tuntap", "add", "dev", name, "mode", "tap"); err != nil {
		return fmt.Errorf("ip tuntap add: %w", err)
	}
	if err := runCmd("ip", "addr", "add", hostIP+"/30", "dev", name); err != nil {
		destroyTap(name)
		return fmt.Errorf("ip addr add: %w", err)
	}
	if err := runCmd("ip", "link", "set", name, "up"); err != nil {
		destroyTap(name)
		return fmt.Errorf("ip link set up: %w", err)
	}
	return nil
}

// destroyTap removes a tap device.
func destroyTap(name string) {
	runCmd("ip", "link", "del", name)
}

// setupNAT adds iptables MASQUERADE and FORWARD rules for guest egress.
func setupNAT(tapName, guestIP string) error {
	src := guestIP + "/30"
	// MASQUERADE for outbound
	if err := runCmd("iptables", "-t", "nat", "-A", "POSTROUTING", "-s", src, "-j", "MASQUERADE"); err != nil {
		return fmt.Errorf("iptables MASQUERADE: %w", err)
	}
	// FORWARD rules
	if err := runCmd("iptables", "-A", "FORWARD", "-i", tapName, "-j", "ACCEPT"); err != nil {
		return fmt.Errorf("iptables FORWARD in: %w", err)
	}
	if err := runCmd("iptables", "-A", "FORWARD", "-o", tapName, "-m", "state", "--state", "RELATED,ESTABLISHED", "-j", "ACCEPT"); err != nil {
		return fmt.Errorf("iptables FORWARD out: %w", err)
	}
	return nil
}

// removeNAT removes iptables rules for a guest. Best-effort — ignores errors.
func removeNAT(tapName, guestIP string) {
	src := guestIP + "/30"
	runCmd("iptables", "-t", "nat", "-D", "POSTROUTING", "-s", src, "-j", "MASQUERADE")
	runCmd("iptables", "-D", "FORWARD", "-i", tapName, "-j", "ACCEPT")
	runCmd("iptables", "-D", "FORWARD", "-o", tapName, "-m", "state", "--state", "RELATED,ESTABLISHED", "-j", "ACCEPT")
}

// --- Helpers ---

// waitForSocket polls until a unix socket file appears.
func waitForSocket(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("socket %s did not appear within %v", path, timeout)
}

// runCmd runs a command and returns an error if it fails.
func runCmd(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
