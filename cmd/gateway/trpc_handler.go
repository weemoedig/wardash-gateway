package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/Hattorius/War-Era-Gateway/internal/scraper"
	gocache "github.com/patrickmn/go-cache"
	"gorm.io/gorm"
)

const (
	maxTRPCInputBytes        int64 = 1 << 20
	maxTRPCMethodsPerRequest       = 50
)

var errTRPCInputTooLarge = errors.New("trpc input exceeds maximum size")

type TRPCRequest struct {
	Method string
	Input  json.RawMessage
}

var allowedMethodSet = func() map[string]struct{} {
	m := make(map[string]struct{}, len(allowedMethods))
	for _, method := range allowedMethods {
		m[method] = struct{}{}
	}
	return m
}()

func trpc_handler(pool *scraper.ScraperPool, c *gocache.Cache, db *gorm.DB, stats *Stats) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		methodsString, ok := strings.CutPrefix(r.URL.Path, "/trpc/")
		if !ok || methodsString == "" {
			http.Error(w, "invalid trpc path", http.StatusBadRequest)
			return
		}

		methods := strings.Split(methodsString, ",")
		if len(methods) == 0 {
			http.Error(w, "no methods provided", http.StatusBadRequest)
			return
		}
		if len(methods) > maxTRPCMethodsPerRequest {
			http.Error(w, "too many methods in one request", http.StatusBadRequest)
			return
		}

		for _, method := range methods {
			if method == "" {
				http.Error(w, "empty method name", http.StatusBadRequest)
				return
			}

			_, ok := allowedMethodSet[method]
			if !ok {
				http.Error(w, "unknown method: "+method, http.StatusBadRequest)
				return
			}
		}

		rawInput, err := readTRPCInput(w, r)
		if err != nil {
			status := http.StatusBadRequest
			if errors.Is(err, errTRPCInputTooLarge) {
				status = http.StatusRequestEntityTooLarge
			}
			http.Error(w, "failed to read input: "+err.Error(), status)
			return
		}

		requests, err := parseTRPCRequests(methods, rawInput)
		if err != nil {
			http.Error(w, "failed to parse input: "+err.Error(), http.StatusBadRequest)
			return
		}

		stats.RecordRequest()

		ctx := r.Context()
		apiKey := apiKeyFromContext(ctx)
		s, err := pool.Get(apiKey)
		if err != nil {
			http.Error(w, "gateway request pool is unavailable", http.StatusServiceUnavailable)
			return
		}
		responses := make([]json.RawMessage, len(requests))
		for i, request := range requests {
			response, err := data_handler(ctx, c, stats, s, db, request.Method, request.Input, apiKey)
			if err != nil {
				slog.Error("Received error from War Era API!", "error", err, "method", request.Method)
				errResp, _ := json.Marshal(map[string]any{
					"error": map[string]any{
						"message": err.Error(),
						"code":    -32603,
						"data": map[string]any{
							"code":       "INTERNAL_SERVER_ERROR",
							"httpStatus": 500,
						},
					},
				})
				responses[i] = errResp
				continue
			}
			responses[i] = response
		}

		w.Header().Set("Content-Type", "application/json")
		if len(responses) == 1 {
			w.Write(responses[0])
		} else {
			json.NewEncoder(w).Encode(responses)
		}
	}
}

func readTRPCInput(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	input := r.URL.Query().Get("input")
	if input != "" {
		if int64(len(input)) > maxTRPCInputBytes {
			return nil, errTRPCInputTooLarge
		}
		return []byte(input), nil
	}

	if r.Body == nil {
		return nil, nil
	}
	defer r.Body.Close()

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxTRPCInputBytes))
	if err != nil {
		if strings.Contains(err.Error(), "http: request body too large") {
			return nil, errTRPCInputTooLarge
		}
		return nil, err
	}

	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return nil, nil
	}

	return body, nil
}

func parseTRPCRequests(methods []string, rawInput []byte) ([]TRPCRequest, error) {
	reqs := make([]TRPCRequest, len(methods))
	for i, method := range methods {
		reqs[i] = TRPCRequest{
			Method: method,
			Input:  json.RawMessage("{}"), // empty object
		}
	}

	if len(bytes.TrimSpace(rawInput)) == 0 {
		return reqs, nil
	}

	if len(methods) == 1 {
		if !json.Valid(rawInput) {
			return nil, fmt.Errorf("single input is not valid JSON")
		}
		reqs[0].Input = json.RawMessage(rawInput)
		return reqs, nil
	}

	var indexed map[string]json.RawMessage
	err := json.Unmarshal(rawInput, &indexed)
	if err != nil {
		return nil, fmt.Errorf("multi-input must be an object keyed by method index: %w", err)
	}

	for k, v := range indexed {
		idx, err := strconv.Atoi(k)
		if err != nil {
			return nil, fmt.Errorf("invalid input index %q", k)
		}
		if idx < 0 || idx >= len(methods) {
			return nil, fmt.Errorf("input index %d out of range", idx)
		}
		reqs[idx].Input = v
	}

	return reqs, nil
}
