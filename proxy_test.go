package main

import (
	"bufio"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func mustParse(raw string) *url.URL {
	u, err := url.Parse(raw)
	if err != nil {
		panic(err)
	}
	return u
}

func TestProxyRoundRobin(t *testing.T) {
	var count1, count2 atomic.Int64

	upstream1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count1.Add(1)
		w.Write([]byte("upstream1"))
	}))
	defer upstream1.Close()

	upstream2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count2.Add(1)
		w.Write([]byte("upstream2"))
	}))
	defer upstream2.Close()

	proxy := httptest.NewServer(NewProxy([]*url.URL{
		mustParse(upstream1.URL),
		mustParse(upstream2.URL),
	}, time.Minute))
	defer proxy.Close()

	client := &http.Client{
		Transport: &http.Transport{Proxy: http.ProxyURL(mustParse(proxy.URL))},
	}

	for i := 0; i < 4; i++ {
		resp, err := client.Get("http://example.com/")
		if err != nil {
			t.Fatalf("request %d failed: %v", i, err)
		}
		resp.Body.Close()
	}

	if got := count1.Load(); got != 2 {
		t.Errorf("upstream1 got %d requests, want 2", got)
	}
	if got := count2.Load(); got != 2 {
		t.Errorf("upstream2 got %d requests, want 2", got)
	}
}

func TestProxyForwardsRequest(t *testing.T) {
	var gotURL string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURL = r.URL.String()
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	}))
	defer upstream.Close()

	proxy := httptest.NewServer(NewProxy([]*url.URL{mustParse(upstream.URL)}, time.Minute))
	defer proxy.Close()

	client := &http.Client{
		Transport: &http.Transport{Proxy: http.ProxyURL(mustParse(proxy.URL))},
	}

	resp, err := client.Get("http://example.com/test")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if string(body) != "ok" {
		t.Errorf("got body %q, want %q", body, "ok")
	}

	if gotURL != "http://example.com/test" {
		t.Errorf("upstream received URL %q, want %q", gotURL, "http://example.com/test")
	}
}

func TestProxyUpstreamError(t *testing.T) {
	// Point at a port that is not listening
	proxy := httptest.NewServer(NewProxy([]*url.URL{
		mustParse("http://127.0.0.1:1"),
	}, time.Minute))
	defer proxy.Close()

	client := &http.Client{
		Transport: &http.Transport{Proxy: http.ProxyURL(mustParse(proxy.URL))},
	}

	resp, err := client.Get("http://example.com/")
	if err != nil {
		t.Fatalf("expected a response (502), got error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("got status %d, want %d", resp.StatusCode, http.StatusBadGateway)
	}
}

func TestProxyConnect(t *testing.T) {
	// Target server behind the upstream proxy
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("hello from target"))
	}))
	defer target.Close()
	targetAddr := strings.TrimPrefix(target.URL, "http://")

	// Fake upstream proxy that handles CONNECT by tunneling to the target
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			t.Errorf("upstream got method %s, want CONNECT", r.Method)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		targetConn, err := net.Dial("tcp", targetAddr)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		defer targetConn.Close()

		hj, ok := w.(http.Hijacker)
		if !ok {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		conn, buf, err := hj.Hijack()
		if err != nil {
			return
		}
		defer conn.Close()

		buf.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		buf.Flush()

		go io.Copy(targetConn, conn)
		io.Copy(conn, targetConn)
	}))
	defer upstream.Close()

	proxy := httptest.NewServer(NewProxy([]*url.URL{mustParse(upstream.URL)}, time.Minute))
	defer proxy.Close()

	// Connect through rotational using CONNECT
	conn, err := net.Dial("tcp", strings.TrimPrefix(proxy.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// Send CONNECT to rotational
	fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", targetAddr, targetAddr)

	// Read the 200 response
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT failed: status %d", resp.StatusCode)
	}

	// Send a plain HTTP request through the tunnel
	fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: %s\r\n\r\n", targetAddr)

	httpResp, err := http.ReadResponse(br, &http.Request{Method: "GET"})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(httpResp.Body)
	if string(body) != "hello from target" {
		t.Errorf("got body %q, want %q", body, "hello from target")
	}
}

func TestParseProxies(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    []string
		wantErr bool
	}{
		{
			name:  "bare user:pass@host:port gets http scheme",
			input: "puser1:ppass123@188.220.219.61:47131",
			want:  []string{"http://puser1:ppass123@188.220.219.61:47131"},
		},
		{
			name:  "bare host:port",
			input: "10.0.0.1:8080",
			want:  []string{"http://10.0.0.1:8080"},
		},
		{
			name:  "full http URL",
			input: "http://proxy1:8080",
			want:  []string{"http://proxy1:8080"},
		},
		{
			name:  "https URL",
			input: "https://proxy1:8443",
			want:  []string{"https://proxy1:8443"},
		},
		{
			name:  "multiple with spaces",
			input: "user:pass@host1:8080, http://host2:8080 , https://host3:8443",
			want: []string{
				"http://user:pass@host1:8080",
				"http://host2:8080",
				"https://host3:8443",
			},
		},
		{
			name:    "empty string",
			input:   "",
			wantErr: true,
		},
		{
			name:    "bad scheme",
			input:   "socks5://host:1080",
			wantErr: true,
		},
		{
			name:    "no host",
			input:   "http://",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseProxies(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got %d proxies, want %d", len(got), len(tt.want))
			}
			for i, u := range got {
				if u.String() != tt.want[i] {
					t.Errorf("proxy %d: got %s, want %s", i, u.String(), tt.want[i])
				}
			}
		})
	}
}

func TestParseProxiesUserInfo(t *testing.T) {
	proxies, err := parseProxies("puser1:ppass123@188.220.219.61:47131")
	if err != nil {
		t.Fatal(err)
	}
	u := proxies[0]
	if got := u.User.Username(); got != "puser1" {
		t.Errorf("username: got %q, want %q", got, "puser1")
	}
	password, ok := u.User.Password()
	if !ok {
		t.Fatal("no password in userinfo")
	}
	if password != "ppass123" {
		t.Errorf("password: got %q, want %q", password, "ppass123")
	}
	if got := u.Host; got != "188.220.219.61:47131" {
		t.Errorf("host: got %q, want %q", got, "188.220.219.61:47131")
	}
}

func TestNonEmptyLines(t *testing.T) {
	input := "line1\n\n  line2  \n# comment\nline3\n"
	got := nonEmptyLines(input)
	want := []string{"line1", "line2", "line3"}
	if len(got) != len(want) {
		t.Fatalf("got %d lines, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d: got %q, want %q", i, got[i], want[i])
		}
	}
}

func TestProxySendsAuthToUpstream(t *testing.T) {
	var gotAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Proxy-Authorization")
		w.Write([]byte("ok"))
	}))
	defer upstream.Close()

	host := strings.TrimPrefix(upstream.URL, "http://")
	proxy := httptest.NewServer(NewProxy([]*url.URL{
		mustParse("http://testuser:testpass@" + host),
	}, time.Minute))
	defer proxy.Close()

	client := &http.Client{
		Transport: &http.Transport{Proxy: http.ProxyURL(mustParse(proxy.URL))},
	}
	resp, err := client.Get("http://example.com/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("testuser:testpass"))
	if gotAuth != want {
		t.Errorf("Proxy-Authorization: got %q, want %q", gotAuth, want)
	}
}

func TestProxyConnectSendsAuth(t *testing.T) {
	var gotAuth, gotConnectHost string

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		gotAuth = r.Header.Get("Proxy-Authorization")
		gotConnectHost = r.Host

		hj, ok := w.(http.Hijacker)
		if !ok {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		conn, buf, _ := hj.Hijack()
		defer conn.Close()
		buf.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		buf.Flush()
	}))
	defer upstream.Close()

	host := strings.TrimPrefix(upstream.URL, "http://")
	proxy := httptest.NewServer(NewProxy([]*url.URL{
		mustParse("http://testuser:testpass@" + host),
	}, time.Minute))
	defer proxy.Close()

	conn, err := net.Dial("tcp", strings.TrimPrefix(proxy.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	fmt.Fprintf(conn, "CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\n\r\n")

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT failed: status %d", resp.StatusCode)
	}

	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("testuser:testpass"))
	if gotAuth != want {
		t.Errorf("Proxy-Authorization: got %q, want %q", gotAuth, want)
	}
	if gotConnectHost != "example.com:443" {
		t.Errorf("CONNECT host: got %q, want %q", gotConnectHost, "example.com:443")
	}
}

func TestUpstreamBurn(t *testing.T) {
	u := &upstream{}

	if u.isBurned() {
		t.Error("new upstream should not be burned")
	}

	u.burn(time.Hour)
	if !u.isBurned() {
		t.Error("upstream should be burned after burn()")
	}

	// Expired cooldown
	u.burn(1 * time.Millisecond)
	time.Sleep(10 * time.Millisecond)
	if u.isBurned() {
		t.Error("upstream should not be burned after cooldown expires")
	}
}

func TestProxySkipsBurned(t *testing.T) {
	var liveCount atomic.Int64
	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		liveCount.Add(1)
		w.Write([]byte("ok"))
	}))
	defer live.Close()

	// Dead upstream (nothing listening on port 1)
	dead := mustParse("http://127.0.0.1:1")

	proxy := httptest.NewServer(NewProxy([]*url.URL{dead, mustParse(live.URL)}, time.Minute))
	defer proxy.Close()

	client := &http.Client{
		Transport: &http.Transport{Proxy: http.ProxyURL(mustParse(proxy.URL))},
	}

	// Request 1: goes to dead (round-robin position 0) → 502, dead is burned
	resp, _ := client.Get("http://example.com/")
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("request 1: got status %d, want 502", resp.StatusCode)
	}

	// Requests 2-4: dead is burned, all go to live
	for i := 2; i <= 4; i++ {
		resp, err := client.Get("http://example.com/")
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("request %d: got status %d, want 200", i, resp.StatusCode)
		}
	}

	if got := liveCount.Load(); got != 3 {
		t.Errorf("live upstream got %d requests, want 3", got)
	}
}

func TestProxyCooldownExpiry(t *testing.T) {
	var liveCount atomic.Int64
	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		liveCount.Add(1)
		w.Write([]byte("ok"))
	}))
	defer live.Close()

	dead := mustParse("http://127.0.0.1:1")
	proxy := httptest.NewServer(NewProxy([]*url.URL{dead, mustParse(live.URL)}, 50*time.Millisecond))
	defer proxy.Close()

	client := &http.Client{
		Transport: &http.Transport{Proxy: http.ProxyURL(mustParse(proxy.URL))},
	}

	// Request 1: dead → 502, burned for 50ms
	resp, _ := client.Get("http://example.com/")
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("request 1: got status %d, want 502", resp.StatusCode)
	}

	// Request 2: dead burned → live → 200
	resp, _ = client.Get("http://example.com/")
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("request 2: got status %d, want 200", resp.StatusCode)
	}

	// Wait for cooldown to expire
	time.Sleep(100 * time.Millisecond)

	// Request 3: dead cooldown expired → dead tried again → 502
	resp, _ = client.Get("http://example.com/")
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("request 3: got status %d, want 502 (dead should be retried after cooldown)", resp.StatusCode)
	}

	// Request 4: dead burned again → live → 200
	resp, _ = client.Get("http://example.com/")
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("request 4: got status %d, want 200", resp.StatusCode)
	}

	if got := liveCount.Load(); got != 2 {
		t.Errorf("live upstream got %d requests, want 2", got)
	}
}

func TestProxyAllBurned(t *testing.T) {
	dead1 := mustParse("http://127.0.0.1:1")
	dead2 := mustParse("http://127.0.0.1:2")
	proxy := httptest.NewServer(NewProxy([]*url.URL{dead1, dead2}, time.Minute))
	defer proxy.Close()

	client := &http.Client{
		Transport: &http.Transport{Proxy: http.ProxyURL(mustParse(proxy.URL))},
	}

	// All requests should return 502 but not hang or crash
	for i := 0; i < 4; i++ {
		resp, err := client.Get("http://example.com/")
		if err != nil {
			t.Fatalf("request %d: unexpected error: %v", i, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadGateway {
			t.Errorf("request %d: got status %d, want 502", i, resp.StatusCode)
		}
	}
}

func TestProxyBurnOnHTTPError(t *testing.T) {
	var badCount, goodCount atomic.Int64
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		badCount.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer bad.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		goodCount.Add(1)
		w.Write([]byte("ok"))
	}))
	defer good.Close()

	proxy := httptest.NewServer(NewProxy([]*url.URL{
		mustParse(bad.URL), mustParse(good.URL),
	}, time.Minute))
	defer proxy.Close()

	client := &http.Client{
		Transport: &http.Transport{Proxy: http.ProxyURL(mustParse(proxy.URL))},
	}

	// Request 1: bad upstream returns 502 → burned
	resp, _ := client.Get("http://example.com/")
	resp.Body.Close()

	// Requests 2-3: bad is burned → good
	for i := 0; i < 2; i++ {
		resp, _ := client.Get("http://example.com/")
		resp.Body.Close()
	}

	if got := badCount.Load(); got != 1 {
		t.Errorf("bad upstream got %d requests, want 1 (should be burned after first 502)", got)
	}
	if got := goodCount.Load(); got != 2 {
		t.Errorf("good upstream got %d requests, want 2", got)
	}
}

func TestProxyBurnOnWAFBlock(t *testing.T) {
	var wafCount, goodCount atomic.Int64
	waf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wafCount.Add(1)
		w.WriteHeader(statusWAFBlocked) // 456: WAF/antibot blocked the proxy IP
	}))
	defer waf.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		goodCount.Add(1)
		w.Write([]byte("ok"))
	}))
	defer good.Close()

	proxy := httptest.NewServer(NewProxy([]*url.URL{
		mustParse(waf.URL), mustParse(good.URL),
	}, time.Minute))
	defer proxy.Close()

	client := &http.Client{
		Transport: &http.Transport{Proxy: http.ProxyURL(mustParse(proxy.URL))},
	}

	// Request 1: WAF upstream returns 456 → burned
	resp, _ := client.Get("http://example.com/")
	resp.Body.Close()

	// Requests 2-3: WAF upstream is burned → good
	for i := 0; i < 2; i++ {
		resp, _ := client.Get("http://example.com/")
		resp.Body.Close()
	}

	if got := wafCount.Load(); got != 1 {
		t.Errorf("WAF upstream got %d requests, want 1 (should be burned after 456)", got)
	}
	if got := goodCount.Load(); got != 2 {
		t.Errorf("good upstream got %d requests, want 2", got)
	}
}

func TestProxyConnectBurnOnWAFBlock(t *testing.T) {
	var wafConnectCount atomic.Int64
	waf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect {
			wafConnectCount.Add(1)
			w.WriteHeader(statusWAFBlocked)
			return
		}
		w.Write([]byte("ok"))
	}))
	defer waf.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))
	defer good.Close()

	proxy := httptest.NewServer(NewProxy([]*url.URL{
		mustParse(waf.URL), mustParse(good.URL),
	}, time.Minute))
	defer proxy.Close()

	// CONNECT through rotational → WAF upstream returns 456 → burned
	conn, err := net.Dial("tcp", strings.TrimPrefix(proxy.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(conn, "CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\n\r\n")
	br := bufio.NewReader(conn)
	resp, _ := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	conn.Close()
	if resp.StatusCode != statusWAFBlocked {
		t.Errorf("CONNECT: got status %d, want %d", resp.StatusCode, statusWAFBlocked)
	}

	// HTTP request: WAF upstream should be burned, traffic goes to good
	client := &http.Client{
		Transport: &http.Transport{Proxy: http.ProxyURL(mustParse(proxy.URL))},
	}
	httpResp, err := client.Get("http://example.com/")
	if err != nil {
		t.Fatal(err)
	}
	httpResp.Body.Close()
	if httpResp.StatusCode != http.StatusOK {
		t.Errorf("HTTP after WAF CONNECT burn: got status %d, want 200", httpResp.StatusCode)
	}

	if got := wafConnectCount.Load(); got != 1 {
		t.Errorf("WAF upstream got %d CONNECT requests, want 1 (should be burned after 456)", got)
	}
}

func TestProxyConnectBurn(t *testing.T) {
	dead := mustParse("http://127.0.0.1:1")
	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))
	defer live.Close()

	proxy := httptest.NewServer(NewProxy([]*url.URL{dead, mustParse(live.URL)}, time.Minute))
	defer proxy.Close()

	// CONNECT to dead upstream → dial fails → burned → 502
	conn, err := net.Dial("tcp", strings.TrimPrefix(proxy.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(conn, "CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\n\r\n")
	br := bufio.NewReader(conn)
	resp, _ := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	conn.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("CONNECT to dead: got status %d, want 502", resp.StatusCode)
	}

	// HTTP request: dead should be burned, traffic goes to live
	client := &http.Client{
		Transport: &http.Transport{Proxy: http.ProxyURL(mustParse(proxy.URL))},
	}
	httpResp, err := client.Get("http://example.com/")
	if err != nil {
		t.Fatal(err)
	}
	httpResp.Body.Close()
	if httpResp.StatusCode != http.StatusOK {
		t.Errorf("HTTP request after CONNECT burn: got status %d, want 200", httpResp.StatusCode)
	}
}

func TestProxyRejectsNonProxyRequest(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream must not be contacted")
	}))
	defer up.Close()

	p := NewProxy([]*url.URL{mustParse(up.URL)}, time.Minute)
	srv := httptest.NewServer(p)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
	if p.upstreams[0].isBurned() {
		t.Error("upstream burned by a non-proxy request")
	}
}
