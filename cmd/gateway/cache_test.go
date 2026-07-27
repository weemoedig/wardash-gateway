package main

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	gocache "github.com/patrickmn/go-cache"
)

func TestCachedRequestBypassesCacheWhenKeyCannotBeDerived(t *testing.T) {
	c := gocache.New(time.Minute, time.Minute)
	calls := 0

	for i := 1; i <= 2; i++ {
		got, err := cachedRequest(c, "user.getUserById", json.RawMessage(`{}`), "api-key", func() (json.RawMessage, error) {
			calls++
			return json.RawMessage(fmt.Sprintf(`{"call":%d}`, calls)), nil
		})
		if err != nil {
			t.Fatalf("cachedRequest returned error: %v", err)
		}
		want := fmt.Sprintf(`{"call":%d}`, i)
		if string(got) != want {
			t.Fatalf("response %d = %s, want %s", i, got, want)
		}
	}

	if calls != 2 {
		t.Fatalf("fetch calls = %d, want 2", calls)
	}
}

func TestCachedRequestCachesNumericIDs(t *testing.T) {
	c := gocache.New(time.Minute, time.Minute)
	calls := 0

	for i := 0; i < 2; i++ {
		got, err := cachedRequest(c, "user.getUserById", json.RawMessage(`{"userId":123}`), "api-key", func() (json.RawMessage, error) {
			calls++
			return json.RawMessage(fmt.Sprintf(`{"call":%d}`, calls)), nil
		})
		if err != nil {
			t.Fatalf("cachedRequest returned error: %v", err)
		}
		if string(got) != `{"call":1}` {
			t.Fatalf("response = %s, want first cached response", got)
		}
	}

	if calls != 1 {
		t.Fatalf("fetch calls = %d, want 1", calls)
	}
}

func TestCachedRequestNormalizesMultiFieldJSONOrder(t *testing.T) {
	c := gocache.New(time.Minute, time.Minute)
	calls := 0

	inputs := []json.RawMessage{
		json.RawMessage(`{"battleId":1,"userId":2}`),
		json.RawMessage(`{"userId":2,"battleId":1}`),
	}

	for _, input := range inputs {
		got, err := cachedRequest(c, "battleLootSummary.getByBattleAndUser", input, "api-key", func() (json.RawMessage, error) {
			calls++
			return json.RawMessage(fmt.Sprintf(`{"call":%d}`, calls)), nil
		})
		if err != nil {
			t.Fatalf("cachedRequest returned error: %v", err)
		}
		if string(got) != `{"call":1}` {
			t.Fatalf("response = %s, want first cached response", got)
		}
	}

	if calls != 1 {
		t.Fatalf("fetch calls = %d, want 1", calls)
	}
}

func TestCachedRequestScopesCacheByCredential(t *testing.T) {
	c := gocache.New(time.Minute, time.Minute)
	calls := 0

	for _, apiKey := range []string{"api-key-a", "api-key-b"} {
		got, err := cachedRequest(c, "user.getUserById", json.RawMessage(`{"userId":"123"}`), apiKey, func() (json.RawMessage, error) {
			calls++
			return json.RawMessage(fmt.Sprintf(`{"call":%d}`, calls)), nil
		})
		if err != nil {
			t.Fatalf("cachedRequest returned error: %v", err)
		}
		want := fmt.Sprintf(`{"call":%d}`, calls)
		if string(got) != want {
			t.Fatalf("response = %s, want %s", got, want)
		}
	}

	if calls != 2 {
		t.Fatalf("fetch calls = %d, want 2", calls)
	}
}
