package scraper

import (
	"errors"
	"testing"
)

func TestScraperPoolRejectsNewKeysWhenFull(t *testing.T) {
	p := NewPool()
	defer p.Close()
	p.maxEntries = 1

	if _, err := p.Get("first-key"); err != nil {
		t.Fatalf("first Get returned error: %v", err)
	}
	if _, err := p.Get("first-key"); err != nil {
		t.Fatalf("existing key Get returned error: %v", err)
	}

	if _, err := p.Get("second-key"); !errors.Is(err, ErrPoolFull) {
		t.Fatalf("second key error = %v, want ErrPoolFull", err)
	}
}
