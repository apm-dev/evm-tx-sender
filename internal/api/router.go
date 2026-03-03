package api

import (
	"log/slog"
	"net/http"

	"github.com/apm-dev/evm-tx-sender/internal/domain"
	"github.com/apm-dev/evm-tx-sender/internal/pipeline"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func NewRouter(
	repo domain.Repository,
	signer domain.Signer,
	chains map[uint64]domain.ChainConfig,
	clients map[uint64]domain.EthClient,
	manager *pipeline.Manager,
	pool *pgxpool.Pool,
	log *slog.Logger,
) http.Handler {
	r := chi.NewRouter()

	r.Use(RecoveryMiddleware(log))
	r.Use(LoggingMiddleware(log))

	transferH := NewTransferHandler(repo, signer, chains, manager)
	txH := NewTransactionHandler(repo)
	healthH := NewHealthHandler(pool, clients, manager)

	r.Route("/v1", func(r chi.Router) {
		r.Post("/transfers", transferH.Create)
		r.Get("/transactions/{id}", txH.Get)
		r.Get("/transactions", txH.List)
	})

	r.Get("/v1/health", healthH.Health)

	return r
}
