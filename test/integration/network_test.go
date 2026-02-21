//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// Network tests: verify egress (VM→internet), ingress (client→VM),
// and large payloads that would fail under TSI's ~32KB limit.

// TestNetwork_BackendInStatus verifies the network_backend field is exposed
// in the /v1/status API response.
func TestNetwork_BackendInStatus(t *testing.T) {
	status := apiGet(t, "/v1/status")
	caps, ok := status["capabilities"].(map[string]interface{})
	if !ok {
		t.Fatalf("capabilities not found in status: %v", status)
	}
	backend, ok := caps["network_backend"].(string)
	if !ok || backend == "" {
		t.Fatalf("network_backend not in capabilities: %v", caps)
	}
	if backend != "gvproxy" && backend != "tsi" {
		t.Fatalf("unexpected network_backend: %q (want gvproxy or tsi)", backend)
	}
	t.Logf("network backend: %s", backend)
}

// TestNetwork_DNSResolution verifies DNS works from inside the VM.
// In gvproxy mode, DNS is served by gvproxy at the gateway (192.168.127.1).
// In TSI mode, DNS goes through 8.8.8.8 via TSI.
func TestNetwork_DNSResolution(t *testing.T) {
	// Use nslookup from busybox to test DNS resolution
	out := aegisRun(t, "run", "--", "sh", "-c",
		"nslookup example.com >/dev/null 2>&1 && echo DNS_OK || echo DNS_FAIL")
	if !strings.Contains(out, "DNS_OK") {
		t.Fatalf("DNS resolution failed inside VM: %s", out)
	}
}

// TestNetwork_EgressHTTP verifies the VM can make outbound HTTP requests.
func TestNetwork_EgressHTTP(t *testing.T) {
	out := aegisRun(t, "run", "--", "sh", "-c",
		"wget -q -O /dev/null -T 10 http://example.com && echo HTTP_OK || echo HTTP_FAIL")
	if !strings.Contains(out, "HTTP_OK") {
		t.Fatalf("outbound HTTP failed: %s", out)
	}
}

// TestNetwork_EgressHTTPS verifies the VM can make outbound HTTPS requests.
// This validates TLS through the network stack (gvproxy or TSI).
func TestNetwork_EgressHTTPS(t *testing.T) {
	out := aegisRun(t, "run", "--", "sh", "-c",
		"wget -q -O /dev/null -T 10 https://example.com && echo HTTPS_OK || echo HTTPS_FAIL")
	if !strings.Contains(out, "HTTPS_OK") {
		t.Fatalf("outbound HTTPS failed: %s", out)
	}
}

// TestNetwork_EgressLargePost verifies outbound POST with a 50KB body.
// This is the key test — TSI fails at ~32KB, gvproxy should handle it fine.
func TestNetwork_EgressLargePost(t *testing.T) {
	// httpbin.org/post echoes the body back, but we just need a 200.
	// Use a simple python script to send a large POST and report the status.
	// The body is 50000 bytes of 'A', well above the TSI ~32KB limit.
	script := `
import urllib.request
import sys

body = b'A' * 50000
req = urllib.request.Request(
    'https://httpbin.org/post',
    data=body,
    method='POST',
    headers={'Content-Type': 'application/octet-stream'}
)
try:
    resp = urllib.request.urlopen(req, timeout=30)
    print(f'POST_STATUS={resp.status}')
except Exception as e:
    print(f'POST_FAIL={e}')
    sys.exit(1)
`
	out := aegisRun(t, "run", "--", "python3", "-c", script)
	if !strings.Contains(out, "POST_STATUS=200") {
		t.Fatalf("50KB POST failed (TSI limit?): %s", out)
	}
	t.Log("50KB outbound POST succeeded")
}

// TestNetwork_EgressLargePost100KB tests an even larger payload (100KB).
func TestNetwork_EgressLargePost100KB(t *testing.T) {
	script := `
import urllib.request
import sys

body = b'X' * 100000
req = urllib.request.Request(
    'https://httpbin.org/post',
    data=body,
    method='POST',
    headers={'Content-Type': 'application/octet-stream'}
)
try:
    resp = urllib.request.urlopen(req, timeout=30)
    print(f'POST_STATUS={resp.status}')
except Exception as e:
    print(f'POST_FAIL={e}')
    sys.exit(1)
`
	out := aegisRun(t, "run", "--", "python3", "-c", script)
	if !strings.Contains(out, "POST_STATUS=200") {
		t.Fatalf("100KB POST failed: %s", out)
	}
	t.Log("100KB outbound POST succeeded")
}

// TestNetwork_IngressSmallPayload verifies a small HTTP request through the
// router to a guest server and back.
func TestNetwork_IngressSmallPayload(t *testing.T) {
	const (
		publicPort = 8182
		guestPort  = 8080
		handle     = "net-ingress-small"
	)

	apiDeleteAllowFail(t, fmt.Sprintf("/v1/instances/%s", handle))
	time.Sleep(500 * time.Millisecond)

	// Start a Python HTTP echo server that returns POST body length
	script := `
import http.server
import json

class Handler(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        length = int(self.headers.get('Content-Length', 0))
        body = self.rfile.read(length)
        self.send_response(200)
        self.send_header('Content-Type', 'application/json')
        self.end_headers()
        resp = json.dumps({"received_bytes": len(body)})
        self.wfile.write(resp.encode())
    def do_GET(self):
        self.send_response(200)
        self.end_headers()
        self.wfile.write(b'ok')
    def log_message(self, fmt, *args):
        pass  # suppress logs

http.server.HTTPServer(('0.0.0.0', 8080), Handler).serve_forever()
`

	inst := apiPost(t, "/v1/instances", map[string]interface{}{
		"command": []string{"python3", "-c", script},
		"handle":  handle,
		"exposes": []map[string]interface{}{
			{"port": guestPort, "public_port": publicPort},
		},
	})
	id := inst["id"].(string)
	t.Cleanup(func() { apiDeleteAllowFail(t, "/v1/instances/"+id) })

	url := fmt.Sprintf("http://127.0.0.1:%d/", publicPort)
	_, err := waitForHTTP(url, 60*time.Second)
	if err != nil {
		t.Fatalf("server not ready: %v", err)
	}

	// Send a small POST (1KB)
	body := bytes.Repeat([]byte("S"), 1024)
	resp, err := http.Post(url, "application/octet-stream", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("POST returned %d", resp.StatusCode)
	}

	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)
	received := int(result["received_bytes"].(float64))
	if received != 1024 {
		t.Fatalf("server received %d bytes, want 1024", received)
	}
	t.Log("1KB ingress POST: OK")
}

// TestNetwork_IngressLargePayload verifies a 50KB HTTP request body can be
// received by a guest server through the router. This tests ingress path with
// a payload larger than the TSI egress limit (ingress was always fine, but
// verifies gvproxy handles it correctly in both directions).
func TestNetwork_IngressLargePayload(t *testing.T) {
	const (
		publicPort = 8183
		guestPort  = 8080
		handle     = "net-ingress-large"
		payloadLen = 50000
	)

	apiDeleteAllowFail(t, fmt.Sprintf("/v1/instances/%s", handle))
	time.Sleep(500 * time.Millisecond)

	script := `
import http.server
import json

class Handler(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        length = int(self.headers.get('Content-Length', 0))
        body = self.rfile.read(length)
        self.send_response(200)
        self.send_header('Content-Type', 'application/json')
        self.end_headers()
        resp = json.dumps({"received_bytes": len(body)})
        self.wfile.write(resp.encode())
    def do_GET(self):
        self.send_response(200)
        self.end_headers()
        self.wfile.write(b'ok')
    def log_message(self, fmt, *args):
        pass

http.server.HTTPServer(('0.0.0.0', 8080), Handler).serve_forever()
`

	inst := apiPost(t, "/v1/instances", map[string]interface{}{
		"command": []string{"python3", "-c", script},
		"handle":  handle,
		"exposes": []map[string]interface{}{
			{"port": guestPort, "public_port": publicPort},
		},
	})
	id := inst["id"].(string)
	t.Cleanup(func() { apiDeleteAllowFail(t, "/v1/instances/"+id) })

	url := fmt.Sprintf("http://127.0.0.1:%d/", publicPort)
	_, err := waitForHTTP(url, 60*time.Second)
	if err != nil {
		t.Fatalf("server not ready: %v", err)
	}

	// Send a 50KB POST
	body := bytes.Repeat([]byte("L"), payloadLen)
	resp, err := http.Post(url, "application/octet-stream", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("50KB POST failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		data, _ := io.ReadAll(resp.Body)
		t.Fatalf("50KB POST returned %d: %s", resp.StatusCode, data)
	}

	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)
	received := int(result["received_bytes"].(float64))
	if received != payloadLen {
		t.Fatalf("server received %d bytes, want %d", received, payloadLen)
	}
	t.Logf("50KB ingress POST: OK (received %d bytes)", received)
}

// TestNetwork_EgressLargeResponse verifies the VM can receive a large HTTP
// response body (the download direction).
func TestNetwork_EgressLargeResponse(t *testing.T) {
	script := `
import urllib.request
import sys

# httpbin.org/bytes/{n} returns n random bytes
resp = urllib.request.urlopen('https://httpbin.org/bytes/65536', timeout=30)
data = resp.read()
print(f'RECEIVED_BYTES={len(data)}')
`
	out := aegisRun(t, "run", "--", "python3", "-c", script)
	if !strings.Contains(out, "RECEIVED_BYTES=65536") {
		t.Fatalf("large response download failed: %s", out)
	}
	t.Log("64KB download: OK")
}

// TestNetwork_ConcurrentEgressPosts sends 5 parallel 40KB POST requests from
// inside a VM. This catches hidden buffering limits that may not manifest
// with a single request.
func TestNetwork_ConcurrentEgressPosts(t *testing.T) {
	// Use a Python script that sends concurrent POST requests via threads
	script := `
import urllib.request
import threading
import sys

results = []
lock = threading.Lock()

def post_large(idx):
    body = bytes([idx % 256]) * 40000
    req = urllib.request.Request(
        'https://httpbin.org/post',
        data=body,
        method='POST',
        headers={'Content-Type': 'application/octet-stream'}
    )
    try:
        resp = urllib.request.urlopen(req, timeout=60)
        with lock:
            results.append(('ok', resp.status))
    except Exception as e:
        with lock:
            results.append(('fail', str(e)))

threads = []
for i in range(5):
    t = threading.Thread(target=post_large, args=(i,))
    threads.append(t)
    t.start()

for t in threads:
    t.join(timeout=90)

ok_count = sum(1 for r in results if r[0] == 'ok')
fail_count = sum(1 for r in results if r[0] == 'fail')
print(f'CONCURRENT_OK={ok_count} FAIL={fail_count}')
if fail_count > 0:
    for r in results:
        if r[0] == 'fail':
            print(f'  DETAIL: {r[1]}')
`
	out := aegisRun(t, "run", "--", "python3", "-c", script)
	if !strings.Contains(out, "CONCURRENT_OK=5 FAIL=0") {
		t.Fatalf("concurrent 40KB POSTs failed: %s", out)
	}
	t.Log("5x 40KB concurrent outbound POSTs: OK")
}

// TestNetwork_IngressConcurrent sends multiple simultaneous requests to a
// guest server through the router to verify gvproxy handles concurrent
// ingress connections.
func TestNetwork_IngressConcurrent(t *testing.T) {
	const (
		publicPort = 8184
		guestPort  = 8080
		handle     = "net-ingress-conc"
		numClients = 10
	)

	apiDeleteAllowFail(t, fmt.Sprintf("/v1/instances/%s", handle))
	time.Sleep(500 * time.Millisecond)

	script := `
import http.server
import json

class Handler(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        length = int(self.headers.get('Content-Length', 0))
        body = self.rfile.read(length)
        self.send_response(200)
        self.send_header('Content-Type', 'application/json')
        self.end_headers()
        resp = json.dumps({"received_bytes": len(body)})
        self.wfile.write(resp.encode())
    def do_GET(self):
        self.send_response(200)
        self.end_headers()
        self.wfile.write(b'ok')
    def log_message(self, fmt, *args):
        pass

http.server.HTTPServer(('0.0.0.0', 8080), Handler).serve_forever()
`

	inst := apiPost(t, "/v1/instances", map[string]interface{}{
		"command": []string{"python3", "-c", script},
		"handle":  handle,
		"exposes": []map[string]interface{}{
			{"port": guestPort, "public_port": publicPort},
		},
	})
	id := inst["id"].(string)
	t.Cleanup(func() { apiDeleteAllowFail(t, "/v1/instances/"+id) })

	url := fmt.Sprintf("http://127.0.0.1:%d/", publicPort)
	_, err := waitForHTTP(url, 60*time.Second)
	if err != nil {
		t.Fatalf("server not ready: %v", err)
	}

	// Send concurrent requests
	var wg sync.WaitGroup
	errors := make(chan string, numClients)

	for i := 0; i < numClients; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			payload := bytes.Repeat([]byte{byte('A' + idx%26)}, 5000)
			resp, err := http.Post(url, "application/octet-stream", bytes.NewReader(payload))
			if err != nil {
				errors <- fmt.Sprintf("client %d: %v", idx, err)
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode != 200 {
				errors <- fmt.Sprintf("client %d: status %d", idx, resp.StatusCode)
				return
			}
			var result map[string]interface{}
			json.NewDecoder(resp.Body).Decode(&result)
			received := int(result["received_bytes"].(float64))
			if received != 5000 {
				errors <- fmt.Sprintf("client %d: received %d, want 5000", idx, received)
			}
		}(i)
	}

	wg.Wait()
	close(errors)

	var errs []string
	for e := range errors {
		errs = append(errs, e)
	}
	if len(errs) > 0 {
		t.Fatalf("concurrent ingress failed:\n%s", strings.Join(errs, "\n"))
	}
	t.Logf("%d concurrent ingress POSTs: OK", numClients)
}
