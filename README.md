# War Era Gateway

Caching proxy and request batcher for the [War Era](https://warera.io/) API. Batches requests within a 400ms window, caches responses, and stores paginated data locally in PostgreSQL.

## Prerequisites

- [Go 1.25+](https://go.dev/dl/)
- [Docker](https://docs.docker.com/get-docker/) (or Podman)
- A War Era API key

## Quick Start

```bash
# Clone the repo
git clone https://github.com/Hattorius/War-Era-Gateway.git
cd War-Era-Gateway

# Set up environment
cp env.example .env
# Edit .env and add your WARERA_API_KEY

# Start all services
docker compose up -d
```

This starts four containers:

| Service    | Description                              |
|------------|------------------------------------------|
| `postgres` | PostgreSQL 17 database                   |
| `gateway`  | API gateway on port 8080 (internal)      |
| `scraper`  | Background scraper (every 5s)            |
| `caddy`    | Reverse proxy with automatic TLS (80/443)|

The gateway is available at `https://gateway.warerastats.io/trpc/` in production, or `http://localhost:80/trpc/` locally (through Caddy).

## Development

### Run without Docker

```bash
# Start a local PostgreSQL (or use docker compose up postgres -d)
export DATABASE_URL="postgres://app:app@localhost:5432/app?sslmode=disable"

# Run the gateway
go run ./cmd/gateway

# In another terminal, run the scraper
export WARERA_API_KEY="your-key"
go run ./cmd/scraper
```

### Project Structure

```
cmd/
  gateway/        # HTTP gateway server
    main.go       # Entry point, routing, cache init
    cache.go      # Cache TTL config per endpoint
    data_handler.go # DB-first logic for paginated endpoints
    trpc_handler.go # Request parsing, batching orchestration
  scraper/        # Background data scraper
internal/
  database/       # PostgreSQL connection & models
  scraper/        # Batching engine, API client, worker pool
static/           # Embedded HTML/CSS for the index page
```

### Environment Variables

| Variable | Required | Description |
|----------|----------|-------------|
| `DATABASE_URL` | Yes | PostgreSQL connection string |
| `WARERA_API_KEY` | Scraper | War Era API key (scraper only) |
| `GATEWAY_ADMIN_API_KEY` | Gateway | Required for `/api/stats` when public stats are disabled. Send it as `X-Gateway-Admin-Key`. |
| `GATEWAY_CORS_ALLOWED_ORIGINS` | Optional | Comma-separated allowed CORS origins. Empty disables CORS headers. |
| `GATEWAY_ENABLE_PUBLIC_STATS` | Optional | Set to `true` only for deliberate public/dev exposure of `/`, `/stats`, `/static/*` and unauthenticated `/api/stats`. |

### Runtime Safety Defaults

- tRPC input is capped at 1 MiB and one request may include at most 50 methods.
- The HTTP server uses header, read, write and idle timeouts and throttles
  concurrent handlers.
- Upstream War Era calls use request context cancellation, a 10s timeout and a
  2 MiB response body cap.
- CORS is disabled unless explicit allowed origins are configured.
- Public stats pages are disabled by default. `/api/stats` requires
  `X-Gateway-Admin-Key` unless public stats are deliberately enabled.
- Cache entries are scoped by API-key fingerprint. If a configured cache key
  field is missing or not a scalar, the Gateway bypasses cache instead of using
  a method-wide key.

## How It Works

1. Incoming request hits the gateway
2. If the response is cached, return immediately
3. If the data exists in the local database, return that
4. Otherwise, batch all requests within a 400ms window
5. Send one optimized request to the War Era API
6. Cache and store the response

Identical requests (same method + input) are deduplicated within each batch.

## License

See [LICENSE](LICENSE).
