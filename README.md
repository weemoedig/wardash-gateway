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

