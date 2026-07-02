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
