package pipeline

import (
	"context"
	"fmt"
	"log/slog"
	"math/big"
	"strings"
	"time"

	"github.com/apm-dev/evm-tx-sender/internal/domain"
	"github.com/apm-dev/evm-tx-sender/internal/infrastructure/ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/oklog/ulid/v2"
)

const (
	maxBroadcastRetries = 5
	pollInterval        = 2 * time.Second
)

// Pipeline processes transactions for a single (sender, chain) pair.
type Pipeline struct {
	sender  common.Address
	chainID uint64
	chain   *domain.ChainConfig
	repo    domain.Repository
	client  domain.EthClient
	signer  domain.Signer
	gas     *ethereum.GasEngine
	notify  chan struct{}
	log     *slog.Logger
}

func NewPipeline(
	sender common.Address,
	chain *domain.ChainConfig,
	repo domain.Repository,
	client domain.EthClient,
	signer domain.Signer,
	gas *ethereum.GasEngine,
	log *slog.Logger,
) *Pipeline {
	return &Pipeline{
		sender:  sender,
		chainID: chain.ChainID,
		chain:   chain,
		repo:    repo,
		client:  client,
		signer:  signer,
		gas:     gas,
		notify:  make(chan struct{}, 1),
		log: log.With(
			"component", "pipeline",
			"sender", strings.ToLower(sender.Hex()),
			"chain_id", chain.ChainID,
		),
	}
}

// senderHex returns the lower-case hex representation of the sender address.
// All DB queries and map keys must use this to ensure consistent casing.
func (p *Pipeline) senderHex() string {
	return strings.ToLower(p.sender.Hex())
}

func (p *Pipeline) Notify() {
	select {
	case p.notify <- struct{}{}:
	default:
	}
}

func (p *Pipeline) Run(ctx context.Context) {
	pipelineID := fmt.Sprintf("%s-%d", p.senderHex(), p.chainID)
	p.log.Info("pipeline started")
	defer p.log.Info("pipeline stopped")

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		tx, err := p.repo.ClaimNextQueued(ctx, p.senderHex(), p.chainID, pipelineID)
		if err != nil {
			p.log.Error("failed to claim queued tx", "error", err)
			sleepCtx(ctx, 5*time.Second)
			continue
		}

		if tx == nil {
			select {
			case <-ctx.Done():
				return
			case <-p.notify:
			case <-time.After(pollInterval):
			}
			continue
		}

		p.processTx(ctx, tx, pipelineID)
	}
}

func (p *Pipeline) processTx(ctx context.Context, tx *domain.Transaction, actor string) {
	log := p.log.With("tx_id", tx.ID)

	// Log state transition QUEUED -> PENDING
	p.logTransition(ctx, tx.ID, string(domain.TxStatusQueued), string(domain.TxStatusPending), actor, "claimed by pipeline")

	// 1. Assign nonce
	nonce, err := p.repo.IncrementNonceCursor(ctx, p.senderHex(), p.chainID)
	if err != nil {
		log.Error("nonce assignment failed", "error", err)
		p.failTx(ctx, tx, domain.ErrCodeInternalError, "nonce assignment: "+err.Error(), actor)
		return
	}

	if err := p.repo.MarkPending(ctx, tx.ID, nonce); err != nil {
		log.Error("failed to mark pending with nonce", "error", err)
		return
	}

	log = log.With("nonce", nonce)
	log.Info("nonce assigned")

	// 2. Estimate gas
	toAddr := common.HexToAddress(tx.ToAddress)
	gasParams, err := p.gas.Estimate(ctx, p.client, p.chain, p.sender, toAddr, tx.Data, tx.Value, tx.Priority)
	if err != nil {
		log.Error("gas estimation failed", "error", err)
		p.failTx(ctx, tx, domain.ErrCodeEstimationReverted, err.Error(), actor)
		return
	}

	// 3. Build and sign transaction
	signedTx, rawTx, err := p.buildAndSign(ctx, tx, nonce, gasParams)
	if err != nil {
		log.Error("signing failed", "error", err)
		p.failTx(ctx, tx, domain.ErrCodeSigningFailed, err.Error(), actor)
		return
	}

	txHash := signedTx.Hash().Hex()
	log = log.With("tx_hash", txHash)

	// 4. Store attempt
	attempt := &domain.TxAttempt{
		ID:                   ulid.Make().String(),
		TransactionID:        tx.ID,
		AttemptNumber:        1,
		TxHash:               txHash,
		GasLimit:             gasParams.GasLimit,
		MaxFeePerGas:         gasParams.MaxFeePerGas,
		MaxPriorityFeePerGas: gasParams.MaxPriorityFeePerGas,
		GasPrice:             gasParams.GasPrice,
		TxType:               gasParams.TxType,
		RawTx:                rawTx,
		Status:               domain.AttemptBroadcast,
		CreatedAt:            time.Now().UTC(),
	}
	if err := p.repo.CreateAttempt(ctx, attempt); err != nil {
		log.Error("failed to store attempt", "error", err)
		return
	}

	// 5. Broadcast with retries
	var broadcastErr error
	for i := range maxBroadcastRetries {
		broadcastErr = p.client.SendTransaction(ctx, signedTx)
		if broadcastErr == nil {
			break
		}
		log.Warn("broadcast failed, retrying", "attempt", i+1, "error", broadcastErr)
		sleepCtx(ctx, time.Duration(i+1)*time.Second)
	}

	if broadcastErr != nil {
		log.Error("broadcast permanently failed", "error", broadcastErr)
		p.failTx(ctx, tx, domain.ErrCodeBroadcastFailed, broadcastErr.Error(), actor)
		return
	}

	// 6. Mark submitted
	if err := p.repo.MarkSubmitted(ctx, tx.ID, txHash, nil); err != nil {
		log.Error("failed to mark submitted", "error", err)
		return
	}

	p.logTransition(ctx, tx.ID, string(domain.TxStatusPending), string(domain.TxStatusSubmitted), actor,
		fmt.Sprintf("broadcast with hash %s, nonce %d", txHash, nonce))

	log.Info("transaction submitted",
		"gas_limit", gasParams.GasLimit,
		"max_fee", gasParams.MaxFeePerGas,
		"priority_fee", gasParams.MaxPriorityFeePerGas,
	)
}

func (p *Pipeline) buildAndSign(ctx context.Context, tx *domain.Transaction, nonce uint64, gas domain.GasParams) (*types.Transaction, []byte, error) {
	chainID := new(big.Int).SetUint64(p.chainID)
	toAddr := common.HexToAddress(tx.ToAddress)

	var ethTx *types.Transaction

	if gas.TxType == 2 {
		ethTx = types.NewTx(&types.DynamicFeeTx{
			ChainID:   chainID,
			Nonce:     nonce,
			GasTipCap: gas.MaxPriorityFeePerGas,
			GasFeeCap: gas.MaxFeePerGas,
			Gas:       gas.GasLimit,
			To:        &toAddr,
			Value:     tx.Value,
			Data:      tx.Data,
		})
	} else {
		ethTx = types.NewTx(&types.LegacyTx{
			Nonce:    nonce,
			GasPrice: gas.GasPrice,
			Gas:      gas.GasLimit,
			To:       &toAddr,
			Value:    tx.Value,
			Data:     tx.Data,
		})
	}

	signed, err := p.signer.Sign(ctx, p.sender, ethTx, chainID)
	if err != nil {
		return nil, nil, err
	}

	rawTx, err := signed.MarshalBinary()
	if err != nil {
		return nil, nil, fmt.Errorf("marshal signed tx: %w", err)
	}

	return signed, rawTx, nil
}

func (p *Pipeline) failTx(ctx context.Context, tx *domain.Transaction, code domain.ErrorCode, reason string, actor string) {
	_ = p.repo.MarkFailed(ctx, tx.ID, code, reason)
	p.logTransition(ctx, tx.ID, string(tx.Status), string(domain.TxStatusFailed), actor, reason)
}

func (p *Pipeline) logTransition(ctx context.Context, txID, from, to, actor, reason string) {
	_ = p.repo.LogStateTransition(ctx, &domain.TxStateLog{
		TransactionID: txID,
		FromStatus:    from,
		ToStatus:      to,
		Actor:         actor,
		Reason:        reason,
	})
}

func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
