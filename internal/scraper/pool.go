package scraper

import (
	"log/slog"
	"sync"
	"time"
)

const (
	poolCleanupInterval = 60 * time.Second
	poolIdleTimeout     = 5 * time.Minute
)

type poolEntry struct {
	scraper  *Scraper
	lastUsed time.Time
}

type ScraperPool struct {
	mu      sync.Mutex
	entries map[string]*poolEntry
	opts    []Option
	stop    chan struct{}
}

func NewPool(opts ...Option) *ScraperPool {
	p := &ScraperPool{
		entries: make(map[string]*poolEntry),
		opts:    opts,
		stop:    make(chan struct{}),
	}
	go p.cleanupLoop()
	return p
}

func (p *ScraperPool) Get(apiKey string) *Scraper {
	p.mu.Lock()
	defer p.mu.Unlock()

	entry, ok := p.entries[apiKey]
	if ok {
		entry.lastUsed = time.Now()
		return entry.scraper
	}

	opts := make([]Option, len(p.opts)+1)
	copy(opts, p.opts)
	opts[len(p.opts)] = WithAPIKey(apiKey)

	s := NewScraper(opts...)
	p.entries[apiKey] = &poolEntry{
		scraper:  s,
		lastUsed: time.Now(),
	}

	slog.Info("Created new scraper for API key", "pool_size", len(p.entries))
	return s
}

func (p *ScraperPool) Close() {
	close(p.stop)

	p.mu.Lock()
	defer p.mu.Unlock()

	for key, entry := range p.entries {
		entry.scraper.Close()
		delete(p.entries, key)
	}
}

func (p *ScraperPool) cleanupLoop() {
	ticker := time.NewTicker(poolCleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-p.stop:
			return
		case <-ticker.C:
			p.evictIdle()
		}
	}
}

func (p *ScraperPool) evictIdle() {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()
	for key, entry := range p.entries {
		if now.Sub(entry.lastUsed) > poolIdleTimeout {
			slog.Info("Evicting idle scraper", "pool_size", len(p.entries)-1)
			entry.scraper.Close()
			delete(p.entries, key)
		}
	}
}
