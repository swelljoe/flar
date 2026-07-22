package main

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestHttpProxy(t *testing.T) {
	// 1. Start a mock upstream server representing the target
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("mock response"))
	}))
	defer upstream.Close()

	// Create temp directory for Unix sockets
	tmpDir, err := os.MkdirTemp("", "flar-proxy-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	sockPath := filepath.Join(tmpDir, "http.sock")

	// Start host proxy allowing port 8080 (but not upstream's port)
	allowPorts := []int{8080}
	proxy, err := StartHttpProxy(sockPath, allowPorts)
	if err != nil {
		t.Fatalf("failed to start HTTP proxy: %v", err)
	}
	defer proxy.Close()

	// Dial the Unix socket and use it as a client to test HTTP requests through the proxy
	client := &http.Client{
		Transport: &http.Transport{
			Dial: func(network, addr string) (net.Conn, error) {
				return net.Dial("unix", sockPath)
			},
		},
	}

	// Case A: Verify proxy blocks localhost when port is not allowed
	req, err := http.NewRequest("GET", upstream.URL, nil)
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request through proxy failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("expected StatusForbidden (403) for blocked localhost upstream, got %d", resp.StatusCode)
	}

	// Case B: Add upstream port to allowPorts and restart HTTP proxy to verify it allows it
	proxy.Close()
	_, upstreamPortStr, err := net.SplitHostPort(upstream.Listener.Addr().String())
	if err != nil {
		t.Fatalf("failed to parse upstream address: %v", err)
	}
	var upstreamPort int
	_, _ = fmt.Sscan(upstreamPortStr, &upstreamPort)

	proxy, err = StartHttpProxy(sockPath, []int{upstreamPort})
	if err != nil {
		t.Fatalf("failed to restart HTTP proxy: %v", err)
	}

	resp2, err := client.Do(req)
	if err != nil {
		t.Fatalf("request through proxy failed: %v", err)
	}
	defer resp2.Body.Close()

	if resp2.StatusCode != http.StatusOK {
		t.Errorf("expected StatusOK (200) for allowed localhost upstream, got %d", resp2.StatusCode)
	}
	body, _ := io.ReadAll(resp2.Body)
	if string(body) != "mock response" {
		t.Errorf("expected body 'mock response', got %q", body)
	}
}

// TestResolveAndCheck verifies that the HTTP proxy refuses destinations in
// loopback, private, link-local (cloud metadata!), multicast, and unspecified
// ranges unless the port is explicitly allowlisted, and that it returns the
// validated IP to dial rather than the hostname (no DNS re-resolution).
func TestResolveAndCheck(t *testing.T) {
	proxy := &HttpProxy{allowPorts: []int{11434}}

	blockedHosts := []string{
		"localhost",
		"127.0.0.1",
		"::1",
		"169.254.169.254", // cloud instance metadata endpoint
		"10.0.0.5",        // RFC 1918
		"172.16.0.1",      // RFC 1918
		"192.168.1.1",     // RFC 1918
		"100.64.0.1",      // CGNAT
		"224.0.0.1",       // multicast
		"0.0.0.0",         // unspecified
	}
	for _, host := range blockedHosts {
		if _, err := proxy.resolveAndCheck(host, "80"); err == nil {
			t.Errorf("resolveAndCheck(%q, 80) allowed a local/private destination", host)
		}
	}

	// An allowlisted port opens local destinations...
	addr, err := proxy.resolveAndCheck("127.0.0.1", "11434")
	if err != nil {
		t.Errorf("resolveAndCheck(127.0.0.1, 11434) blocked an allowlisted port: %v", err)
	}
	// ...and the returned address carries the validated IP, not the hostname.
	if addr != "127.0.0.1:11434" {
		t.Errorf("resolveAndCheck returned %q, want the validated address 127.0.0.1:11434", addr)
	}

	// Ports outside the valid range are rejected outright.
	for _, bad := range []string{"0", "65536", "notaport"} {
		if _, err := proxy.resolveAndCheck("127.0.0.1", bad); err == nil {
			t.Errorf("resolveAndCheck(127.0.0.1, %q) accepted an invalid port", bad)
		}
	}

	// Unresolvable hosts are refused rather than dialed.
	if _, err := proxy.resolveAndCheck("nonexistent.invalid", "443"); err == nil {
		t.Errorf("resolveAndCheck(nonexistent.invalid, 443) did not fail")
	}
}

// TestHttpProxyDoesNotFollowRedirects verifies that a 3xx is passed back to
// the client instead of being followed inside the proxy. Following redirects
// here would let a public URL bounce the agent to a loopback/private target
// without going through resolveAndCheck.
func TestHttpProxyDoesNotFollowRedirects(t *testing.T) {
	// The redirect target is a second local server; if the proxy followed the
	// redirect it would have to dial this loopback address, which the port
	// allowance below permits — so a followed redirect would succeed and a
	// passed-through one returns the 302 itself.
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("followed redirect"))
	}))
	defer target.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer redirector.Close()

	_, portStr, err := net.SplitHostPort(redirector.Listener.Addr().String())
	if err != nil {
		t.Fatalf("failed to parse redirector address: %v", err)
	}
	var port int
	if _, err := fmt.Sscan(portStr, &port); err != nil {
		t.Fatalf("failed to parse redirector port: %v", err)
	}

	tmpDir, err := os.MkdirTemp("", "flar-proxy-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Allow BOTH local ports so the only thing stopping a followed redirect is
	// the no-follow policy, not the address filter.
	_, targetPortStr, _ := net.SplitHostPort(target.Listener.Addr().String())
	var targetPort int
	_, _ = fmt.Sscan(targetPortStr, &targetPort)

	proxy, err := StartHttpProxy(filepath.Join(tmpDir, "http.sock"), []int{port, targetPort})
	if err != nil {
		t.Fatalf("failed to start HTTP proxy: %v", err)
	}
	defer proxy.Close()

	client := &http.Client{
		Transport: &http.Transport{
			Dial: func(network, addr string) (net.Conn, error) {
				return net.Dial("unix", filepath.Join(tmpDir, "http.sock"))
			},
		},
		// The test client must not follow redirects either, or it masks what
		// the proxy did.
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	resp, err := client.Get(redirector.URL)
	if err != nil {
		t.Fatalf("request through proxy failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusFound {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("expected the 302 to be passed through, got status %d body %q (proxy followed the redirect)", resp.StatusCode, body)
	}
	if loc := resp.Header.Get("Location"); loc != target.URL {
		t.Errorf("expected Location %q, got %q", target.URL, loc)
	}
}

func TestTCPPortProxy(t *testing.T) {
	// 1. Start a local TCP listener to represent the local service
	localListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start local TCP listener: %v", err)
	}
	defer localListener.Close()

	_, portStr, _ := net.SplitHostPort(localListener.Addr().String())
	var localPort int
	_, _ = fmt.Sscan(portStr, &localPort)

	// Start a goroutine to accept and echo data from the TCP listener
	go func() {
		conn, err := localListener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 1024)
		n, err := conn.Read(buf)
		if err != nil {
			return
		}
		_, _ = conn.Write(buf[:n])
	}()

	// Create temp directory for Unix socket
	tmpDir, err := os.MkdirTemp("", "flar-tcp-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	sockPath := filepath.Join(tmpDir, "tcp.sock")

	// Start TCP port proxy pointing to localPort
	proxy, err := StartHostProxy(localPort, sockPath)
	if err != nil {
		t.Fatalf("failed to start host TCP proxy: %v", err)
	}
	defer proxy.Close()

	// Dial the unix socket
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("failed to dial unix socket: %v", err)
	}
	defer conn.Close()

	// Send message
	msg := "hello proxy"
	if _, err := conn.Write([]byte(msg)); err != nil {
		t.Fatalf("failed to write to proxy: %v", err)
	}

	// Read response
	buf := make([]byte, 1024)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("failed to read from proxy: %v", err)
	}

	if string(buf[:n]) != msg {
		t.Errorf("expected echo %q, got %q", msg, buf[:n])
	}
}
