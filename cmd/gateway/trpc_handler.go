package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/Hattorius/War-Era-Gateway/internal/scraper"
	"gorm.io/gorm"
)

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

func trpc_handler(s *scraper.Scraper, db *gorm.DB) http.HandlerFunc {
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

		rawInput, err := readTRPCInput(r)
		if err != nil {
			http.Error(w, "failed to read input: "+err.Error(), http.StatusBadRequest)
			return
		}

		requests, err := parseTRPCRequests(methods, rawInput)
		if err != nil {
			http.Error(w, "failed to parse input: "+err.Error(), http.StatusBadRequest)
			return
		}

		slog.Info(
			"RETRIEVED REQUEST",
			"requests", requests,
		)

		ctx := r.Context()
		responses := make([]json.RawMessage, len(requests))
		for i, request := range requests {
			response, err := data_handler(ctx, s, db, request.Method, request.Input)
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

func readTRPCInput(r *http.Request) ([]byte, error) {
	input := r.URL.Query().Get("input")
	if input != "" {
		return []byte(input), nil
	}

	if r.Body == nil {
		return nil, nil
	}
	defer r.Body.Close()

	body, err := io.ReadAll(r.Body)
	if err != nil {
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
