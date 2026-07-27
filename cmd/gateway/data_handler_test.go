package main

import (
	"context"
	"encoding/json"
	"testing"

	"gorm.io/gorm"
)

func TestWithFallbackTreatsLastPageWithItemsAsDatabaseHit(t *testing.T) {
	stats := NewStats(t.TempDir()+"/stats.json", allowedMethods)
	dbCalls := 0
	resp := json.RawMessage(`{"result":{"data":{"items":[{"id":1}],"nextCursor":""}}}`)

	got, err := withFallback(
		context.Background(),
		stats,
		nil,
		nil,
		"event.getEventsPaginated",
		json.RawMessage(`{"limit":100}`),
		func() (json.RawMessage, error) {
			dbCalls++
			return resp, nil
		},
		func(*gorm.DB, json.RawMessage) error {
			t.Fatal("upsert should not run when the database has a valid final page")
			return nil
		},
	)
	if err != nil {
		t.Fatalf("withFallback returned error: %v", err)
	}
	if string(got) != string(resp) {
		t.Fatalf("response = %s, want %s", got, resp)
	}
	if dbCalls != 1 {
		t.Fatalf("db calls = %d, want 1", dbCalls)
	}
}
