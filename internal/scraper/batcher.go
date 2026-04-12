package scraper

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const urlSizeThreshold = 15_000

type Future[T any] struct {
	val     T
	err     error
	batchCh chan error
}

func (f *Future[T]) Wait() (T, error) {
	batchErr := <-f.batchCh
	if batchErr != nil && f.err == nil {
		f.err = batchErr
	}
	return f.val, f.err
}

type pendingCall struct {
	call    batchCall
	batchCh chan error
}

type GlobalBatcher struct {
	s      *Scraper
	submit chan pendingCall
	stop   chan struct{}
}

func newGlobalBatcher(s *Scraper) *GlobalBatcher {
	gb := &GlobalBatcher{
		s:      s,
		submit: make(chan pendingCall, 512),
		stop:   make(chan struct{}),
	}
	go gb.loop()
	return gb
}

func (gb *GlobalBatcher) Close() {
	close(gb.stop)
}

func doGlobal(gb *GlobalBatcher, method string, input json.RawMessage) (json.RawMessage, error) {
	f := addGlobal(gb, method, input)
	return f.Wait()
}

func addGlobal(gb *GlobalBatcher, method string, input json.RawMessage) *Future[json.RawMessage] {
	f := &Future[json.RawMessage]{batchCh: make(chan error, 1)}

	gb.submit <- pendingCall{
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

	return f
}

func (gb *GlobalBatcher) loop() {
	var (
		pending []pendingCall
		urlLen  int
	)

	var timer *time.Timer
	var timerC <-chan time.Time
	if gb.s.flushTimeout != nil {
		timer = time.NewTimer(*gb.s.flushTimeout)
		timer.Stop()
		timerC = timer.C
	}

	baseLen := len(gb.s.baseURL) + len("?batch=1")
	urlLen = baseLen

	flush := func() {
		if len(pending) == 0 {
			return
		}
		batch := pending
		pending = nil
		urlLen = baseLen
		go gb.executePending(batch)
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

			addLen := len(call.call.method)
			if len(pending) > 0 {
				addLen++
			}

			if len(pending) > 0 && urlLen+addLen > urlSizeThreshold {
				stopTimer()
				flush()
			}

			if len(pending) > 0 {
				urlLen++
			}
			urlLen += len(call.call.method)
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

	if gb.s.flushTimeout != nil {
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

	req, err := http.NewRequest("POST", reqURL, bodyReader)
	if err != nil {
		slog.Error("Failed creating request", "url", reqURL, "error", err)
		signalAll(pending, err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", gb.s.apiKey)

	err = gb.s.limiter.Wait(context.Background())
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
		body, _ := io.ReadAll(res.Body)
		slog.Error("Received fail response from request", "status_code", res.StatusCode, "body", string(body))
		signalAll(pending, fmt.Errorf("http %d: %s", res.StatusCode, string(body)))
		return
	}

	body, err := io.ReadAll(res.Body)
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

func signalAll(pending []pendingCall, err error) {
	for _, p := range pending {
		p.batchCh <- err
	}
}
