# WhatsApp Bridge

A lightweight HTTP API bridge for WhatsApp built with Go and [whatsmeow](https://github.com/tulir/whatsmeow). Send text and media messages, receive incoming messages via webhooks, and manage device linking through a web-based QR/pairing code page.

## Features

- Send text messages and media (image, video, audio, document) via REST API
- Receive incoming messages forwarded to a configurable webhook
- Web-based QR code and pairing code page for device linking
- API key authentication and secret-path URL obfuscation
- Rate-limited QR page with brute-force protection
- SQLite-backed session persistence (survives restarts)
- Zero external runtime dependencies — single binary

## Requirements

- Go 1.25+
- A WhatsApp account to link as a secondary device

## Quick Start

### 1. Clone and build

```bash
git clone <repo-url> && cd whatsapp-bridge-go
go build -o whatsapp-bridge .
```

### 2. Configure environment

Copy the example below into a `.env` file in the project root:

```env
# Server
PORT=3567

# Security — all values below are examples, generate your own
API_PATH_SECRET=your_random_string_min_16_chars
API_KEY=your_api_key
QR_ACCESS_SECRET=your_random_string_min_32_chars

# Webhook (optional)
WEBHOOK_URL=https://example.com/api/webhooks/wa-bridge
WEBHOOK_SECRET=your_webhook_secret

# Pairing code login (optional — omit to use QR code instead)
# PAIRING_PHONE_NUMBER=1234567890
```

| Variable | Required | Description |
|---|---|---|
| `PORT` | No | HTTP port (default `3567`) |
| `API_PATH_SECRET` | Yes | Random string (≥16 chars) used as the API URL prefix: `/api-<secret>/…` |
| `API_KEY` | Yes | Key required in `X-API-Key` header (or `?apiKey=` query param) for API calls |
| `QR_ACCESS_SECRET` | Yes | Secret (≥32 chars) required as `?secret=` to access the QR page |
| `WEBHOOK_URL` | No | URL to POST incoming messages to |
| `WEBHOOK_SECRET` | No | Sent as `X-Webhook-Secret` header on webhook calls |
| `PAIRING_PHONE_NUMBER` | No | Phone number (with country code, no `+`) to use pairing code flow instead of QR |

### 3. Run

```bash
./whatsapp-bridge
```

### 4. Link your WhatsApp

Open the QR page in a browser:

```
http://localhost:3567/api-<API_PATH_SECRET>/qr?secret=<QR_ACCESS_SECRET>
```

Then in WhatsApp → **Linked Devices** → **Link a Device** and scan the QR code (or enter the pairing code if `PAIRING_PHONE_NUMBER` is set).

The session is stored in `wa_store.db` and persists across restarts.

## API Reference

All endpoints live under `/api-<API_PATH_SECRET>/` and require the `X-API-Key` header (except health and QR).

### Health Check

```
GET /api-<secret>/health
```

No authentication required. Returns:

```json
{ "status": "ok", "whatsapp": "open" }
```

### Connection Status

```
GET /api-<secret>/status
```

```json
{ "connected": true, "state": "open" }
```

### Send Text Message

```
POST /api-<secret>/send
Content-Type: application/json
X-API-Key: <API_KEY>

{
  "to": "1234567890",
  "message": "Hello from the bridge!"
}
```

`to` is the recipient's phone number with country code, no `+` or spaces.

Response:

```json
{ "success": true, "messageId": "ABCDEF123456" }
```

### Send Media

```
POST /api-<secret>/send-media
Content-Type: application/json
X-API-Key: <API_KEY>

{
  "to": "1234567890",
  "url": "https://example.com/photo.jpg",
  "caption": "Check this out",
  "type": "image"
}
```

| Field | Required | Description |
|---|---|---|
| `to` | Yes | Recipient phone number |
| `url` | Yes | Public URL of the media file |
| `caption` | No | Caption text |
| `type` | No | `image` (default), `video`, `audio`, `document` |

### QR / Pairing Page

```
GET /api-<secret>/qr?secret=<QR_ACCESS_SECRET>
```

Browser-friendly page that shows the QR code, pairing code, or connection status. Rate-limited to 5 requests per 15 minutes per IP.

## Webhook Events

When `WEBHOOK_URL` is configured, incoming messages (non-group, non-self) are POSTed as JSON:

```json
{
  "event": "message.received",
  "contact": {
    "phoneNumber": "1234567890",
    "name": "John"
  },
  "message": {
    "id": "ABCDEF123456",
    "type": "text",
    "text": "Hey there",
    "caption": "",
    "from": "1234567890",
    "timestamp": 1745455512
  }
}
```

Supported message types: `text`, `image`, `video`, `audio`, `document`.

If `WEBHOOK_SECRET` is set, it is included as the `X-Webhook-Secret` header on every webhook request.

## Project Structure

```
.
├── main.go          # HTTP server, routing, rate limiter, env loading
├── wa/
│   └── bridge.go    # WhatsApp client, QR/pairing, send/receive, webhook
├── go.mod
├── .env             # Configuration (not committed)
└── wa_store.db      # SQLite session store (auto-created, not committed)
```

## Security Notes

- The API path itself is secret (`/api-<random>/`) — acts as a first layer of obscurity.
- All mutating endpoints require the `X-API-Key` header.
- The QR page requires a separate `QR_ACCESS_SECRET` query parameter and is rate-limited.
- Constant-time comparison is used for all secret checks to prevent timing attacks.
- Never commit your `.env` file.

## License

MIT
