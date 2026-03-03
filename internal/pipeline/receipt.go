package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/apm-dev/evm-tx-sender/internal/domain"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

// ReceiptPoller polls for receipts of SUBMITTED transactions and checks
// confirmation depth for INCLUDED transactions (reorg detection).
type ReceiptPoller struct {
	chainID            uint64
	confirmationBlocks int
	repo               domain.Repository
	client             domain.EthClient
	interval           time.Duration
	log                *slog.Logger
}

func NewReceiptPoller(
	chainID uint64,
	confirmationBlocks int,
	repo domain.Repository,
	client domain.EthClient,
	interval time.Duration,
	log *slog.Logger,
) *ReceiptPoller {
	return &ReceiptPoller{
		chainID:            chainID,
		confirmationBlocks: confirmationBlocks,
		repo:               repo,
		client:             client,
		interval:           interval,
		log:                log.With("component", "receipt-poller", "chain_id", chainID),
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
	rp.pollSubmitted(ctx)
	rp.pollIncluded(ctx)
}

// pollSubmitted handles SUBMITTED -> INCLUDED (or direct CONFIRMED if confirmationBlocks == 0).
func (rp *ReceiptPoller) pollSubmitted(ctx context.Context) {
	txs, err := rp.repo.GetSubmittedTransactions(ctx, rp.chainID)
	if err != nil {
		rp.log.Error("failed to get submitted transactions", "error", err)
		return
	}

	// Collect hashes for batch fetch
	var hashes []common.Hash
	hashToTx := make(map[common.Hash]*domain.Transaction, len(txs))
	for _, tx := range txs {
		if tx.FinalTxHash == "" {
			continue
		}
		h := common.HexToHash(tx.FinalTxHash)
		hashes = append(hashes, h)
		hashToTx[h] = tx
	}

	if len(hashes) == 0 {
		return
	}

	receipts, err := rp.client.BatchTransactionReceipts(ctx, hashes)
	if err != nil {
		rp.log.Error("failed to batch fetch receipts", "error", err)
		return
	}

	actor := fmt.Sprintf("receipt-poller:%d", rp.chainID)

	for hash, receipt := range receipts {
		tx := hashToTx[hash]

		receiptJSON, _ := json.Marshal(receipt)

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
			// For ERC20 transfers, verify a Transfer event log exists.
			if tx.TokenContract != "" && !rp.verifyERC20Transfer(receipt, tx) {
				if err := rp.repo.MarkReverted(ctx, tx.ID, txReceipt); err != nil {
					rp.log.Error("failed to mark reverted (ERC20 not verified)", "tx_id", tx.ID, "error", err)
					continue
				}
				reason := fmt.Sprintf("ERC20 Transfer event not found in receipt logs (block %d)", receipt.BlockNumber.Uint64())
				_ = rp.repo.LogStateTransition(ctx, &domain.TxStateLog{
					TransactionID: tx.ID,
					FromStatus:    string(domain.TxStatusSubmitted),
					ToStatus:      string(domain.TxStatusReverted),
					Actor:         actor,
					Reason:        reason,
				})
				rp.log.Warn("ERC20 transfer not verified",
					"tx_id", tx.ID,
					"tx_hash", tx.FinalTxHash,
					"token_contract", tx.TokenContract,
					"block", receipt.BlockNumber.Uint64(),
				)
				continue
			}

			if rp.confirmationBlocks > 0 {
				// Wait for confirmation depth -- mark as INCLUDED
				if err := rp.repo.MarkIncluded(ctx, tx.ID, txReceipt); err != nil {
					rp.log.Error("failed to mark included", "tx_id", tx.ID, "error", err)
					continue
				}
				_ = rp.repo.LogStateTransition(ctx, &domain.TxStateLog{
					TransactionID: tx.ID,
					FromStatus:    string(domain.TxStatusSubmitted),
					ToStatus:      string(domain.TxStatusIncluded),
					Actor:         actor,
					Reason:        fmt.Sprintf("included in block %d, waiting for %d confirmations", receipt.BlockNumber.Uint64(), rp.confirmationBlocks),
				})
				rp.log.Info("transaction included, awaiting confirmations",
					"tx_id", tx.ID,
					"tx_hash", tx.FinalTxHash,
					"block", receipt.BlockNumber.Uint64(),
					"confirmations_needed", rp.confirmationBlocks,
				)
			} else {
				// No confirmation depth required -- confirm directly
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
			}
		} else {
			// Reverted -- mark immediately regardless of confirmationBlocks
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

// pollIncluded handles INCLUDED -> CONFIRMED (enough depth) or INCLUDED -> SUBMITTED (reorg).
func (rp *ReceiptPoller) pollIncluded(ctx context.Context) {
	if rp.confirmationBlocks == 0 {
		return
	}

	currentBlock, err := rp.client.BlockNumber(ctx)
	if err != nil {
		rp.log.Error("failed to get current block number", "error", err)
		return
	}

	txs, err := rp.repo.GetIncludedTransactions(ctx, rp.chainID)
	if err != nil {
		rp.log.Error("failed to get included transactions", "error", err)
		return
	}

	// Filter txs with enough depth and collect hashes
	var readyTxs []*domain.Transaction
	var hashes []common.Hash
	hashToTx := make(map[common.Hash]*domain.Transaction)

	for _, tx := range txs {
		if tx.BlockNumber == nil {
			continue
		}
		depth := currentBlock - *tx.BlockNumber
		if depth < uint64(rp.confirmationBlocks) {
			continue
		}
		h := common.HexToHash(tx.FinalTxHash)
		hashes = append(hashes, h)
		hashToTx[h] = tx
		readyTxs = append(readyTxs, tx)
	}

	if len(hashes) == 0 {
		return
	}

	receipts, err := rp.client.BatchTransactionReceipts(ctx, hashes)
	if err != nil {
		rp.log.Error("failed to batch fetch receipts for included txs", "error", err)
		return
	}

	actor := fmt.Sprintf("receipt-poller:%d", rp.chainID)

	for _, tx := range readyTxs {
		hash := common.HexToHash(tx.FinalTxHash)
		receipt, found := receipts[hash]

		if !found {
			// Reorg detected: receipt no longer available
			rp.log.Warn("reorg detected, receipt gone",
				"tx_id", tx.ID,
				"tx_hash", tx.FinalTxHash,
				"original_block", *tx.BlockNumber,
			)

			if err := rp.repo.RevertToSubmitted(ctx, tx.ID); err != nil {
				rp.log.Error("failed to revert to submitted", "tx_id", tx.ID, "error", err)
				continue
			}
			_ = rp.repo.LogStateTransition(ctx, &domain.TxStateLog{
				TransactionID: tx.ID,
				FromStatus:    string(domain.TxStatusIncluded),
				ToStatus:      string(domain.TxStatusSubmitted),
				Actor:         actor,
				Reason:        "reorg detected",
			})

			// Re-broadcast raw tx from latest attempt
			rp.rebroadcastAfterReorg(ctx, tx)
			continue
		}

		receiptBlockHash := receipt.BlockHash.Hex()

		if receiptBlockHash == tx.BlockHash {
			// Same block hash -- confirmed at expected depth
			depth := currentBlock - *tx.BlockNumber
			receiptJSON, _ := json.Marshal(receipt)
			txReceipt := &domain.TxReceipt{
				TxHash:            tx.FinalTxHash,
				BlockNumber:       receipt.BlockNumber.Uint64(),
				BlockHash:         receiptBlockHash,
				GasUsed:           receipt.GasUsed,
				EffectiveGasPrice: receipt.EffectiveGasPrice,
				Status:            uint8(receipt.Status),
				ReceiptJSON:       receiptJSON,
			}

			if err := rp.repo.MarkConfirmed(ctx, tx.ID, txReceipt); err != nil {
				rp.log.Error("failed to mark confirmed", "tx_id", tx.ID, "error", err)
				continue
			}
			_ = rp.repo.LogStateTransition(ctx, &domain.TxStateLog{
				TransactionID: tx.ID,
				FromStatus:    string(domain.TxStatusIncluded),
				ToStatus:      string(domain.TxStatusConfirmed),
				Actor:         actor,
				Reason:        fmt.Sprintf("confirmed after %d blocks (block %d)", depth, receipt.BlockNumber.Uint64()),
			})
			rp.log.Info("transaction confirmed with depth",
				"tx_id", tx.ID,
				"tx_hash", tx.FinalTxHash,
				"block", receipt.BlockNumber.Uint64(),
				"depth", depth,
				"gas_used", receipt.GasUsed,
			)
			rp.markAttemptConfirmed(ctx, tx.ID, tx.FinalTxHash)
		} else {
			// Receipt exists but in a different block -- reorged into new block.
			// Update block info and restart confirmation counting.
			rp.log.Warn("reorg into different block",
				"tx_id", tx.ID,
				"tx_hash", tx.FinalTxHash,
				"old_block", *tx.BlockNumber,
				"old_block_hash", tx.BlockHash,
				"new_block", receipt.BlockNumber.Uint64(),
				"new_block_hash", receiptBlockHash,
			)

			receiptJSON, _ := json.Marshal(receipt)
			txReceipt := &domain.TxReceipt{
				TxHash:            tx.FinalTxHash,
				BlockNumber:       receipt.BlockNumber.Uint64(),
				BlockHash:         receiptBlockHash,
				GasUsed:           receipt.GasUsed,
				EffectiveGasPrice: receipt.EffectiveGasPrice,
				Status:            uint8(receipt.Status),
				ReceiptJSON:       receiptJSON,
			}

			if err := rp.repo.MarkIncluded(ctx, tx.ID, txReceipt); err != nil {
				rp.log.Error("failed to update included with new block", "tx_id", tx.ID, "error", err)
				continue
			}
			_ = rp.repo.LogStateTransition(ctx, &domain.TxStateLog{
				TransactionID: tx.ID,
				FromStatus:    string(domain.TxStatusIncluded),
				ToStatus:      string(domain.TxStatusIncluded),
				Actor:         actor,
				Reason:        fmt.Sprintf("reorged from block %d to block %d, restarting confirmation count", *tx.BlockNumber, receipt.BlockNumber.Uint64()),
			})
		}
	}
}

// rebroadcastAfterReorg retrieves the latest attempt's raw_tx and re-sends it.
func (rp *ReceiptPoller) rebroadcastAfterReorg(ctx context.Context, tx *domain.Transaction) {
	attempts, err := rp.repo.GetAttemptsByTransactionID(ctx, tx.ID)
	if err != nil {
		rp.log.Warn("failed to get attempts for rebroadcast", "tx_id", tx.ID, "error", err)
		return
	}
	if len(attempts) == 0 {
		rp.log.Warn("no attempts found for rebroadcast", "tx_id", tx.ID)
		return
	}

	// Use the latest attempt
	latestAttempt := attempts[len(attempts)-1]
	if len(latestAttempt.RawTx) == 0 {
		rp.log.Warn("latest attempt has no raw_tx", "tx_id", tx.ID, "attempt_id", latestAttempt.ID)
		return
	}

	var ethTx types.Transaction
	if err := ethTx.UnmarshalBinary(latestAttempt.RawTx); err != nil {
		rp.log.Warn("failed to unmarshal raw_tx for rebroadcast", "tx_id", tx.ID, "error", err)
		return
	}

	if err := rp.client.SendTransaction(ctx, &ethTx); err != nil {
		rp.log.Warn("rebroadcast after reorg failed", "tx_id", tx.ID, "tx_hash", tx.FinalTxHash, "error", err)
		return
	}

	rp.log.Info("rebroadcast after reorg succeeded", "tx_id", tx.ID, "tx_hash", tx.FinalTxHash)
}

// transferEventSig is keccak256("Transfer(address,address,uint256)").
var transferEventSig = crypto.Keccak256Hash([]byte("Transfer(address,address,uint256)"))

// verifyERC20Transfer checks that the receipt contains a Transfer event log
// matching the expected token contract, sender, and recipient. This catches
// non-standard ERC20s (e.g. USDT) that return false instead of reverting.
func (rp *ReceiptPoller) verifyERC20Transfer(receipt *types.Receipt, tx *domain.Transaction) bool {
	contract := common.HexToAddress(tx.TokenContract)
	from := common.BytesToHash(common.HexToAddress(tx.Sender).Bytes())
	to := common.BytesToHash(common.HexToAddress(tx.TransferRecipient).Bytes())

	for _, log := range receipt.Logs {
		if !strings.EqualFold(log.Address.Hex(), contract.Hex()) {
			continue
		}
		if len(log.Topics) < 3 {
			continue
		}
		if log.Topics[0] != transferEventSig {
			continue
		}
		if log.Topics[1] != from {
			continue
		}
		if log.Topics[2] != to {
			continue
		}
		return true
	}
	return false
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
