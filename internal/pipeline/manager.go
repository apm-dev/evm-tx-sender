package pipeline

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/apm-dev/evm-tx-sender/internal/domain"
	"github.com/apm-dev/evm-tx-sender/internal/infrastructure/ethereum"
	"github.com/ethereum/go-ethereum/common"
)

// Manager manages all sender pipelines and background workers.
type Manager struct {
	pipelines map[string]*Pipeline // key: "sender-chainID"
	repo      domain.Repository
	clients   map[uint64]domain.EthClient
	signer    domain.Signer
	gas       *ethereum.GasEngine
	chains    map[uint64]domain.ChainConfig
	log       *slog.Logger
}

func NewManager(
	repo domain.Repository,
	clients map[uint64]domain.EthClient,
	signer domain.Signer,
	gas *ethereum.GasEngine,
	chains map[uint64]domain.ChainConfig,
	log *slog.Logger,
) *Manager {
	return &Manager{
		pipelines: make(map[string]*Pipeline),
		repo:      repo,
		clients:   clients,
		signer:    signer,
		gas:       gas,
		chains:    chains,
		log:       log,
	}
}

// InitNonces initializes nonce cursors for all (sender, chain) pairs.
func (m *Manager) InitNonces(ctx context.Context) error {
	// Reset any PENDING transactions to QUEUED (crash recovery)
	count, err := m.repo.ResetPendingToQueued(ctx)
	if err != nil {
		return fmt.Errorf("reset pending: %w", err)
	}
	if count > 0 {
		m.log.Info("reset pending transactions to queued", "count", count)
	}

	for _, addr := range m.signer.Addresses() {
		for chainID, client := range m.clients {
			if err := m.initNonceForSender(ctx, addr, chainID, client); err != nil {
				return fmt.Errorf("init nonce for %s on chain %d: %w", addr.Hex(), chainID, err)
			}
		}
	}
	return nil
}

func (m *Manager) initNonceForSender(ctx context.Context, addr common.Address, chainID uint64, client domain.EthClient) error {
	senderHex := addr.Hex()

	// Get on-chain pending nonce
	pendingNonce, err := client.PendingNonceAt(ctx, addr)
	if err != nil {
		return fmt.Errorf("pending nonce: %w", err)
	}

	// Get DB cursor
	cursor, err := m.repo.GetNonceCursor(ctx, senderHex, chainID)
	if err != nil {
		return fmt.Errorf("get nonce cursor: %w", err)
	}

	// Use max(db_nonce, pending_nonce) as starting point
	startNonce := pendingNonce
	if cursor != nil && cursor.NextNonce > startNonce {
		// DB is ahead. Could mean txs were assigned but never broadcast.
		// After crash recovery (PENDING->QUEUED reset), we should align with on-chain.
		// Use on-chain as truth since PENDING txs were reset.
		m.log.Warn("DB nonce ahead of on-chain, aligning to on-chain",
			"sender", senderHex,
			"chain_id", chainID,
			"db_nonce", cursor.NextNonce,
			"onchain_nonce", pendingNonce,
		)
		startNonce = pendingNonce
	}

	if cursor == nil {
		err = m.repo.InitNonceCursor(ctx, senderHex, chainID, startNonce)
	} else {
		err = m.repo.SetNonceCursor(ctx, senderHex, chainID, startNonce)
	}
	if err != nil {
		return fmt.Errorf("set nonce cursor: %w", err)
	}

	m.log.Info("nonce initialized",
		"sender", senderHex,
		"chain_id", chainID,
		"nonce", startNonce,
	)
	return nil
}

// Start launches all pipelines and background workers.
func (m *Manager) Start(ctx context.Context) {
	// Start sender pipelines
	for chainID, chain := range m.chains {
		client, ok := m.clients[chainID]
		if !ok {
			continue
		}
		for _, addr := range m.signer.Addresses() {
			key := fmt.Sprintf("%s-%d", addr.Hex(), chainID)
			p := NewPipeline(addr, chain, m.repo, client, m.signer, m.gas, m.log)
			m.pipelines[key] = p
			go p.Run(ctx)
		}
	}

	// Start receipt pollers and stuck detectors
	for chainID, chain := range m.chains {
		client, ok := m.clients[chainID]
		if !ok {
			continue
		}

		pollInterval := chain.BlockTime
		if pollInterval < 2*time.Second {
			pollInterval = 2 * time.Second
		}

		rp := NewReceiptPoller(chainID, m.repo, client, pollInterval, m.log)
		go rp.Run(ctx)

		sd := NewStuckDetector(chain, m.repo, client, m.signer, m.gas, chain.GasBumpInterval, m.log)
		go sd.Run(ctx)
	}
}

// NotifyPipeline wakes the pipeline for a (sender, chain) pair.
func (m *Manager) NotifyPipeline(sender string, chainID uint64) {
	key := fmt.Sprintf("%s-%d", sender, chainID)
	if p, ok := m.pipelines[key]; ok {
		p.Notify()
	}
}

// PipelineStatus returns status info for the health endpoint.
func (m *Manager) PipelineStatus(ctx context.Context) map[string]PipelineInfo {
	result := make(map[string]PipelineInfo)
	for key, p := range m.pipelines {
		count, _ := m.repo.CountQueuedTransactions(ctx, p.sender.Hex(), p.chainID)
		result[key] = PipelineInfo{
			Status:     "running",
			QueueDepth: count,
		}
	}
	return result
}

type PipelineInfo struct {
	Status     string `json:"status"`
	QueueDepth int    `json:"queue_depth"`
}
