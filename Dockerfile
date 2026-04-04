FROM golang:1.22-alpine AS builder

WORKDIR /app
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o wa-gateway .

FROM alpine:3.20

# Install wacli
RUN apk add --no-cache ca-certificates curl && \
    curl -fsSL https://github.com/steipete/wacli/releases/latest/download/wacli_linux_amd64.tar.gz | \
    tar xz -C /usr/local/bin wacli && \
    chmod +x /usr/local/bin/wacli

COPY --from=builder /app/wa-gateway /usr/local/bin/wa-gateway

VOLUME /data/wa-session
EXPOSE 3100

ENTRYPOINT ["wa-gateway"]
