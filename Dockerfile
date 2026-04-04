FROM golang:1.25 AS builder

RUN apt-get update && apt-get install -y git gcc

# Build wa-gateway
WORKDIR /app
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=1 go build -o wa-gateway .

# Build wacli with CGO (needs sqlite)
RUN CGO_ENABLED=1 go install github.com/steipete/wacli/cmd/wacli@latest

FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y ca-certificates && rm -rf /var/lib/apt/lists/*

COPY --from=builder /app/wa-gateway /usr/local/bin/wa-gateway
COPY --from=builder /go/bin/wacli /usr/local/bin/wacli

VOLUME /data/wa-session
EXPOSE 3100

CMD ["wa-gateway"]
