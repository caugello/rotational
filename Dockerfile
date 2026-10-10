# Build stage
FROM registry.access.redhat.com/hi/go:latest AS builder
WORKDIR /build
COPY go.mod *.go ./
RUN CGO_ENABLED=0 go build -o /tmp/rotational .

# Runtime stage
FROM registry.access.redhat.com/hi/core-runtime:latest
COPY --from=builder /tmp/rotational /app
EXPOSE 8080
ENTRYPOINT ["/app"]
