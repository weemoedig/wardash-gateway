package scraper

import (
	"encoding/json"
	"net/http"
	"os"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

const defaultBaseURL = "https://api2.warera.io/trpc/"

var rateLimitHeaders = []string{
	"Ratelimit-Limit",
	"Ratelimit-Policy",
	"Ratelimit-Remaining",
	"Ratelimit-Reset",
}

type RateLimitInfo struct {
	Headers http.Header
}

type Scraper struct {
	client       http.Client
	gb           *GlobalBatcher
	baseURL      string
	apiKey       string
	flushTimeout *time.Duration
	limiter      *rate.Limiter
	onForward    func()

	rlMu      sync.RWMutex
	rateLimit RateLimitInfo
}

type Option func(*Scraper)

type batchCall struct {
	method  string
	input   json.RawMessage
	process func(raw json.RawMessage) error
}

func WithFlushTimeout(timeout *time.Duration) Option {
	return func(s *Scraper) {
		s.flushTimeout = timeout
	}
}

func WithBaseURL(baseURL string) Option {
	return func(s *Scraper) {
		s.baseURL = baseURL
	}
}

func WithAPIKey(apiKey string) Option {
	return func(s *Scraper) {
		s.apiKey = apiKey
	}
}

func WithOnForward(fn func()) Option {
	return func(s *Scraper) {
		s.onForward = fn
	}
}

func (s *Scraper) Close() {
	s.gb.Close()
}

func NewScraper(opts ...Option) *Scraper {
	s := &Scraper{
		client:  http.Client{},
		baseURL: defaultBaseURL,
		apiKey:  os.Getenv("WARERA_API_KEY"),
		limiter: rate.NewLimiter(rate.Every(time.Minute/200), 200),
	}
	for _, opt := range opts {
		opt(s)
	}
	s.gb = newGlobalBatcher(s)
	return s
}

func (s *Scraper) storeRateLimitHeaders(h http.Header) {
	info := RateLimitInfo{Headers: make(http.Header)}
	for _, key := range rateLimitHeaders {
		if v := h.Get(key); v != "" {
			info.Headers.Set(key, v)
		}
	}
	s.rlMu.Lock()
	s.rateLimit = info
	s.rlMu.Unlock()
}

func (s *Scraper) GetRateLimitHeaders() http.Header {
	s.rlMu.RLock()
	defer s.rlMu.RUnlock()
	return s.rateLimit.Headers.Clone()
}
