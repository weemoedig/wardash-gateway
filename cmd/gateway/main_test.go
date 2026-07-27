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
