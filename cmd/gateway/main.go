package main

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
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

type contextKey struct{}

func apiKeyFromContext(ctx context.Context) string {
	v, ok := ctx.Value(contextKey{}).(string)
	if ok {
		return v
	}
	return ""
}

func apiKeyMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("X-API-Key")
		if key == "" {
			http.Error(w, "missing X-API-Key header", http.StatusUnauthorized)
			return
		}
		ctx := context.WithValue(r.Context(), contextKey{}, key)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

var allowedMethods = []string{
	"company.getById",
	"company.getCompanies",
	"country.getCountryById",
	"country.getAllCountries",
	"event.getEventsPaginated",
	"government.getByCountryId",
	"region.getById",
	"region.getRegionsObject",
	"battle.getById",
	"battle.getLiveBattleData",
	"battle.getBattles",
	"round.getById",
	"round.getLastHits",
	"battleRanking.getRanking",
	"itemTrading.getPrices",
	"tradingOrder.getTopOrders",
	"itemOffer.getById",
	"workOffer.getById",
	"workOffer.getWorkOfferByCompanyId",
	"workOffer.getWorkOffersPaginated",
	"ranking.getRanking",
	"search.searchAnything",
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
	"upgrade.getUpgradeByTypeAndEntity",
	"worker.getWorkers",
	"worker.getTotalWorkersCount",
	"battleOrder.getByBattle",
	"inventory.fetchCurrentEquipment",
}

func main() {
	addr := os.Getenv("LISTEN")
	if addr == "" {
		addr = "0.0.0.0:8080"
	}

	db, err := database.Connect()
	if err != nil {
		slog.Error("Failed to connect to database", "error", err)
		os.Exit(1)
	}

	server := &http.Server{Addr: addr, Handler: service(db)}

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

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	err = server.Shutdown(shutdownCtx)
	if err != nil {
		slog.Error("Failed gracefully shutting down server!", "error", err)
		os.Exit(1)
	}
}

func service(db *gorm.DB) http.Handler {
	flushTimeout := time.Millisecond * 400
	pool := scraper.NewPool(scraper.WithFlushTimeout(&flushTimeout))
	c := gocache.New(5*time.Minute, 10*time.Minute)

	r := chi.NewRouter()

	r.Use(cors.Handler(cors.Options{
		AllowedOrigins:   []string{"https://*", "http://*"},
		AllowedMethods:   []string{"GET", "POST", "OPTIONS"},
		AllowedHeaders:   []string{"*"},
		AllowCredentials: false,
		MaxAge:           300,
	}))

	r.Use(middleware.Logger)
	r.Use(middleware.RequestID)
	r.Use(middleware.CleanPath)

	trpc_handler := trpc_handler(pool, c, db)
	r.With(apiKeyMiddleware).Method(http.MethodGet, "/trpc/*", trpc_handler)
	r.With(apiKeyMiddleware).Method(http.MethodPost, "/trpc/*", trpc_handler)

	staticSub, _ := fs.Sub(static.Files, ".")
	staticFS := http.FileServer(http.FS(staticSub))
	r.Handle("/static/*", http.StripPrefix("/static/", staticFS))
	r.Get("/", func(w http.ResponseWriter, r *http.Request) {
		index, _ := static.Files.ReadFile("index.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(index)
	})

	return r
}
