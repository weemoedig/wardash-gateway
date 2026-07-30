package scraper

import (
	"encoding/json"
	"net/http"
	"os"
	"time"

	"golang.org/x/time/rate"
)

const (
	defaultBaseURL              = "https://api2.warera.io/trpc/"
	defaultUpstreamTimeout      = 10 * time.Second
	defaultMaxResponseBytes     = 2 * 1024 * 1024
	defaultMaxConcurrentBatches = 8
	defaultRequestsPerMinute    = 200
	defaultRequestBurst         = 200
)

type Scraper struct {
	client               http.Client
	gb                   *GlobalBatcher
	baseURL              string
	apiKey               string
	flushTimeout         *time.Duration
	limiter              *rate.Limiter
	onForward            func()
	upstreamTimeout      time.Duration
	maxResponseBytes     int64
	maxConcurrentBatches int
	requestsPerMinute    int
	requestBurst         int
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

func WithUpstreamTimeout(timeout time.Duration) Option {
	return func(s *Scraper) {
		s.upstreamTimeout = timeout
		s.client.Timeout = timeout
	}
}

func WithMaxResponseBytes(maxBytes int64) Option {
	return func(s *Scraper) {
		s.maxResponseBytes = maxBytes
	}
}

func WithMaxConcurrentBatches(maxConcurrent int) Option {
	return func(s *Scraper) {
		s.maxConcurrentBatches = maxConcurrent
	}
}

func WithRequestRateLimit(requestsPerMinute, burst int) Option {
	return func(s *Scraper) {
		s.requestsPerMinute = requestsPerMinute
		s.requestBurst = burst
	}
}

func (s *Scraper) Close() {
	s.gb.Close()
}

func NewScraper(opts ...Option) *Scraper {
	s := &Scraper{
		client:               http.Client{Timeout: defaultUpstreamTimeout},
		baseURL:              defaultBaseURL,
		apiKey:               os.Getenv("WARERA_API_KEY"),
		upstreamTimeout:      defaultUpstreamTimeout,
		maxResponseBytes:     defaultMaxResponseBytes,
		maxConcurrentBatches: defaultMaxConcurrentBatches,
		requestsPerMinute:    defaultRequestsPerMinute,
		requestBurst:         defaultRequestBurst,
	}
	for _, opt := range opts {
		opt(s)
	}
	if s.upstreamTimeout <= 0 {
		s.upstreamTimeout = defaultUpstreamTimeout
	}
	if s.client.Timeout <= 0 {
		s.client.Timeout = s.upstreamTimeout
	}
	if s.maxResponseBytes <= 0 {
		s.maxResponseBytes = defaultMaxResponseBytes
	}
	if s.maxConcurrentBatches <= 0 {
		s.maxConcurrentBatches = defaultMaxConcurrentBatches
	}
	if s.requestsPerMinute <= 0 {
		s.requestsPerMinute = defaultRequestsPerMinute
	}
	if s.requestBurst <= 0 {
		s.requestBurst = defaultRequestBurst
	}
	s.limiter = rate.NewLimiter(
		rate.Every(time.Minute/time.Duration(s.requestsPerMinute)),
		s.requestBurst,
	)
	s.gb = newGlobalBatcher(s)
	return s
}
