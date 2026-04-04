FROM golang:1.25-alpine AS builder

# Install build deps for CGO (wacli needs sqlite via cgo)
RUN apk add --no-cache git gcc musl-dev

# Build wa-gateway (pure Go, no CGO needed)
WORKDIR /app
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o wa-gateway .

# Build wacli with CGO enabled (requires sqlite)
RUN CGO_ENABLED=1 go install github.com/steipete/wacli/cmd/wacli@latest

FROM alpine:3.20

# wacli needs libc for CGO sqlite
RUN apk add --no-cache ca-certificates

COPY --from=builder /app/wa-gateway /usr/local/bin/wa-gateway
COPY --from=builder /go/bin/wacli /usr/local/bin/wacli

VOLUME /data/wa-session
EXPOSE 3100

# No fixed entrypoint — allows running either wa-gateway or wacli
CMD ["wa-gateway"]
