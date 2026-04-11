package main

import (
	"context"
	"encoding/json"

	"github.com/Hattorius/War-Era-Gateway/internal/scraper"
)

func data_handler(
	ctx context.Context,
	s *scraper.Scraper,
	method string,
	input json.RawMessage,
) (json.RawMessage, error) {
	// do other stuff like caching check, database check, will be implemented later

	// if not database or cache hit
	return s.Request(ctx, method, input)
}
