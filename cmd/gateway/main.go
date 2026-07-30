package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Hattorius/War-Era-Gateway/internal/database"
	"github.com/Hattorius/War-Era-Gateway/internal/scraper"
	"github.com/Hattorius/War-Era-Gateway/static"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
	gocache "github.com/patrickmn/go-cache"
	"gorm.io/gorm"
)

const (
	maxAPIKeyBytes                   = 512
	gatewayAdminAPIKeyEnv            = "GATEWAY_ADMIN_API_KEY"
	gatewayCORSAllowedOriginsEnv     = "GATEWAY_CORS_ALLOWED_ORIGINS"
	gatewayEnablePublicStatsPagesEnv = "GATEWAY_ENABLE_PUBLIC_STATS"
	gatewayTransactionLocalOnlyEnv   = "GATEWAY_TRANSACTION_LOCAL_ONLY"
)

type contextKey struct{}

func apiKeyFromContext(ctx context.Context) string {
	v, ok := ctx.Value(contextKey{}).(string)
	if ok {
		return v
	}
	return ""
}

func apiKeyFromHeader(r *http.Request, header string) (string, bool) {
	key := strings.TrimSpace(r.Header.Get(header))
	if key == "" || len(key) > maxAPIKeyBytes {
		return "", false
	}
	return key, true
}

func apiKeyMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key, ok := apiKeyFromHeader(r, "X-API-Key")
		if !ok {
			http.Error(w, "missing X-API-Key header", http.StatusUnauthorized)
			return
		}
		ctx := context.WithValue(r.Context(), contextKey{}, key)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func adminKeyMiddleware(adminKey string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if adminKey == "" {
				http.NotFound(w, r)
				return
			}

			key, ok := apiKeyFromHeader(r, "X-Gateway-Admin-Key")
			if !ok || subtle.ConstantTimeCompare([]byte(key), []byte(adminKey)) != 1 {
				http.Error(w, "missing or invalid X-Gateway-Admin-Key header", http.StatusUnauthorized)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

var allowedMethods = []string{
	"alliance.getById",
	"alliance.getManyPaginated",
	"company.getById",
	"company.getCompanies",
	"company.getProductionBonus",
	"company.getRecommendedRegionIdsByItemCode",
	"country.getCountryById",
	"country.getAllCountries",
	"event.getEventsPaginated",
	"government.getByCountryId",
	"region.getById",
	"region.getAll",
	"region.getRegionsObject",
	"battle.getById",
	"battle.getLiveBattleData",
	"battle.getBattles",
	"battleLootSummary.getByBattleAndUser",
	"round.getById",
	"round.getLastHits",
	"battleRanking.getRanking",
	"itemTrading.getPrices",
	"tradingOrder.getTopOrders",
	"itemOffer.getById",
	"workOffer.getById",
	"workOffer.getWorkOfferByCompanyId",
	"workOffer.getWorkOffersPaginated",
	"mercenaryContractAuction.getPaginatedAuctions",
	"party.getById",
	"party.getManyPaginated",
	"ranking.getRanking",
	"search.searchAnything",
	"search.searchMus",
	"search.searchUsers",
	"gameConfig.getDates",
	"gameConfig.getGameConfig",
	"user.getUserLite",
	"user.getUsersByCountry",
	"user.getUserById",
	"article.getArticleById",
	"article.getArticleLiteById",
	"article.getArticlesPaginated",
	"mu.getById",
	"mu.getManyPaginated",
	"transaction.getPaginatedTransactions",
	"tournament.getById",
	"tournament.getLastTournament",
	"tournamentTeam.getById",
	"tournamentTeam.getByTournamentId",
	"upgrade.getUpgradeByTypeAndEntity",
	"war.getById",
	"worker.getWorkers",
	"worker.getTotalWorkersCount",
	"battleOrder.getByBattle",
	"inventory.fetchCurrentEquipment",
}

func main() {
	slog.Info("Gateway starting")

	addr := os.Getenv("LISTEN")
	if addr == "" {
		addr = "0.0.0.0:8080"
	}

	dataDir := os.Getenv("DATA_DIR")
	if dataDir == "" {
		dataDir = "./data"
	}

	db, err := database.Connect()
	if err != nil {
		slog.Error("Failed to connect to database", "error", err)
		os.Exit(1)
	}

	stats := NewStats(dataDir+"/stats.json", allowedMethods)
	stopStats := make(chan struct{})
	go stats.Run(stopStats)

	server := &http.Server{
		Addr:              addr,
		Handler:           service(db, stats),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		slog.Info("Starting http server", "listen", addr)
		err := server.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("HTTP server died!", "error", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()

	close(stopStats)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	err = server.Shutdown(shutdownCtx)
	if err != nil {
		slog.Error("Failed gracefully shutting down server!", "error", err)
		os.Exit(1)
	}
}

type serviceConfig struct {
	adminAPIKey          string
	corsAllowedOrigins   []string
	publicStats          bool
	transactionLocalOnly bool
}

func loadServiceConfig() serviceConfig {
	return serviceConfig{
		adminAPIKey:          strings.TrimSpace(os.Getenv(gatewayAdminAPIKeyEnv)),
		corsAllowedOrigins:   splitCSV(os.Getenv(gatewayCORSAllowedOriginsEnv)),
		publicStats:          parseBoolEnv(os.Getenv(gatewayEnablePublicStatsPagesEnv)),
		transactionLocalOnly: parseBoolEnv(os.Getenv(gatewayTransactionLocalOnlyEnv)),
	}
}

func splitCSV(raw string) []string {
	fields := strings.Split(raw, ",")
	values := make([]string, 0, len(fields))
	for _, field := range fields {
		value := strings.TrimSpace(field)
		if value != "" {
			values = append(values, value)
		}
	}
	return values
}

func parseBoolEnv(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func service(db *gorm.DB, stats *Stats) http.Handler {
	cfg := loadServiceConfig()
	flushTimeout := time.Millisecond * 400
	pool := scraper.NewPool(
		scraper.WithFlushTimeout(&flushTimeout),
		scraper.WithOnForward(stats.RecordForwarded),
	)
	c := gocache.New(5*time.Minute, 10*time.Minute)

	r := chi.NewRouter()

	if len(cfg.corsAllowedOrigins) > 0 {
		r.Use(cors.Handler(cors.Options{
			AllowedOrigins: cfg.corsAllowedOrigins,
			AllowedMethods: []string{"GET", "POST", "OPTIONS"},
			AllowedHeaders: []string{
				"Content-Type",
				"X-API-Key",
				"X-Gateway-Admin-Key",
			},
			AllowCredentials: false,
			MaxAge:           300,
		}))
	}

	r.Use(middleware.Logger)
	r.Use(middleware.RequestID)
	r.Use(middleware.CleanPath)
	r.Use(middleware.Throttle(128))

	trpc_handler := trpc_handler(pool, c, db, stats, cfg.transactionLocalOnly)
	r.With(apiKeyMiddleware).Method(http.MethodGet, "/trpc/*", trpc_handler)
	r.With(apiKeyMiddleware).Method(http.MethodPost, "/trpc/*", trpc_handler)

	if cfg.publicStats {
		r.Get("/api/stats", stats.HTTPHandler())
	} else {
		r.With(adminKeyMiddleware(cfg.adminAPIKey)).Get("/api/stats", stats.HTTPHandler())
	}

	if cfg.publicStats {
		staticSub, _ := fs.Sub(static.Files, ".")
		staticFS := http.FileServer(http.FS(staticSub))
		r.Handle("/static/*", http.StripPrefix("/static/", staticFS))
		r.Get("/", func(w http.ResponseWriter, r *http.Request) {
			index, _ := static.Files.ReadFile("index.html")
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Write(index)
		})

		r.Get("/stats", func(w http.ResponseWriter, r *http.Request) {
			page, _ := static.Files.ReadFile("stats.html")
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Write(page)
		})
	}

	return r
}
