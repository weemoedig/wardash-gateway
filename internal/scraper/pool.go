package scraper

import (
	"errors"
	"log/slog"
	"sync"
	"time"
)

const (
	poolCleanupInterval = 60 * time.Second
	poolIdleTimeout     = 5 * time.Minute
	maxPoolEntries      = 1024
)

var ErrPoolFull = errors.New("scraper pool is full")

type poolEntry struct {
	scraper  *Scraper
	lastUsed time.Time
}

type ScraperPool struct {
	mu         sync.Mutex
	entries    map[string]*poolEntry
	opts       []Option
	stop       chan struct{}
	maxEntries int
}

func NewPool(opts ...Option) *ScraperPool {
	p := &ScraperPool{
		entries:    make(map[string]*poolEntry),
		opts:       opts,
		stop:       make(chan struct{}),
		maxEntries: maxPoolEntries,
	}
	go p.cleanupLoop()
	return p
}

func (p *ScraperPool) Get(apiKey string) (*Scraper, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	entry, ok := p.entries[apiKey]
	if ok {
		entry.lastUsed = time.Now()
		return entry.scraper, nil
	}

	if len(p.entries) >= p.maxEntries {
		p.evictIdleLocked(time.Now())
	}
	if len(p.entries) >= p.maxEntries {
		return nil, ErrPoolFull
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
	return s, nil
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

	p.evictIdleLocked(time.Now())
}

func (p *ScraperPool) evictIdleLocked(now time.Time) {
	for key, entry := range p.entries {
		if now.Sub(entry.lastUsed) > poolIdleTimeout {
			slog.Info("Evicting idle scraper", "pool_size", len(p.entries)-1)
			entry.scraper.Close()
			delete(p.entries, key)
		}
	}
}
