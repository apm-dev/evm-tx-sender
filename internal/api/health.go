package api

import (
	"context"
	"net/http"

	"github.com/apm-dev/evm-tx-sender/internal/domain"
	"github.com/apm-dev/evm-tx-sender/internal/pipeline"
	"github.com/jackc/pgx/v5/pgxpool"
)

type HealthHandler struct {
	pool    *pgxpool.Pool
	clients map[uint64]domain.EthClient
	manager *pipeline.Manager
}

func NewHealthHandler(pool *pgxpool.Pool, clients map[uint64]domain.EthClient, manager *pipeline.Manager) *HealthHandler {
	return &HealthHandler{pool: pool, clients: clients, manager: manager}
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

func (h *HealthHandler) Health(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	resp := HealthResponse{
		Status: "healthy",
		Chains: make(map[uint64]ChainHealth),
	}

	// DB check
	if err := h.pool.Ping(ctx); err != nil {
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
			}
		} else {
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
