package scraper

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestRequestReturnsWhenContextIsCanceledBeforeFlush(t *testing.T) {
	flushTimeout := time.Hour
	s := NewScraper(WithFlushTimeout(&flushTimeout))
	defer s.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := s.Request(ctx, "user.getUserById", json.RawMessage(`{"userId":1}`))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

func TestReadLimitedRejectsOversizedResponse(t *testing.T) {
	_, err := readLimited(strings.NewReader(strings.Repeat("x", 64)), 16)
	if err == nil {
		t.Fatal("expected oversized response error")
	}
	if !strings.Contains(err.Error(), "response body exceeds 16 bytes") {
		t.Fatalf("error = %v, want response size error", err)
	}
}
