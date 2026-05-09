package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

type HourlySnapshot struct {
	Hour        time.Time        `json:"hour"`
	Received    int64            `json:"received"`
	Forwarded   int64            `json:"forwarded"`
	CacheMisses int64            `json:"cacheMisses"`
	Methods     map[string]int64 `json:"methods"`
}

type Stats struct {
	currentReceived    atomic.Int64
	currentForwarded   atomic.Int64
	currentCacheMisses atomic.Int64
	currentMethods     []atomic.Int64
	methodIndex        map[string]int
	methodNames        []string
	currentHour        time.Time

	mu       sync.RWMutex
	history  []HourlySnapshot
	filePath string
}

type statsFileData struct {
	History []HourlySnapshot `json:"history"`
}

func NewStats(filePath string, methods []string) *Stats {
	s := &Stats{
		methodIndex:    make(map[string]int, len(methods)),
		methodNames:    make([]string, len(methods)),
		currentMethods: make([]atomic.Int64, len(methods)),
		currentHour:    time.Now().UTC().Truncate(time.Hour),
		filePath:       filePath,
	}

	for i, m := range methods {
		s.methodIndex[m] = i
		s.methodNames[i] = m
	}

	s.loadFromDisk()
	return s
}

func (s *Stats) loadFromDisk() {
	data, err := os.ReadFile(s.filePath)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Error("Failed to read stats file", "error", err)
		}
		return
	}

	var file statsFileData
	if err := json.Unmarshal(data, &file); err != nil {
		slog.Error("Failed to parse stats file", "error", err)
		return
	}

	cutoff := time.Now().UTC().Add(-24 * time.Hour)
	s.history = make([]HourlySnapshot, 0, len(file.History))
	for _, snap := range file.History {
		if snap.Hour.After(cutoff) {
			s.history = append(s.history, snap)
		}
	}

	slog.Info("Loaded stats from disk", "snapshots", len(s.history))
}

func (s *Stats) saveToDisk() {
	dir := filepath.Dir(s.filePath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		slog.Error("Failed to create stats directory", "error", err)
		return
	}

	file := statsFileData{History: s.history}
	data, err := json.Marshal(file)
	if err != nil {
		slog.Error("Failed to marshal stats", "error", err)
		return
	}

	tmp := s.filePath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		slog.Error("Failed to write stats temp file", "error", err)
		return
	}

	if err := os.Rename(tmp, s.filePath); err != nil {
		slog.Error("Failed to rename stats file", "error", err)
		return
	}
}

func (s *Stats) RecordRequest() {
	s.currentReceived.Add(1)
}

func (s *Stats) RecordMethod(method string) {
	if idx, ok := s.methodIndex[method]; ok {
		s.currentMethods[idx].Add(1)
	}
}

func (s *Stats) RecordCacheMiss() {
	s.currentCacheMisses.Add(1)
}

func (s *Stats) RecordForwarded() {
	s.currentForwarded.Add(1)
}

func (s *Stats) snapshot() {
	methods := make(map[string]int64, len(s.methodNames))
	for i, name := range s.methodNames {
		v := s.currentMethods[i].Swap(0)
		if v > 0 {
			methods[name] = v
		}
	}

	snap := HourlySnapshot{
		Hour:        s.currentHour,
		Received:    s.currentReceived.Swap(0),
		Forwarded:   s.currentForwarded.Swap(0),
		CacheMisses: s.currentCacheMisses.Swap(0),
		Methods:     methods,
	}

	s.mu.Lock()
	s.history = append(s.history, snap)

	cutoff := time.Now().UTC().Add(-24 * time.Hour)
	start := 0
	for start < len(s.history) && !s.history[start].Hour.After(cutoff) {
		start++
	}
	if start > 0 {
		s.history = s.history[start:]
	}

	s.currentHour = time.Now().UTC().Truncate(time.Hour)
	s.saveToDisk()
	s.mu.Unlock()

	slog.Info("Stats snapshot saved", "hour", snap.Hour, "received", snap.Received, "forwarded", snap.Forwarded)
}

func (s *Stats) Run(stop <-chan struct{}) {
	for {
		nextHour := s.currentHour.Add(time.Hour)
		waitDuration := max(time.Until(nextHour), 0)

		timer := time.NewTimer(waitDuration)
		select {
		case <-stop:
			timer.Stop()
			return
		case <-timer.C:
			s.snapshot()
		}
	}
}

type methodCount struct {
	Method string `json:"method"`
	Count  int64  `json:"count"`
}

type statsSummary struct {
	TotalReceived    int64   `json:"totalReceived"`
	TotalForwarded   int64   `json:"totalForwarded"`
	TotalMethods     int64   `json:"totalMethods"`
	TotalCacheMisses int64   `json:"totalCacheMisses"`
	CacheHitRate     float64 `json:"cacheHitRate"`
	BatchEfficiency  float64 `json:"batchEfficiency"`
}

type statsResponse struct {
	Current    *HourlySnapshot  `json:"current"`
	History    []HourlySnapshot `json:"history"`
	TopMethods []methodCount    `json:"topMethods"`
	Summary    statsSummary     `json:"summary"`
}

func (s *Stats) buildResponse() statsResponse {
	currentMethods := make(map[string]int64, len(s.methodNames))
	for i, name := range s.methodNames {
		v := s.currentMethods[i].Load()
		if v > 0 {
			currentMethods[name] = v
		}
	}

	current := &HourlySnapshot{
		Hour:        s.currentHour,
		Received:    s.currentReceived.Load(),
		Forwarded:   s.currentForwarded.Load(),
		CacheMisses: s.currentCacheMisses.Load(),
		Methods:     currentMethods,
	}

	s.mu.RLock()
	history := make([]HourlySnapshot, len(s.history))
	copy(history, s.history)
	s.mu.RUnlock()

	var totalReceived, totalForwarded, totalCacheMisses, totalMethods int64
	totals := make(map[string]int64, len(s.methodNames))

	for _, snap := range history {
		totalReceived += snap.Received
		totalForwarded += snap.Forwarded
		totalCacheMisses += snap.CacheMisses
		for m, c := range snap.Methods {
			totals[m] += c
			totalMethods += c
		}
	}
	totalReceived += current.Received
	totalForwarded += current.Forwarded
	totalCacheMisses += current.CacheMisses
	for m, c := range currentMethods {
		totals[m] += c
		totalMethods += c
	}

	ranked := make([]methodCount, 0, len(totals))
	for m, c := range totals {
		ranked = append(ranked, methodCount{Method: m, Count: c})
	}
	sort.Slice(ranked, func(i, j int) bool {
		return ranked[i].Count > ranked[j].Count
	})
	if len(ranked) > 10 {
		ranked = ranked[:10]
	}

	summary := statsSummary{
		TotalReceived:    totalReceived,
		TotalForwarded:   totalForwarded,
		TotalMethods:     totalMethods,
		TotalCacheMisses: totalCacheMisses,
		CacheHitRate:     -1,
		BatchEfficiency:  -1,
	}
	if totalMethods > 0 {
		summary.CacheHitRate = 1.0 - float64(totalCacheMisses)/float64(totalMethods)
	}
	if totalCacheMisses > 0 {
		summary.BatchEfficiency = 1.0 - float64(totalForwarded)/float64(totalCacheMisses)
	}

	return statsResponse{
		Current:    current,
		History:    history,
		TopMethods: ranked,
		Summary:    summary,
	}
}

func (s *Stats) HTTPHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		resp := s.buildResponse()

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-cache")
		json.NewEncoder(w).Encode(resp)
	}
}
