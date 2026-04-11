package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Hattorius/War-Era-Gateway/internal/api"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
)

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
		addr = "127.0.0.1:8080"
	}

	server := &http.Server{Addr: addr, Handler: service()}

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

	err := server.Shutdown(shutdownCtx)
	if err != nil {
		slog.Error("Failed gracefully shutting down server!", "error", err)
		os.Exit(1)
	}
}

func service() http.Handler {
	s := api.NewScraper(api.WithFlushTimeout(time.Millisecond * 400))

	r := chi.NewRouter()

	r.Use(cors.Handler(cors.Options{
		AllowedOrigins:   []string{"https://*", "http://*"},
		AllowedMethods:   []string{"GET", "POST"},
		AllowedHeaders:   []string{"Content-Type", "X-API-Key"},
		AllowCredentials: false,
		MaxAge:           300,
	}))

	r.Use(middleware.Logger)
	r.Use(middleware.RequestID)
	r.Use(middleware.CleanPath)

	trpc_handler := trpc_handler(s)
	r.Method(http.MethodGet, "/trpc/*", trpc_handler)
	r.Method(http.MethodPost, "/trpc/*", trpc_handler)

	return r
}
