package scraper

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const maxBatchSize = 50
const maxLoggedErrorBodyBytes int64 = 4096

var errBatcherClosed = errors.New("batcher is closed")

type Future[T any] struct {
	val     T
	err     error
	batchCh chan error
}

func (f *Future[T]) Wait(ctx context.Context) (T, error) {
	select {
	case batchErr := <-f.batchCh:
		if batchErr != nil && f.err == nil {
			f.err = batchErr
		}
		return f.val, f.err
	case <-ctx.Done():
		var zero T
		return zero, ctx.Err()
	}
}

type pendingCall struct {
	ctx     context.Context
	call    batchCall
	batchCh chan error
}

type GlobalBatcher struct {
	s          *Scraper
	submit     chan pendingCall
	stop       chan struct{}
	executeSem chan struct{}
}

func newGlobalBatcher(s *Scraper) *GlobalBatcher {
	gb := &GlobalBatcher{
		s:          s,
		submit:     make(chan pendingCall, 512),
		stop:       make(chan struct{}),
		executeSem: make(chan struct{}, s.maxConcurrentBatches),
	}
	go gb.loop()
	return gb
}

func (gb *GlobalBatcher) Close() {
	close(gb.stop)
}

func doGlobal(ctx context.Context, gb *GlobalBatcher, method string, input json.RawMessage) (json.RawMessage, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	f, err := addGlobal(ctx, gb, method, input)
	if err != nil {
		return nil, err
	}
	return f.Wait(ctx)
}

func addGlobal(ctx context.Context, gb *GlobalBatcher, method string, input json.RawMessage) (*Future[json.RawMessage], error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	f := &Future[json.RawMessage]{batchCh: make(chan error, 1)}

	pending := pendingCall{
		ctx: ctx,
		call: batchCall{
			method: method,
			input:  input,
			process: func(raw json.RawMessage) error {
				f.val = raw
				return nil
			},
		},
		batchCh: f.batchCh,
	}

	select {
	case gb.submit <- pending:
		return f, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-gb.stop:
		return nil, errBatcherClosed
	}
}

func (gb *GlobalBatcher) loop() {
	var pending []pendingCall

	var timer *time.Timer
	var timerC <-chan time.Time
	if gb.s.flushTimeout != nil {
		timer = time.NewTimer(*gb.s.flushTimeout)
		timer.Stop()
		timerC = timer.C
	}

	flush := func() {
		if len(pending) == 0 {
			return
		}
		batch := pending
		pending = nil
		select {
		case gb.executeSem <- struct{}{}:
			go func() {
				defer func() { <-gb.executeSem }()
				gb.executePending(batch)
			}()
		case <-gb.stop:
			signalAll(batch, errBatcherClosed)
		}
	}

	stopTimer := func() {
		if timer == nil {
			return
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	}

	for {
		select {
		case <-gb.stop:
			stopTimer()
			flush()
			return
		case call := <-gb.submit:
			if gb.s.flushTimeout == nil {
				pending = append(pending, call)
				flush()
				continue
			}

			if len(pending) >= maxBatchSize {
				stopTimer()
				flush()
			}

			pending = append(pending, call)

			if len(pending) == 1 {
				timer.Reset(*gb.s.flushTimeout)
			}

		case <-timerC:
			flush()
		}
	}
}

type dedupKey struct {
	method string
	input  string
}

type uniqueCall struct {
	method  string
	input   json.RawMessage
	indices []int
}

func (gb *GlobalBatcher) executePending(pending []pendingCall) {
	pending = activePending(pending)
	if len(pending) == 0 {
		return
	}

	seen := make(map[dedupKey]int)
	var unique []uniqueCall

	for i, p := range pending {
		key := dedupKey{method: p.call.method, input: string(p.call.input)}
		if idx, ok := seen[key]; ok {
			unique[idx].indices = append(unique[idx].indices, i)
		} else {
			seen[key] = len(unique)
			unique = append(unique, uniqueCall{
				method:  p.call.method,
				input:   p.call.input,
				indices: []int{i},
			})
		}
	}

	if gb.s.flushTimeout != nil && len(pending) > 1 {
		slog.Info("Batch dedup", "pending", len(pending), "unique", len(unique))
	}

	methods := make([]string, 0, len(unique))
	for _, u := range unique {
		methods = append(methods, u.method)
	}
	reqURL := gb.s.baseURL + strings.Join(methods, ",") + "?batch=1"

	inputObj := make(map[string]json.RawMessage)
	for i, u := range unique {
		if u.input == nil {
			continue
		}
		d, err := json.Marshal(u.input)
		if err != nil {
			slog.Error("Failed to marshal JSON input", "index", i, "method", u.method, "error", err)
			signalAll(pending, err)
			return
		}
		inputObj[strconv.Itoa(i)] = d
	}

	var bodyReader *bytes.Reader
	if len(inputObj) > 0 {
		d, err := json.Marshal(inputObj)
		if err != nil {
			slog.Error("Failed to marshal batch input object", "error", err)
			signalAll(pending, err)
			return
		}
		bodyReader = bytes.NewReader(d)
	} else {
		bodyReader = bytes.NewReader([]byte("{}"))
	}

	reqCtx, cancelReq := batchRequestContext(pending, gb.s.upstreamTimeout)
	defer cancelReq()

	req, err := http.NewRequestWithContext(reqCtx, "POST", reqURL, bodyReader)
	if err != nil {
		slog.Error("Failed creating request", "url", reqURL, "error", err)
		signalAll(pending, err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", gb.s.apiKey)

	err = gb.s.limiter.Wait(reqCtx)
	if err != nil {
		slog.Error("Rate limiter error", "error", err)
		signalAll(pending, err)
		return
	}

	res, err := gb.s.client.Do(req)

	if err != nil {
		slog.Error("Failed making request", "url", reqURL, "error", err)
		signalAll(pending, err)
		return
	}
	defer res.Body.Close()

	if res.StatusCode < 200 || res.StatusCode > 299 {
		body, _ := readLimited(res.Body, maxLoggedErrorBodyBytes)
		slog.Error("Received fail response from request", "status_code", res.StatusCode, "body", string(body))
		signalAll(pending, fmt.Errorf("http %d: %s", res.StatusCode, string(body)))
		return
	}

	if gb.s.onForward != nil {
		gb.s.onForward()
	}

	body, err := readLimited(res.Body, gb.s.maxResponseBytes)
	if err != nil {
		slog.Error("Failed reading body", "error", err)
		signalAll(pending, err)
		return
	}

	var items []json.RawMessage
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		err = json.Unmarshal(body, &items)
	} else {
		var single json.RawMessage
		err = json.Unmarshal(body, &single)
		if err == nil {
			items = []json.RawMessage{single}
		}
	}
	if err != nil {
		slog.Error("Could not unmarshal batch response", "body", string(body), "error", err)
		signalAll(pending, err)
		return
	}

	if len(items) != len(unique) {
		slog.Error("Batch length mismatch", "got", len(items), "expected", len(unique))
		n := min(len(items), len(unique))
		for i := 0; i < n; i++ {
			for _, pendingIdx := range unique[i].indices {
				pending[pendingIdx].call.process(items[i])
				pending[pendingIdx].batchCh <- nil
			}
		}
		mismatchErr := fmt.Errorf("batch response length mismatch: got %d items, expected %d", len(items), len(unique))
		for i := n; i < len(unique); i++ {
			for _, pendingIdx := range unique[i].indices {
				pending[pendingIdx].batchCh <- mismatchErr
			}
		}
		return
	}

	for i, u := range unique {
		for _, pendingIdx := range u.indices {
			pending[pendingIdx].call.process(items[i])
			pending[pendingIdx].batchCh <- nil
		}
	}
}

func activePending(pending []pendingCall) []pendingCall {
	active := pending[:0]
	for _, p := range pending {
		if p.ctx == nil {
			p.ctx = context.Background()
		}
		if err := p.ctx.Err(); err != nil {
			p.batchCh <- err
			continue
		}
		active = append(active, p)
	}
	return active
}

func batchRequestContext(pending []pendingCall, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		timeout = defaultUpstreamTimeout
	}

	parents := make([]context.Context, 0, len(pending))
	for _, p := range pending {
		if p.ctx != nil && p.ctx.Err() == nil {
			parents = append(parents, p.ctx)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	if len(parents) == 0 {
		cancel()
		return ctx, cancel
	}

	var mu sync.Mutex
	remaining := len(parents)
	stops := make([]func() bool, 0, len(parents))

	for _, parent := range parents {
		stop := context.AfterFunc(parent, func() {
			mu.Lock()
			remaining--
			shouldCancel := remaining == 0
			mu.Unlock()
			if shouldCancel {
				cancel()
			}
		})
		stops = append(stops, stop)
	}

	return ctx, func() {
		for _, stop := range stops {
			stop()
		}
		cancel()
	}
}

func readLimited(r io.Reader, maxBytes int64) ([]byte, error) {
	if maxBytes <= 0 {
		maxBytes = defaultMaxResponseBytes
	}

	body, err := io.ReadAll(io.LimitReader(r, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > maxBytes {
		return nil, fmt.Errorf("response body exceeds %d bytes", maxBytes)
	}
	return body, nil
}

func signalAll(pending []pendingCall, err error) {
	for _, p := range pending {
		p.batchCh <- err
	}
}
