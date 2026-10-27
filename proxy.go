package main

import (
	"bufio"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
)

// statusWAFBlocked is a non-standard status code (456) returned by some
// WAF/antibot systems when the proxy IP has been flagged as blocked.
const statusWAFBlocked = 456

type upstream struct {
	url         *url.URL
	transport   *http.Transport
	burnedUntil atomic.Int64 // Unix nanoseconds; 0 = healthy
}

func (u *upstream) isBurned() bool {
	until := u.burnedUntil.Load()
	return until > 0 && time.Now().UnixNano() < until
}

func (u *upstream) burn(d time.Duration) {
	u.burnedUntil.Store(time.Now().Add(d).UnixNano())
}

// Proxy is a forward proxy that rotates through upstream proxies round-robin.
// Burned proxies are put in a cooldown and skipped until it expires.
type Proxy struct {
	upstreams []*upstream
	counter   atomic.Uint64
	cooldown  time.Duration
}

// NewProxy creates a Proxy that rotates through the given upstream proxy URLs.
// cooldown is how long a burned proxy is skipped before being retried.
func NewProxy(proxyURLs []*url.URL, cooldown time.Duration) *Proxy {
	p := &Proxy{cooldown: cooldown}
	for _, u := range proxyURLs {
		p.upstreams = append(p.upstreams, &upstream{
			url: u,
			transport: &http.Transport{
				Proxy: http.ProxyURL(u),
			},
		})
	}
	return p
}

func (p *Proxy) next() *upstream {
	n := len(p.upstreams)
	start := int(p.counter.Add(1)-1) % n
	// Scan forward from the round-robin position for a healthy upstream
	for i := 0; i < n; i++ {
		idx := (start + i) % n
		if !p.upstreams[idx].isBurned() {
			return p.upstreams[idx]
		}
	}
	// All burned — fall through to round-robin anyway
	return p.upstreams[start]
}

// ServeHTTP implements http.Handler.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Proxy requests carry an absolute URI; anything else (health checks,
	// browsers hitting us directly) is the client's mistake, not the upstream's.
	if r.Method != http.MethodConnect && !r.URL.IsAbs() {
		http.Error(w, "this is a forward proxy: absolute URI required", http.StatusBadRequest)
		return
	}

	up := p.next()
	log.Printf("%s %s via %s", r.Method, r.Host, up.url.Redacted())

	if r.Method == http.MethodConnect {
		p.handleConnect(w, r, up)
		return
	}

	p.handleHTTP(w, r, up)
}

func (p *Proxy) handleHTTP(w http.ResponseWriter, r *http.Request, up *upstream) {
	// Clone the request and clean it for client-side transport
	outReq := r.Clone(r.Context())
	outReq.RequestURI = ""
	removeHopHeaders(outReq.Header)

	resp, err := up.transport.RoundTrip(outReq)
	if err != nil {
		up.burn(p.cooldown)
		log.Printf("burned %s: %v", up.url.Redacted(), err)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// Burn on gateway-level failures and WAF/antibot blocks from the upstream
	if resp.StatusCode == http.StatusProxyAuthRequired ||
		resp.StatusCode == http.StatusBadGateway ||
		resp.StatusCode == http.StatusServiceUnavailable ||
		resp.StatusCode == http.StatusGatewayTimeout ||
		resp.StatusCode == statusWAFBlocked {
		up.burn(p.cooldown)
		log.Printf("burned %s: upstream returned %d", up.url.Redacted(), resp.StatusCode)
	}

	removeHopHeaders(resp.Header)
	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

func (p *Proxy) handleConnect(w http.ResponseWriter, r *http.Request, up *upstream) {
	// Hijack the client connection
	clientConn, clientBuf, err := w.(http.Hijacker).Hijack()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer clientConn.Close()

	// Connect to the upstream proxy
	var upstreamConn net.Conn
	if up.url.Scheme == "https" {
		upstreamConn, err = tls.Dial("tcp", up.url.Host, &tls.Config{
			ServerName: up.url.Hostname(),
		})
	} else {
		upstreamConn, err = net.Dial("tcp", up.url.Host)
	}
	if err != nil {
		up.burn(p.cooldown)
		log.Printf("burned %s: %v", up.url.Redacted(), err)
		clientBuf.WriteString("HTTP/1.1 502 Bad Gateway\r\n\r\n")
		clientBuf.Flush()
		return
	}
	defer upstreamConn.Close()

	// Send CONNECT to the upstream proxy
	var b strings.Builder
	fmt.Fprintf(&b, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n", r.Host, r.Host)
	if up.url.User != nil {
		password, _ := up.url.User.Password()
		creds := base64.StdEncoding.EncodeToString(
			[]byte(up.url.User.Username() + ":" + password))
		fmt.Fprintf(&b, "Proxy-Authorization: Basic %s\r\n", creds)
	}
	b.WriteString("\r\n")

	if _, err := upstreamConn.Write([]byte(b.String())); err != nil {
		up.burn(p.cooldown)
		log.Printf("burned %s: %v", up.url.Redacted(), err)
		clientBuf.WriteString("HTTP/1.1 502 Bad Gateway\r\n\r\n")
		clientBuf.Flush()
		return
	}

	// Read the upstream's response to CONNECT
	br := bufio.NewReader(upstreamConn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		up.burn(p.cooldown)
		log.Printf("burned %s: %v", up.url.Redacted(), err)
		clientBuf.WriteString("HTTP/1.1 502 Bad Gateway\r\n\r\n")
		clientBuf.Flush()
		return
	}

	if resp.StatusCode != http.StatusOK {
		// Burn on auth failure, gateway errors, or WAF blocks;
		// 403 may be target-specific
		if resp.StatusCode == http.StatusProxyAuthRequired ||
			resp.StatusCode == statusWAFBlocked ||
			resp.StatusCode >= 500 {
			up.burn(p.cooldown)
			log.Printf("burned %s: CONNECT returned %d", up.url.Redacted(), resp.StatusCode)
		}
		fmt.Fprintf(clientBuf, "HTTP/1.1 %d %s\r\n\r\n", resp.StatusCode, resp.Status)
		clientBuf.Flush()
		return
	}

	// Tell the client the tunnel is established
	clientBuf.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
	clientBuf.Flush()

	// Relay data in both directions (br wraps upstreamConn, draining any
	// bytes the upstream sent immediately after the CONNECT response)
	go func() {
		io.Copy(upstreamConn, clientConn)
		upstreamConn.Close()
	}()
	io.Copy(clientConn, br)
}

var hopHeaders = []string{
	"Connection",
	"Proxy-Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Te",
	"Trailers",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}

func removeHopHeaders(h http.Header) {
	for _, k := range hopHeaders {
		h.Del(k)
	}
}
