package api

import (
	"encoding/json"
	"net/http"
	"os"
	"time"
)

const defaultBaseURL = "https://api2.warera.io/trpc/"

type Scraper struct {
	client       http.Client
	baseURL      string
	apiKey       string
	flushTimeout *time.Duration
}

type Option func(*Scraper)

type batchCall struct {
	method string
	input  json.RawMessage
}

type batchBuilder struct {
	s     *Scraper
	calls []batchCall
}

func WithFlushTimeout(timeout time.Duration) Option {
	return func(s *Scraper) {
		s.flushTimeout = &timeout
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
	return s
}

func (s *Scraper) NewBatch() *batchBuilder {
	return &batchBuilder{s: s}
}
