package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/apm-dev/evm-tx-sender/internal/domain"
	"github.com/ethereum/go-ethereum/common"
)

// ReceiptPoller polls for receipts of SUBMITTED transactions.
type ReceiptPoller struct {
	chainID  uint64
	repo     domain.Repository
	client   domain.EthClient
	interval time.Duration
	log      *slog.Logger
}

func NewReceiptPoller(
	chainID uint64,
	repo domain.Repository,
	client domain.EthClient,
	interval time.Duration,
	log *slog.Logger,
) *ReceiptPoller {
	return &ReceiptPoller{
		chainID:  chainID,
		repo:     repo,
		client:   client,
		interval: interval,
		log:      log.With("component", "receipt-poller", "chain_id", chainID),
	}
}

func (rp *ReceiptPoller) Run(ctx context.Context) {
	rp.log.Info("receipt poller started")
	defer rp.log.Info("receipt poller stopped")

	ticker := time.NewTicker(rp.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			rp.poll(ctx)
		}
	}
}

func (rp *ReceiptPoller) poll(ctx context.Context) {
	txs, err := rp.repo.GetSubmittedTransactions(ctx, rp.chainID)
	if err != nil {
		rp.log.Error("failed to get submitted transactions", "error", err)
		return
	}

	for _, tx := range txs {
		if tx.FinalTxHash == "" {
			continue
		}

		hash := common.HexToHash(tx.FinalTxHash)
		receipt, err := rp.client.TransactionReceipt(ctx, hash)
		if err != nil {
			// No receipt yet -- transaction still pending
			continue
		}

		receiptJSON, _ := json.Marshal(receipt)
		actor := fmt.Sprintf("receipt-poller:%d", rp.chainID)

		txReceipt := &domain.TxReceipt{
			TxHash:            tx.FinalTxHash,
			BlockNumber:       receipt.BlockNumber.Uint64(),
			BlockHash:         receipt.BlockHash.Hex(),
			GasUsed:           receipt.GasUsed,
			EffectiveGasPrice: receipt.EffectiveGasPrice,
			Status:            uint8(receipt.Status),
			ReceiptJSON:       receiptJSON,
		}

		if receipt.Status == 1 {
			if err := rp.repo.MarkConfirmed(ctx, tx.ID, txReceipt); err != nil {
				rp.log.Error("failed to mark confirmed", "tx_id", tx.ID, "error", err)
				continue
			}
			_ = rp.repo.LogStateTransition(ctx, &domain.TxStateLog{
				TransactionID: tx.ID,
				FromStatus:    string(domain.TxStatusSubmitted),
				ToStatus:      string(domain.TxStatusConfirmed),
				Actor:         actor,
				Reason:        fmt.Sprintf("confirmed in block %d", receipt.BlockNumber.Uint64()),
			})
			rp.log.Info("transaction confirmed",
				"tx_id", tx.ID,
				"tx_hash", tx.FinalTxHash,
				"block", receipt.BlockNumber.Uint64(),
				"gas_used", receipt.GasUsed,
			)

			// Mark the confirmed attempt
			rp.markAttemptConfirmed(ctx, tx.ID, tx.FinalTxHash)
		} else {
			if err := rp.repo.MarkReverted(ctx, tx.ID, txReceipt); err != nil {
				rp.log.Error("failed to mark reverted", "tx_id", tx.ID, "error", err)
				continue
			}
			_ = rp.repo.LogStateTransition(ctx, &domain.TxStateLog{
				TransactionID: tx.ID,
				FromStatus:    string(domain.TxStatusSubmitted),
				ToStatus:      string(domain.TxStatusReverted),
				Actor:         actor,
				Reason:        fmt.Sprintf("reverted in block %d", receipt.BlockNumber.Uint64()),
			})
			rp.log.Warn("transaction reverted",
				"tx_id", tx.ID,
				"tx_hash", tx.FinalTxHash,
				"block", receipt.BlockNumber.Uint64(),
			)
		}
	}
}

func (rp *ReceiptPoller) markAttemptConfirmed(ctx context.Context, txID, txHash string) {
	attempts, err := rp.repo.GetAttemptsByTransactionID(ctx, txID)
	if err != nil {
		return
	}
	for _, a := range attempts {
		if a.TxHash == txHash {
			_ = rp.repo.MarkAttemptStatus(ctx, a.ID, domain.AttemptConfirmed)
		} else if a.Status == domain.AttemptBroadcast {
			_ = rp.repo.MarkAttemptStatus(ctx, a.ID, domain.AttemptReplaced)
		}
	}
}
