package pipeline

import (
	"context"
	"fmt"
	"log/slog"
	"math/big"
	"time"

	"github.com/apm-dev/evm-tx-sender/internal/domain"
	"github.com/apm-dev/evm-tx-sender/internal/infrastructure/ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/oklog/ulid/v2"
)

const maxGasBumps = 5

// StuckDetector detects and handles stuck transactions via gas bumping.
type StuckDetector struct {
	chainID        uint64
	chain          domain.ChainConfig
	repo           domain.Repository
	client         domain.EthClient
	signer         domain.Signer
	gas            *ethereum.GasEngine
	interval       time.Duration
	stuckThreshold time.Duration
	log            *slog.Logger
}

func NewStuckDetector(
	chain domain.ChainConfig,
	repo domain.Repository,
	client domain.EthClient,
	signer domain.Signer,
	gas *ethereum.GasEngine,
	interval time.Duration,
	log *slog.Logger,
) *StuckDetector {
	threshold := chain.StuckThreshold
	if threshold == 0 {
		threshold = 3 * time.Minute
	}
	return &StuckDetector{
		chainID:        chain.ChainID,
		chain:          chain,
		repo:           repo,
		client:         client,
		signer:         signer,
		gas:            gas,
		interval:       interval,
		stuckThreshold: threshold,
		log:            log.With("component", "stuck-detector", "chain_id", chain.ChainID),
	}
}

func (sd *StuckDetector) Run(ctx context.Context) {
	sd.log.Info("stuck detector started")
	defer sd.log.Info("stuck detector stopped")

	ticker := time.NewTicker(sd.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sd.check(ctx)
		}
	}
}

func (sd *StuckDetector) check(ctx context.Context) {
	txs, err := sd.repo.GetSubmittedTransactions(ctx, sd.chainID)
	if err != nil {
		sd.log.Error("failed to get submitted txs", "error", err)
		return
	}

	now := time.Now().UTC()
	for _, tx := range txs {
		if tx.SubmittedAt == nil {
			continue
		}
		age := now.Sub(*tx.SubmittedAt)
		if age < sd.stuckThreshold {
			continue
		}

		// Check if already at max bumps
		attempts, err := sd.repo.GetAttemptsByTransactionID(ctx, tx.ID)
		if err != nil {
			sd.log.Error("failed to get attempts", "tx_id", tx.ID, "error", err)
			continue
		}
		if len(attempts) > maxGasBumps {
			sd.log.Error("max gas bumps exceeded, marking failed", "tx_id", tx.ID)
			_ = sd.repo.MarkFailed(ctx, tx.ID, domain.ErrCodeMaxRetriesExceeded, "max gas bumps exceeded")
			_ = sd.repo.LogStateTransition(ctx, &domain.TxStateLog{
				TransactionID: tx.ID,
				FromStatus:    string(domain.TxStatusSubmitted),
				ToStatus:      string(domain.TxStatusFailed),
				Actor:         "stuck-detector",
				Reason:        "max gas bumps exceeded",
			})
			continue
		}

		sd.bumpGas(ctx, tx, attempts)
	}
}

func (sd *StuckDetector) bumpGas(ctx context.Context, tx *domain.Transaction, attempts []*domain.TxAttempt) {
	if tx.Nonce == nil {
		return
	}

	lastAttempt := attempts[len(attempts)-1]
	bumpNumber := len(attempts)

	// Only bump if enough time since last attempt
	if time.Since(lastAttempt.CreatedAt) < sd.chain.GasBumpInterval {
		return
	}

	// Get current gas params from last attempt
	currentGas := domain.GasParams{
		GasLimit:             lastAttempt.GasLimit,
		MaxFeePerGas:         lastAttempt.MaxFeePerGas,
		MaxPriorityFeePerGas: lastAttempt.MaxPriorityFeePerGas,
		GasPrice:             lastAttempt.GasPrice,
		TxType:               lastAttempt.TxType,
	}

	// Bump gas
	bumpedGas := sd.gas.BumpGas(currentGas, bumpNumber)

	// Build replacement transaction with same nonce
	chainIDBig := new(big.Int).SetUint64(sd.chainID)
	toAddr := common.HexToAddress(tx.ToAddress)

	var ethTx *types.Transaction
	if bumpedGas.TxType == 2 {
		ethTx = types.NewTx(&types.DynamicFeeTx{
			ChainID:   chainIDBig,
			Nonce:     *tx.Nonce,
			GasTipCap: bumpedGas.MaxPriorityFeePerGas,
			GasFeeCap: bumpedGas.MaxFeePerGas,
			Gas:       bumpedGas.GasLimit,
			To:        &toAddr,
			Value:     tx.Value,
			Data:      tx.Data,
		})
	} else {
		ethTx = types.NewTx(&types.LegacyTx{
			Nonce:    *tx.Nonce,
			GasPrice: bumpedGas.GasPrice,
			Gas:      bumpedGas.GasLimit,
			To:       &toAddr,
			Value:    tx.Value,
			Data:     tx.Data,
		})
	}

	senderAddr := common.HexToAddress(tx.Sender)
	signed, err := sd.signer.Sign(ctx, senderAddr, ethTx, chainIDBig)
	if err != nil {
		sd.log.Error("failed to sign bumped tx", "tx_id", tx.ID, "error", err)
		return
	}

	rawTx, err := signed.MarshalBinary()
	if err != nil {
		sd.log.Error("failed to marshal bumped tx", "tx_id", tx.ID, "error", err)
		return
	}

	txHash := signed.Hash().Hex()

	// Store attempt
	attempt := &domain.TxAttempt{
		ID:                   ulid.Make().String(),
		TransactionID:        tx.ID,
		AttemptNumber:        bumpNumber + 1,
		TxHash:               txHash,
		GasLimit:             bumpedGas.GasLimit,
		MaxFeePerGas:         bumpedGas.MaxFeePerGas,
		MaxPriorityFeePerGas: bumpedGas.MaxPriorityFeePerGas,
		GasPrice:             bumpedGas.GasPrice,
		TxType:               bumpedGas.TxType,
		RawTx:                rawTx,
		Status:               domain.AttemptBroadcast,
		CreatedAt:            time.Now().UTC(),
	}
	if err := sd.repo.CreateAttempt(ctx, attempt); err != nil {
		sd.log.Error("failed to store bumped attempt", "tx_id", tx.ID, "error", err)
		return
	}

	// Broadcast
	if err := sd.client.SendTransaction(ctx, signed); err != nil {
		sd.log.Warn("failed to broadcast bumped tx", "tx_id", tx.ID, "error", err)
		// Non-fatal: the original might still get mined
		return
	}

	// Update the main transaction's tx_hash to the latest attempt
	_ = sd.repo.MarkSubmitted(ctx, tx.ID, txHash, nil)

	_ = sd.repo.LogStateTransition(ctx, &domain.TxStateLog{
		TransactionID: tx.ID,
		FromStatus:    string(domain.TxStatusSubmitted),
		ToStatus:      string(domain.TxStatusSubmitted),
		Actor:         "stuck-detector",
		Reason:        fmt.Sprintf("gas bump #%d, new hash %s", bumpNumber, txHash),
	})

	sd.log.Info("gas bumped",
		"tx_id", tx.ID,
		"bump", bumpNumber,
		"new_hash", txHash,
		"max_fee", bumpedGas.MaxFeePerGas,
		"priority_fee", bumpedGas.MaxPriorityFeePerGas,
	)
}
