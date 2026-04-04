FROM golang:1.22-alpine AS builder

# Build wa-gateway
WORKDIR /app
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o wa-gateway .

# Build wacli from source (no Linux binary in releases)
RUN apk add --no-cache git && \
    go install github.com/steipete/wacli@latest

FROM alpine:3.20

RUN apk add --no-cache ca-certificates

COPY --from=builder /app/wa-gateway /usr/local/bin/wa-gateway
COPY --from=builder /go/bin/wacli /usr/local/bin/wacli

VOLUME /data/wa-session
EXPOSE 3100

ENTRYPOINT ["wa-gateway"]
