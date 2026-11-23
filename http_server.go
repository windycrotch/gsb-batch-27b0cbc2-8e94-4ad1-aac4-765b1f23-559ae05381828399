// HTTP Server

package main

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	tls_certificate_loader "github.com/AgustinSRG/go-tls-certificate-loader"
)

// Size limit for WebSocket messages (64 kb)
const WS_MSG_SIZE_LIMIT = 64 * 1024

// Generates unique ID for each request
func (node *WebRTC_CDN_Node) getRequestID() uint64 {
	node.mutexReqCount.Lock()
	defer node.mutexReqCount.Unlock()

	node.reqCount++

	return node.reqCount
}

// Adds IP address to the list
// Returns false if the limit has been reached
func (node *WebRTC_CDN_Node) AddIP(ip string) bool {
	node.mutexIpCount.Lock()
	defer node.mutexIpCount.Unlock()

	c := node.ipCount[ip]

	if c >= node.ipLimit {
		return false
	}

	node.ipCount[ip] = c + 1

	return true
}

// Checks if an IP is exempted from the limit
func (node *WebRTC_CDN_Node) isIPExempted(ipStr string) bool {
	r := os.Getenv("CONCURRENT_LIMIT_WHITELIST")

	if r == "" {
		return false
	}

	if r == "*" {
		return true
	}

	ip := net.ParseIP(ipStr)

	parts := strings.Split(r, ",")

	for i := 0; i < len(parts); i++ {
		_, rang, e := net.ParseCIDR(parts[i])

		if e != nil {
			LogError(e)
			continue
		}

		if rang.Contains(ip) {
			return true
		}
	}

	return false
}

// Removes an IP from the list.
// Idempotent and underflow-safe: if the IP has no recorded slot (it was never
// counted, or the slot was already released), this is a no-op, so a duplicate
// close can never drive the counter negative or delete another connection's
// quota.
func (node *WebRTC_CDN_Node) RemoveIP(ip string) {
	node.mutexIpCount.Lock()
	defer node.mutexIpCount.Unlock()

	c := node.ipCount[ip]

	if c == 0 {
		return // Nothing to release
	}

	if c <= 1 {
		delete(node.ipCount, ip)
	} else {
		node.ipCount[ip] = c - 1
	}
}

// Runs secure HTTPs server
func (node *WebRTC_CDN_Node) runHTTPSecureServer(wg *sync.WaitGroup) {
	defer func() {
		wg.Done()
	}()

	bind_addr := os.Getenv("BIND_ADDRESS")

	// Setup HTTPS server
	var port int
	port = 443
	customSSLPort := os.Getenv("SSL_PORT")
	if customSSLPort != "" {
		sslp, e := strconv.Atoi(customSSLPort)
		if e == nil {
			port = sslp
		}
	}

	certFile := os.Getenv("SSL_CERT")
	keyFile := os.Getenv("SSL_KEY")

	if certFile == "" || keyFile == "" {
		return
	}

	var sslReloadSeconds = 60
	customSslReloadSeconds := os.Getenv("SSL_CHECK_RELOAD_SECONDS")
	if customSslReloadSeconds != "" {
		n, e := strconv.Atoi(customSslReloadSeconds)
		if e == nil {
			sslReloadSeconds = n
		}
	}

	certificateLoader, err := tls_certificate_loader.NewTlsCertificateLoader(tls_certificate_loader.TlsCertificateLoaderConfig{
		CertificatePath:   certFile,
		KeyPath:           keyFile,
		CheckReloadPeriod: time.Duration(sslReloadSeconds) * time.Second,
		OnReload: func() {
			LogInfo("Reloaded SSL certificates")
		},
		OnError: func(err error) {
			LogError(err)
		},
	})

	if err != nil {
		LogError(err)
		return
	}

	defer certificateLoader.Close()

	// Setup HTTPS server

	tlsServer := http.Server{
		Addr:    bind_addr + ":" + strconv.Itoa(port),
		Handler: node,
		TLSConfig: &tls.Config{
			GetCertificate: certificateLoader.GetCertificate,
		},
	}

	// Listen

	LogInfo("[SSL] Listening on " + bind_addr + ":" + strconv.Itoa(port))

	errSSL := tlsServer.ListenAndServeTLS("", "")

	if errSSL != nil {
		LogError(errSSL)
	}
}

// Runs HTTP server
func (node *WebRTC_CDN_Node) runHTTPServer(wg *sync.WaitGroup) {
	defer func() {
		wg.Done()
	}()

	bind_addr := os.Getenv("BIND_ADDRESS")

	// Setup HTTP server
	var tcp_port int
	tcp_port = 80
	customTCPPort := os.Getenv("HTTP_PORT")
	if customTCPPort != "" {
		tcpp, e := strconv.Atoi(customTCPPort)
		if e == nil {
			tcp_port = tcpp
		}
	}

	// Listen
	LogInfo("[HTTP] Listening on " + bind_addr + ":" + strconv.Itoa(tcp_port))
	errHTTP := http.ListenAndServe(bind_addr+":"+strconv.Itoa(tcp_port), node)

	if errHTTP != nil {
		LogError(errHTTP)
	}
}

// Handles HTTP/HTTPS requests from clients
func (node *WebRTC_CDN_Node) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	reqId := node.getRequestID()

	ip, remotePort, err := net.SplitHostPort(req.RemoteAddr)

	if err != nil {
		LogError(err)
		w.WriteHeader(200)
		fmt.Fprintf(w, "WebRTC-CDN Signaling Server. Connect to /ws for signaling\nGo to /test for testing.")
		return
	}

	LogRequest(reqId, ip, ""+req.Method+" "+req.RequestURI)

	if req.URL.Path == "/ws" {
		// Websocket signaling connection
		ipCounted := false
		if !node.isIPExempted(ip) {
			if !node.AddIP(ip) {
				w.WriteHeader(429)
				fmt.Fprintf(w, "Too many requests.")
				LogRequest(reqId, ip, "Connection rejected: Too many requests")
				return
			}
			ipCounted = true
		}

		c, err := node.upgrader.Upgrade(w, req, nil)
		if err != nil {
			LogError(err)
			// The upgrade failed before any handler took ownership of the
			// slot: release it here so the quota does not leak.
			if ipCounted {
				node.RemoveIP(ip)
			}
			return
		}

		c.SetReadLimit(WS_MSG_SIZE_LIMIT)

		handler := Connection_Handler{
			id:         reqId,
			ip:         ip,
			port:       remotePort,
			node:       node,
			connection: c,
			ipCounted:  ipCounted,
		}

		handler.init()

		node.mutexConnections.Lock()

		node.connections[reqId] = &handler

		node.mutexConnections.Unlock()

		go handler.run()
	} else {
		w.WriteHeader(200)
		fmt.Fprintf(w, "WebRTC-CDN Signaling Server. Connect to /ws for signaling")
	}
}

// onConnectionClose unregisters the connection and releases its per-IP
// concurrency slot. It is idempotent: only the first invocation acts, so an
// abnormal disconnect and an explicit cleanup racing each other produce the
// same final state and the quota is released exactly once.
func (node *WebRTC_CDN_Node) onConnectionClose(id uint64, ip string) {
	node.mutexConnections.Lock()

	handler, exists := node.connections[id]
	if !exists {
		// Already cleaned up (duplicate/concurrent teardown): do nothing,
		// otherwise the shared per-IP counter would be decremented twice.
		node.mutexConnections.Unlock()
		return
	}

	delete(node.connections, id)

	node.mutexConnections.Unlock()

	if handler.ipCounted {
		node.RemoveIP(ip) // Release the per-IP slot exactly once
	}
}
