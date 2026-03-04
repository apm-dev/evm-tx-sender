package api

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/apm-dev/evm-tx-sender/internal/domain"
	"github.com/apm-dev/evm-tx-sender/internal/pipeline"
	"github.com/jackc/pgx/v5/pgxpool"
)

type HealthHandler struct {
	pool    *pgxpool.Pool
	clients map[uint64]domain.EthClient
	manager *pipeline.Manager
	log     *slog.Logger
}

func NewHealthHandler(pool *pgxpool.Pool, clients map[uint64]domain.EthClient, manager *pipeline.Manager, log *slog.Logger) *HealthHandler {
	return &HealthHandler{pool: pool, clients: clients, manager: manager, log: log}
}

type HealthResponse struct {
	Status    string                           `json:"status"`
	Chains    map[uint64]ChainHealth           `json:"chains"`
	Pipelines map[string]pipeline.PipelineInfo `json:"pipelines"`
	DB        string                           `json:"db"`
}

type ChainHealth struct {
	Healthy      bool   `json:"healthy"`
	BlockNumber  uint64 `json:"block_number,omitempty"`
	RPCEndpoints int    `json:"rpc_endpoints"`
}

// Health returns the current health status of the service.
//
// @Summary      Health check
// @Description  Returns the operational health of the service, including database connectivity,
// @Description  per-chain RPC status and latest block number, and active pipeline queue depths.
// @Description  Responds with 200 when all components are healthy, 503 when any component is degraded.
// @Tags         health
// @Produce      json
// @Success      200  {object}  HealthResponse  "Service is healthy"
// @Failure      503  {object}  HealthResponse  "Service is degraded"
// @Router       /health [get]
func (h *HealthHandler) Health(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	resp := HealthResponse{
		Status: "healthy",
		Chains: make(map[uint64]ChainHealth),
	}

	// DB check
	if err := h.pool.Ping(ctx); err != nil {
		h.log.Warn("health: db ping failed", "error", err)
		resp.DB = "disconnected"
		resp.Status = "degraded"
	} else {
		resp.DB = "connected"
	}

	// Chain checks
	for chainID, client := range h.clients {
		ch := ChainHealth{
			Healthy: client.Healthy(),
		}
		if ch.Healthy {
			if bn, err := client.BlockNumber(ctx); err == nil {
				ch.BlockNumber = bn
			} else {
				h.log.Warn("health: block number fetch failed", "chain_id", chainID, "error", err)
			}
		} else {
			h.log.Warn("health: chain rpc unhealthy", "chain_id", chainID)
			resp.Status = "degraded"
		}
		resp.Chains[chainID] = ch
	}

	// Pipeline status
	resp.Pipelines = h.manager.PipelineStatus(context.Background())

	status := http.StatusOK
	if resp.Status != "healthy" {
		status = http.StatusServiceUnavailable
	}

	writeJSON(w, status, resp)
}
