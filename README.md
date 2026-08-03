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
`GATEWAY_TRANSACTION_LOCAL_ONLY=true`. Local transaction reads require a
dedicated `GATEWAY_TRANSACTION_READ_API_KEY` through
`X-Gateway-Transaction-Key`; ordinary WarEra `X-API-Key` credentials are
rejected for this method and the transaction secret is never forwarded to
WarEra. Incremental cycles ingest new
transactions first; the bounded historical backfill then advances by at most 24
pages per cycle and persists its cursor in the configured data directory.
Restarts resume that cursor instead of restarting the import. See `env.example`
for backward-compatible defaults.

### WarDash market rollups

The transaction scraper maintains `market_daily_rollups` for the
`trading` and `itemMarket` transaction types. Transaction inserts and their
rollup increments commit atomically, while reconciliation at startup, daily,
whenever the reliable upper calendar window advances, and after each
successful backfill advance rebuilds the proven retained calendar window from
the transaction store. Calendar days use `Europe/Brussels`.

WarDash workers can read at most 30 reliable days through:

```text
GET /api/market/daily?days=30
X-Gateway-Market-Key: <GATEWAY_MARKET_READ_API_KEY>
```

The gateway and scraper must share `DATA_DIR` so the endpoint can read the
backfill coverage recorded in `transaction-scraper-state.json`. Decimal totals
remain JSON strings. The endpoint is disabled with a `404` when the dedicated
market read key is not configured.
