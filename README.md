# rotational

[![Go](https://img.shields.io/badge/go-1.21+-00ADD8?logo=go)](https://go.dev)
[![License](https://img.shields.io/badge/license-MIT-blue)](LICENSE)

A tiny round-robin proxy written in Go. Point your app at rotational, and it
rotates every request through a pool of upstream proxies — spreading traffic,
avoiding rate limits, and automatically benching proxies that get burned.

```
app → rotational → upstream proxy 1 ─┐
                     upstream proxy 2 ─┼→ target
                     upstream proxy 3 ─┘
```

## Features

- **Round-robin rotation** — every request goes through the next proxy in the pool
- **Burn & cooldown** — failed or WAF-blocked proxies are benched automatically and retried later
- **HTTP + HTTPS** — plain HTTP and CONNECT tunneling
- **Proxy auth** — credentials via URL (`user:pass@host:port`)
- **Tiny** — single static binary, ~65 MB Docker image, zero dependencies
- **Hardened image** — built on Red Hat hardened images

## Install

### Go

```bash
go install github.com/caugello/rotational@latest
```

### Docker

```bash
docker build -t rotational .
```

### From source

```bash
git clone https://github.com/caugello/rotational.git
cd rotational
go build -o rotational .
```

## Quick start

```bash
# Inline proxy list
rotational -proxies user:pass@proxy1:8080,user:pass@proxy2:8080

# Or from a file (one proxy per line)
rotational -file proxies.txt
```

Then point your app at `http://localhost:8080` as its HTTP/HTTPS proxy.

## Proxy file format

One proxy per line. Blank lines and `#` comments are ignored:

```
# my proxy pool
puser1:ppass123@188.220.219.61:47131
puser1:ppass123@203.0.113.2:8080
http://proxy3:8080
```

Accepted entry formats:

| Format                       | Example                                                |
| ---------------------------- | ------------------------------------------------------ |
| `user:pass@host:port`        | `puser1:ppass123@188.220.219.61:47131` |
| `host:port`                  | `10.0.0.1:8080`                                        |
| `http://host:port`           | `http://proxy1:8080`                                   |
| `http://user:pass@host:port` | `http://user:pass@proxy1:8080`                         |
| `https://...`                | `https://proxy1:8443`                                  |

Bare entries (no scheme) default to `http://`.

## Flags

| Flag        | Default | Description                                              |
| ----------- | ------- | -------------------------------------------------------- |
| `-listen`   | `:8080` | Address to listen on                                     |
| `-proxies`  |         | Comma-separated list of upstream proxies                 |
| `-file`     |         | File with one proxy per line (alternative to `-proxies`) |
| `-cooldown` | `60s`   | How long a burned proxy is skipped before being retried  |

## Docker

```bash
docker build -t rotational .

# Inline proxies
docker run -p 8080:8080 rotational \
  -proxies user:pass@proxy1:8080,user:pass@proxy2:8080

# Proxy file
docker run -p 8080:8080 \
  -v "$(pwd)/proxies.txt:/proxies.txt:ro" \
  rotational -file /proxies.txt -cooldown 2m
```

Pre-built images are pushed to Quay on every merge to `main` and on version tags:

```bash
docker pull quay.io/caugello/rotational:latest
```

## CI/CD

GitHub Actions workflow (`.github/workflows/ci.yaml`) runs on every push and PR:

| Job | Runs on | What it does |
|-----|---------|--------------|
| Lint & Test | PRs + pushes | `gofmt`, `go vet`, `go test -race` |
| Build & Push | Pushes to `main` + `v*` tags | Builds image, pushes to Quay |

Image tags:
- `v1.2.3` tag → pushes `1.2.3` and `latest`
- `main` push → pushes `latest` and `sha-<short-sha>`

Required GitHub secrets: `QUAY_USERNAME`, `QUAY_PASSWORD` (robot account token recommended, e.g. `caugello+ci_bot`).

## How it works

### Round-robin

Each incoming request is forwarded through the next proxy in the list.
Supports both plain HTTP and HTTPS (CONNECT tunneling). Credentials in the
proxy URL (`user:pass@`) are sent as `Proxy-Authorization` to the upstream.

### Burn & cooldown

When a proxy fails, it's put in a cooldown and skipped until it expires:

| Signal                              | Action |
| ----------------------------------- | ------ |
| Connection error (refused, timeout) | burned |
| HTTP 407 (auth required)            | burned |
| HTTP 502 / 503 / 504                | burned |
| HTTP 456 (WAF/antibot block)        | burned |
| CONNECT returns 407 / 456 / 5xx     | burned |

While in cooldown, the proxy is skipped and traffic goes to the remaining
healthy proxies. After the cooldown expires, the proxy is tried again.
If all proxies are in cooldown, rotational falls back to round-robin
(better to try than to fail).

## Contributing

Contributions are welcome! Open an issue or submit a pull request.

```bash
git clone https://github.com/caugello/rotational.git
cd rotational
go test ./...
```

## License

[MIT](LICENSE)
