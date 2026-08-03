package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAPIKeyMiddlewareRejectsBlankAndOversizedKeys(t *testing.T) {
	next := apiKeyMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := apiKeyFromContext(r.Context()); got != "warera-key" {
			t.Fatalf("context key = %q, want trimmed key", got)
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	t.Run("trims key", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/trpc/user.getUserById", nil)
		req.Header.Set("X-API-Key", "  warera-key  ")
		rec := httptest.NewRecorder()

		next.ServeHTTP(rec, req)

		if rec.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusNoContent)
		}
	})

	t.Run("rejects oversized key", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/trpc/user.getUserById", nil)
		req.Header.Set("X-API-Key", strings.Repeat("x", maxAPIKeyBytes+1))
		rec := httptest.NewRecorder()

		next.ServeHTTP(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
		}
	})
}

func TestServiceDisablesPublicPagesAndStatsByDefault(t *testing.T) {
	t.Setenv(gatewayAdminAPIKeyEnv, "")
	t.Setenv(gatewayCORSAllowedOriginsEnv, "")
	t.Setenv(gatewayEnablePublicStatsPagesEnv, "")

	handler := service(nil, NewStats(t.TempDir()+"/stats.json", allowedMethods))

	for _, path := range []string{"/", "/stats", "/static/style.css", "/api/stats"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()

		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s status = %d, want %d", path, rec.Code, http.StatusNotFound)
		}
	}
}

func TestStatsRequiresAdminKeyWhenPublicStatsAreDisabled(t *testing.T) {
	t.Setenv(gatewayAdminAPIKeyEnv, "admin-secret")
	t.Setenv(gatewayCORSAllowedOriginsEnv, "")
	t.Setenv(gatewayEnablePublicStatsPagesEnv, "")

	handler := service(nil, NewStats(t.TempDir()+"/stats.json", allowedMethods))

	req := httptest.NewRequest(http.MethodGet, "/api/stats", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing key status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/stats", nil)
	req.Header.Set("X-Gateway-Admin-Key", "admin-secret")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin key status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestPublicStatsFlagIntentionallyExposesStatsPages(t *testing.T) {
	t.Setenv(gatewayAdminAPIKeyEnv, "")
	t.Setenv(gatewayCORSAllowedOriginsEnv, "")
	t.Setenv(gatewayEnablePublicStatsPagesEnv, "true")

	handler := service(nil, NewStats(t.TempDir()+"/stats.json", allowedMethods))

	for _, path := range []string{"/", "/stats", "/api/stats"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()

		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d, want %d", path, rec.Code, http.StatusOK)
		}
	}
}

func TestCORSIsDisabledUnlessAllowedOriginsAreConfigured(t *testing.T) {
	t.Setenv(gatewayAdminAPIKeyEnv, "admin-secret")
	t.Setenv(gatewayEnablePublicStatsPagesEnv, "")

	t.Run("default disabled", func(t *testing.T) {
		t.Setenv(gatewayCORSAllowedOriginsEnv, "")
		handler := service(nil, NewStats(t.TempDir()+"/stats.json", allowedMethods))
		req := httptest.NewRequest(http.MethodGet, "/api/stats", nil)
		req.Header.Set("Origin", "https://example.com")
		req.Header.Set("X-Gateway-Admin-Key", "admin-secret")
		rec := httptest.NewRecorder()

		handler.ServeHTTP(rec, req)

		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Fatalf("Access-Control-Allow-Origin = %q, want empty", got)
		}
	})

	t.Run("explicit origin", func(t *testing.T) {
		t.Setenv(gatewayCORSAllowedOriginsEnv, "https://example.com")
		handler := service(nil, NewStats(t.TempDir()+"/stats.json", allowedMethods))
		req := httptest.NewRequest(http.MethodGet, "/api/stats", nil)
		req.Header.Set("Origin", "https://example.com")
		req.Header.Set("X-Gateway-Admin-Key", "admin-secret")
		rec := httptest.NewRecorder()

		handler.ServeHTTP(rec, req)

		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://example.com" {
			t.Fatalf("Access-Control-Allow-Origin = %q, want explicit origin", got)
		}
	})
}

func TestTransactionLocalOnlyRequiresExplicitConfiguration(t *testing.T) {
	t.Run("disabled by default", func(t *testing.T) {
		t.Setenv(gatewayTransactionLocalOnlyEnv, "")

		if loadServiceConfig().transactionLocalOnly {
			t.Fatal("transactionLocalOnly = true, want false")
		}
	})

	t.Run("enabled explicitly", func(t *testing.T) {
		t.Setenv(gatewayTransactionLocalOnlyEnv, "true")

		if !loadServiceConfig().transactionLocalOnly {
			t.Fatal("transactionLocalOnly = false, want true")
		}
	})
}

func TestTransactionReadCredentialIsIsolatedFromUpstreamCredentials(t *testing.T) {
	next := transactionReadKeyMiddleware("transaction-secret", true)(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			credential := trpcCredentialFromContext(r.Context())
			if credential.kind != trpcCredentialTransaction {
				t.Fatalf("credential kind = %q, want transaction", credential.kind)
			}
			if credential.upstreamKey != "" || apiKeyFromContext(r.Context()) != "" {
				t.Fatal("transaction secret was exposed as an upstream credential")
			}
			w.WriteHeader(http.StatusNoContent)
		}),
	)

	req := httptest.NewRequest(
		http.MethodGet,
		"/trpc/transaction.getPaginatedTransactions",
		nil,
	)
	req.Header.Set("X-Gateway-Transaction-Key", "transaction-secret")
	rec := httptest.NewRecorder()

	next.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNoContent)
	}
}

func TestTransactionReadCredentialRejectsAmbiguousOrDisabledUse(t *testing.T) {
	tests := []struct {
		localOnly bool
		headers   map[string]string
		status    int
		name      string
	}{
		{
			name:      "disabled local reads stay hidden",
			localOnly: false,
			headers: map[string]string{
				"X-Gateway-Transaction-Key": "transaction-secret",
			},
			status: http.StatusNotFound,
		},
		{
			name:      "both credential headers are rejected",
			localOnly: true,
			headers: map[string]string{
				"X-Gateway-Transaction-Key": "transaction-secret",
				"X-API-Key":                 "upstream-secret",
			},
			status: http.StatusBadRequest,
		},
		{
			name:      "an explicitly blank upstream header is still ambiguous",
			localOnly: true,
			headers: map[string]string{
				"X-Gateway-Transaction-Key": "transaction-secret",
				"X-API-Key":                 "",
			},
			status: http.StatusBadRequest,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			next := transactionReadKeyMiddleware("transaction-secret", test.localOnly)(
				http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(http.StatusNoContent)
				}),
			)
			req := httptest.NewRequest(
				http.MethodGet,
				"/trpc/transaction.getPaginatedTransactions",
				nil,
			)
			for name, value := range test.headers {
				req.Header.Set(name, value)
			}
			rec := httptest.NewRecorder()

			next.ServeHTTP(rec, req)

			if rec.Code != test.status {
				t.Fatalf("status = %d, want %d", rec.Code, test.status)
			}
		})
	}
}

func TestGenericUpstreamCredentialCannotReadTransactions(t *testing.T) {
	handler := apiKeyMiddleware(trpc_handler(
		nil,
		nil,
		nil,
		NewStats(t.TempDir()+"/stats.json", allowedMethods),
		true,
	))
	req := httptest.NewRequest(
		http.MethodGet,
		"/trpc/transaction.getPaginatedTransactions?input=%7B%22limit%22%3A1%7D",
		nil,
	)
	req.Header.Set("X-API-Key", "upstream-secret")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
}

func TestMarketDailyRouteRequiresDedicatedReadKey(t *testing.T) {
	t.Setenv(gatewayAdminAPIKeyEnv, "admin-secret")
	t.Setenv(gatewayMarketReadAPIKeyEnv, "market-secret")
	t.Setenv(gatewayCORSAllowedOriginsEnv, "")
	t.Setenv(gatewayEnablePublicStatsPagesEnv, "")
	t.Setenv("DATA_DIR", t.TempDir())

	handler := service(nil, NewStats(t.TempDir()+"/stats.json", allowedMethods))

	tests := []struct {
		name    string
		headers map[string]string
		status  int
	}{
		{
			name:   "missing key",
			status: http.StatusUnauthorized,
		},
		{
			name: "admin key is not accepted",
			headers: map[string]string{
				"X-Gateway-Admin-Key": "admin-secret",
			},
			status: http.StatusUnauthorized,
		},
		{
			name: "generic upstream key is not accepted",
			headers: map[string]string{
				"X-API-Key": "market-secret",
			},
			status: http.StatusUnauthorized,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/market/daily?days=30", nil)
			for name, value := range test.headers {
				req.Header.Set(name, value)
			}
			rec := httptest.NewRecorder()

			handler.ServeHTTP(rec, req)

			if rec.Code != test.status {
				t.Fatalf("status = %d, want %d", rec.Code, test.status)
			}
		})
	}
}

func TestMarketDailyRouteIsHiddenWithoutConfiguredReadKey(t *testing.T) {
	t.Setenv(gatewayMarketReadAPIKeyEnv, "")
	t.Setenv(gatewayCORSAllowedOriginsEnv, "")
	t.Setenv(gatewayEnablePublicStatsPagesEnv, "")
	t.Setenv("DATA_DIR", t.TempDir())

	handler := service(nil, NewStats(t.TempDir()+"/stats.json", allowedMethods))
	req := httptest.NewRequest(http.MethodGet, "/api/market/daily?days=30", nil)
	req.Header.Set("X-Gateway-Market-Key", "anything")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}
