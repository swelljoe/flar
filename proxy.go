package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
)

// PortProxy handles forwarding from a Unix socket to a local host TCP port.
type PortProxy struct {
	Port       int
	SocketPath string
	listener   net.Listener
	wg         sync.WaitGroup
	closeChan  chan struct{}
}

// StartHostProxy starts a host TCP proxy listening on a Unix domain socket.
func StartHostProxy(port int, socketPath string) (*PortProxy, error) {
	_ = os.Remove(socketPath)

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, err
	}

	proxy := &PortProxy{
		Port:       port,
		SocketPath: socketPath,
		listener:   listener,
		closeChan:  make(chan struct{}),
	}

	proxy.wg.Add(1)
	go func() {
		defer proxy.wg.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				select {
				case <-proxy.closeChan:
					return
				default:
					continue
				}
			}
			go proxy.handleConnection(conn)
		}
	}()

	return proxy, nil
}

func (p *PortProxy) handleConnection(unixConn net.Conn) {
	defer unixConn.Close()

	tcpConn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", p.Port))
	if err != nil {
		return
	}
	defer tcpConn.Close()

	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(unixConn, tcpConn)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(tcpConn, unixConn)
		done <- struct{}{}
	}()
	<-done
}

// Close stops the proxy listener and cleans up the Unix socket.
func (p *PortProxy) Close() {
	close(p.closeChan)
	_ = p.listener.Close()
	p.wg.Wait()
	_ = os.Remove(p.SocketPath)
}

// HttpProxy handles HTTP/HTTPS proxying on the host from a Unix socket.
type HttpProxy struct {
	SocketPath string
	listener   net.Listener
	server     *http.Server
	wg         sync.WaitGroup
	allowPorts []int
}

// StartHttpProxy starts a host HTTP proxy listening on a Unix domain socket.
func StartHttpProxy(socketPath string, allowPorts []int) (*HttpProxy, error) {
	_ = os.Remove(socketPath)

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, err
	}

	proxy := &HttpProxy{
		SocketPath: socketPath,
		listener:   listener,
		allowPorts: allowPorts,
	}

	proxy.server = &http.Server{
		Handler: http.HandlerFunc(proxy.handleProxy),
	}

	proxy.wg.Add(1)
	go func() {
		defer proxy.wg.Done()
		_ = proxy.server.Serve(listener)
	}()

	return proxy, nil
}

func (p *HttpProxy) handleProxy(w http.ResponseWriter, req *http.Request) {
	// Parse the host and port from request
	host := req.Host
	if !strings.Contains(host, ":") {
		if req.URL.Scheme == "https" {
			host = host + ":443"
		} else {
			host = host + ":80"
		}
	}

	hostname, portStr, err := net.SplitHostPort(host)
	if err != nil {
		http.Error(w, fmt.Sprintf("invalid destination host %q", req.Host), http.StatusBadRequest)
		return
	}

	// Resolve and validate the destination once, then dial the validated
	// address below — never the hostname again.
	dialAddr, err := p.resolveAndCheck(hostname, portStr)
	if err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}

	if req.Method == http.MethodConnect {
		// HTTPS Tunnel
		destConn, err := net.Dial("tcp", dialAddr)
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		defer destConn.Close()

		w.WriteHeader(http.StatusOK)
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "Hijacking not supported", http.StatusInternalServerError)
			return
		}
		clientConn, _, err := hijacker.Hijack()
		if err != nil {
			return
		}
		defer clientConn.Close()

		done := make(chan struct{}, 2)
		go func() {
			_, _ = io.Copy(destConn, clientConn)
			done <- struct{}{}
		}()
		go func() {
			_, _ = io.Copy(clientConn, destConn)
			done <- struct{}{}
		}()
		<-done
		return
	}

	// HTTP request
	if req.URL.Scheme == "" {
		req.URL.Scheme = "http"
	}
	if req.URL.Host == "" {
		req.URL.Host = host
	}
	req.RequestURI = ""

	// Redirects are deliberately NOT followed inside the proxy: a 3xx is
	// returned to the agent, which re-requests the new Location through this
	// proxy, where the target is validated by resolveAndCheck like any other
	// request. Following redirects here would let a public URL bounce the
	// agent straight to a blocked loopback/private address unchecked.
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, dialAddr)
		},
		DisableKeepAlives: true,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// resolveAndCheck resolves hostname and refuses the request when any of its
// addresses is loopback, private, link-local, multicast, or unspecified —
// unless the destination port is explicitly allowlisted (allowPorts exists so
// the user can reach local services such as a database or Ollama). It returns
// the validated IP:port to dial: dialing the checked address instead of
// re-resolving the hostname closes the DNS-rebinding window between check and
// connect.
func (p *HttpProxy) resolveAndCheck(hostname, portStr string) (string, error) {
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return "", fmt.Errorf("invalid destination port %q", portStr)
	}
	ips, err := net.LookupIP(hostname)
	if err != nil {
		return "", fmt.Errorf("cannot resolve %q: %v", hostname, err)
	}
	if len(ips) == 0 {
		return "", fmt.Errorf("cannot resolve %q: no addresses", hostname)
	}
	blocked := false
	for _, ip := range ips {
		if isLocalOrPrivateIP(ip) {
			blocked = true
			break
		}
	}
	if blocked && !p.portAllowed(port) {
		return "", fmt.Errorf("access to local/private address %s port %d is blocked inside the sandbox", hostname, port)
	}
	return net.JoinHostPort(ips[0].String(), portStr), nil
}

// cgnatRange is 100.64.0.0/10 (RFC 6598 shared address space). net.IP's
// IsPrivate covers only RFC 1918/RFC 4193, so CGNAT — commonly used for
// internal ISP and cloud networks — must be blocked explicitly.
var cgnatRange = &net.IPNet{IP: net.ParseIP("100.64.0.0"), Mask: net.CIDRMask(10, 32)}

// isLocalOrPrivateIP reports whether ip is in any range that must not be
// reachable from the sandbox without an explicit port allowance: loopback,
// RFC 1918 / RFC 4193 / CGNAT private, link-local (which covers the
// 169.254.169.254 cloud metadata endpoint), multicast, and unspecified
// addresses.
func isLocalOrPrivateIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() ||
		cgnatRange.Contains(ip)
}

// portAllowed reports whether the user explicitly allowlisted this local port.
func (p *HttpProxy) portAllowed(port int) bool {
	for _, ap := range p.allowPorts {
		if ap == port {
			return true
		}
	}
	return false
}

// Close stops the HTTP proxy server and cleans up the Unix socket.
func (p *HttpProxy) Close() {
	_ = p.server.Close()
	_ = p.listener.Close()
	p.wg.Wait()
	_ = os.Remove(p.SocketPath)
}

// RunSandboxProxy runs the proxy inside the sandbox.
// Binds TCP 127.0.0.1:port and routes to the Unix socket.
func RunSandboxProxy(port int, socketPath string) {
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Proxy listener failed to bind to 127.0.0.1:%d: %v\n", port, err)
		os.Exit(1)
	}
	defer listener.Close()

	for {
		tcpConn, err := listener.Accept()
		if err != nil {
			continue
		}
		go func(conn net.Conn) {
			defer conn.Close()
			unixConn, err := net.Dial("unix", socketPath)
			if err != nil {
				return
			}
			defer unixConn.Close()

			done := make(chan struct{}, 2)
			go func() {
				_, _ = io.Copy(conn, unixConn)
				done <- struct{}{}
			}()
			go func() {
				_, _ = io.Copy(unixConn, conn)
				done <- struct{}{}
			}()
			<-done
		}(tcpConn)
	}
}
