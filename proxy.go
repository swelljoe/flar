package main

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
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
	if err == nil {
		// Resolve IPs to check for loopback/private access
		ips, err := net.LookupIP(hostname)
		if err == nil {
			isLoopback := false
			for _, ip := range ips {
				if ip.IsLoopback() {
					isLoopback = true
					break
				}
			}

			if isLoopback {
				var port int
				_, _ = fmt.Sscan(portStr, &port)
				allowed := false
				for _, ap := range p.allowPorts {
					if ap == port {
						allowed = true
						break
					}
				}
				if !allowed {
					http.Error(w, fmt.Sprintf("Access to localhost port %s is blocked inside the sandbox", portStr), http.StatusForbidden)
					return
				}
			}
		}
	}

	if req.Method == http.MethodConnect {
		// HTTPS Tunnel
		destConn, err := net.Dial("tcp", host)
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
	client := &http.Client{}
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
