# War Era Gateway

Caching proxy and request batcher for the [War Era](https://warera.io/) API. Batches requests within a 400ms window, caches responses, and stores paginated data locally in PostgreSQL.

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

### WarDash transaction runtime

WarDash production uses `SCRAPER_DATASETS=transactions`,
`SCRAPER_INTERVAL_SECONDS=60`, `SCRAPER_REQUESTS_PER_MINUTE=30`,
`TRANSACTION_RETENTION_DAYS=30` and
`GATEWAY_TRANSACTION_LOCAL_ONLY=true`. Incremental cycles ingest new
transactions first; the bounded historical backfill then advances by at most 24
pages per cycle and persists its cursor in the configured data directory.
Restarts resume that cursor instead of restarting the import. See `env.example`
for backward-compatible defaults.
