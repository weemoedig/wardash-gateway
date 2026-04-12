package scraper

import (
	"encoding/json"
	"net/http"
	"os"
	"time"
)

const defaultBaseURL = "https://api2.warera.io/trpc/"

type Scraper struct {
	client       http.Client
	gb           *GlobalBatcher
	baseURL      string
	apiKey       string
	flushTimeout *time.Duration
}

type Option func(*Scraper)

type batchCall struct {
	method  string
	input   json.RawMessage
	process func(raw json.RawMessage) error
}

func WithFlushTimeout(timeout time.Duration) Option {
	return func(s *Scraper) {
		s.flushTimeout = &timeout
	}
}

func WithBaseURL(baseURL string) Option {
	return func(s *Scraper) {
		s.baseURL = baseURL
	}
}

func NewScraper(opts ...Option) *Scraper {
	s := &Scraper{
		client:  http.Client{},
		baseURL: defaultBaseURL,
		apiKey:  os.Getenv("WARERA_API_KEY"),
	}
	for _, opt := range opts {
		opt(s)
	}
	s.gb = newGlobalBatcher(s)
	return s
}
