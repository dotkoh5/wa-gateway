# wa-gateway

Thin HTTP gateway wrapping [wacli](https://github.com/steipete/wacli) for OtterClawd's WhatsApp integration.

## What it does

- **Inbound**: Spawns `wacli sync --follow --json`, parses messages from stdout, POSTs them to OtterClawd's webhook endpoint
- **Outbound**: Exposes `POST /send` that shells out to `wacli send text`
- **Health**: `GET /health` and `GET /status` for monitoring

## Setup

### 1. First-time auth (link to physical phone)

```bash
# Run wacli auth interactively — displays QR code in terminal
docker run -it -v wa-session:/data/wa-session wa-gateway \
  wacli auth --store /data/wa-session

# On physical phone: WhatsApp → Linked Devices → Scan QR
```

### 2. Run the gateway

```bash
cp .env.example .env
# Edit .env with your secrets

docker compose up -d
```

### 3. Verify

```bash
# Health check
curl http://localhost:3100/health

# Send a test message
curl -X POST http://localhost:3100/send \
  -H "Authorization: Bearer $API_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"to": "+6591234567", "text": "Hello from OtterClawd!"}'
```

## API

### POST /send

Send a WhatsApp message.

```
Authorization: Bearer <API_TOKEN>
Content-Type: application/json

{ "to": "+6591234567", "text": "Hello" }

→ { "success": true, "messageId": "..." }
```

### GET /status

```
→ { "connected": true, "lastMessage": "2026-04-04T12:00:00Z" }
```

### GET /health

Returns `200 ok` if wacli sync is running, `503` otherwise.

## Re-linking

If the linked device gets disconnected (phone offline >14 days, phone replaced, etc.):

```bash
docker compose exec wa-gateway wacli auth --store /data/wa-session
# Scan QR from phone, then restart:
docker compose restart wa-gateway
```
