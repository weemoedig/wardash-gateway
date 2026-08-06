package scraper

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
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

func TestUpstreamHTTPErrorDoesNotExposeResponseBody(t *testing.T) {
	const sensitiveBody = `{"token":"private-upstream-response-4f5b"}`

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, sensitiveBody, http.StatusBadGateway)
	}))
	t.Cleanup(server.Close)

	logs := captureDefaultLogger(t)
	s := NewScraper(
		WithBaseURL(server.URL+"/"),
		WithAPIKey("test-api-key"),
	)
	t.Cleanup(s.Close)

	_, err := s.Request(context.Background(), "user.getUserById", nil)
	if err == nil {
		t.Fatal("expected upstream HTTP error")
	}
	if !strings.Contains(err.Error(), "upstream returned HTTP status 502") {
		t.Fatalf("error = %q, want redacted upstream status", err)
	}
	if strings.Contains(err.Error(), sensitiveBody) {
		t.Fatalf("error exposed upstream response body: %q", err)
	}
	if strings.Contains(logs.String(), sensitiveBody) {
		t.Fatalf("logs exposed upstream response body: %s", logs.String())
	}
}

func TestInvalidUpstreamJSONDoesNotExposeResponseBody(t *testing.T) {
	const sensitiveBody = "private-upstream-response-9c2d"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(sensitiveBody))
	}))
	t.Cleanup(server.Close)

	logs := captureDefaultLogger(t)
	s := NewScraper(
		WithBaseURL(server.URL+"/"),
		WithAPIKey("test-api-key"),
	)
	t.Cleanup(s.Close)

	_, err := s.Request(context.Background(), "user.getUserById", nil)
	if err == nil {
		t.Fatal("expected invalid JSON error")
	}
	if strings.Contains(err.Error(), sensitiveBody) {
		t.Fatalf("error exposed invalid upstream response body: %q", err)
	}
	if strings.Contains(logs.String(), sensitiveBody) {
		t.Fatalf("logs exposed invalid upstream response body: %s", logs.String())
	}
}

func TestRequestRateLimitCanUseConservativeSingleRequestBurst(t *testing.T) {
	s := NewScraper(WithRequestRateLimit(30, 1))
	defer s.Close()

	if got := s.limiter.Burst(); got != 1 {
		t.Fatalf("burst = %d, want 1", got)
	}
	if got := float64(s.limiter.Limit()); math.Abs(got-0.5) > 0.0001 {
		t.Fatalf("requests per second = %f, want 0.5", got)
	}
}

func captureDefaultLogger(t *testing.T) *bytes.Buffer {
	t.Helper()

	previous := slog.Default()
	logs := &bytes.Buffer{}
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	t.Cleanup(func() {
		slog.SetDefault(previous)
	})
	return logs
}
