# Build stage
FROM golang:1.24-alpine AS builder

WORKDIR /build

COPY go.mod ./
COPY main.go ./

ARG GIT_COMMIT=unknown
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath \
    -ldflags="-s -w -X main.gitCommit=${GIT_COMMIT}" \
    -o adaptr .

# Runtime stage
FROM alpine:3.21

RUN apk add --no-cache ca-certificates tzdata wget

RUN addgroup -S adaptr && adduser -S adaptr -G adaptr

WORKDIR /app
COPY --from=builder /build/adaptr .

# 8080 — external port: inbound HTTP traffic
EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=3s CMD wget -qO- http://localhost:8080/health || exit 1

USER adaptr

# docker run -e TARGET=127.0.0.1:3000 -e BASE_PATH=/myapp -p 8080:8080 adaptr
ENTRYPOINT ["/app/adaptr"]
