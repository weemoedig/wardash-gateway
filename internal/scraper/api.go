package scraper

import (
	"context"
	"encoding/json"
	"log/slog"
)

func (s *Scraper) Request(
	ctx context.Context,
	method string,
	input json.RawMessage,
) (json.RawMessage, error) {
	res, err := doGlobal(ctx, s.gb, method, input)
	if err != nil {
		slog.Error("Failed batching request to War Era API", "error", err)
	}
	return res, err
}
