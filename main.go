package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

func main() {
	listen := flag.String("listen", ":8080", "Address to listen on")
	proxiesFlag := flag.String("proxies", "", "Comma-separated list of upstream proxies (e.g. user:pass@host:8080,http://host2:8080)")
	proxiesFile := flag.String("file", "", "File with one proxy per line (alternative to -proxies)")
	cooldown := flag.Duration("cooldown", 60*time.Second, "How long a burned proxy is skipped before being retried")
	flag.Parse()

	if *proxiesFile != "" && *proxiesFlag != "" {
		fmt.Fprintln(os.Stderr, "error: use either -proxies or -file, not both")
		os.Exit(1)
	}

	var raw string
	if *proxiesFile != "" {
		data, err := os.ReadFile(*proxiesFile)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error: reading proxy file:", err)
			os.Exit(1)
		}
		raw = strings.Join(nonEmptyLines(string(data)), ",")
	} else if *proxiesFlag != "" {
		raw = *proxiesFlag
	} else {
		fmt.Fprintln(os.Stderr, "error: -proxies or -file is required")
		os.Exit(1)
	}

	proxies, err := parseProxies(raw)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}

	proxy := NewProxy(proxies, *cooldown)

	log.Printf("rotational listening on %s, rotating through %d upstream proxies (cooldown: %s)", *listen, len(proxies), *cooldown)
	for _, p := range proxies {
		log.Printf("  - %s", p.Redacted())
	}

	log.Fatal(http.ListenAndServe(*listen, proxy))
}

// parseProxies parses a comma-separated list of proxy addresses.
// Entries may be full URLs (http://host:8080) or bare user:pass@host:port,
// in which case http:// is assumed.
func parseProxies(s string) ([]*url.URL, error) {
	var proxies []*url.URL
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if !strings.Contains(p, "://") {
			p = "http://" + p
		}
		u, err := url.Parse(p)
		if err != nil {
			return nil, fmt.Errorf("invalid proxy %q: %w", p, err)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return nil, fmt.Errorf("proxy must use http or https scheme: %q", p)
		}
		if u.Host == "" {
			return nil, fmt.Errorf("proxy must have a host: %q", p)
		}
		proxies = append(proxies, u)
	}
	if len(proxies) == 0 {
		return nil, fmt.Errorf("at least one proxy is required")
	}
	return proxies, nil
}

// nonEmptyLines returns the trimmed, non-empty, non-comment lines of s.
func nonEmptyLines(s string) []string {
	var lines []string
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			lines = append(lines, line)
		}
	}
	return lines
}
