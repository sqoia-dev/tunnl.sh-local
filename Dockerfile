# Build stage
FROM golang:1.24-alpine AS builder

WORKDIR /build

COPY go.mod ./
COPY main.go ./

RUN CGO_ENABLED=0 GOOS=linux go build -trimpath \
    -ldflags="-s -w -X main.gitCommit=$(git rev-parse --short HEAD 2>/dev/null || echo unknown)" \
    -o resolvr .

# Runtime stage
FROM alpine:3.21

RUN apk add --no-cache ca-certificates tzdata

RUN addgroup -S resolvr && adduser -S resolvr -G resolvr

WORKDIR /app
COPY --from=builder /build/resolvr .

# 8080 — external port: inbound HTTP traffic
EXPOSE 8080

USER resolvr

# docker run -e TARGET=127.0.0.1:3000 -e BASE_PATH=/myapp -p 8080:8080 resolvr
ENTRYPOINT ["/app/resolvr"]
