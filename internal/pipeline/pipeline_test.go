package pipeline

import (
	"context"
	"fmt"
	"log/slog"
	"math/big"
	"testing"
	"time"

	"github.com/apm-dev/evm-tx-sender/internal/domain"
	"github.com/apm-dev/evm-tx-sender/internal/infrastructure/ethereum"
	"github.com/apm-dev/evm-tx-sender/internal/mocks"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"
)

var (
	pipelineSender = common.HexToAddress("0xAb5801a7D398351b8bE11C439e05C5B3259aeC9B")
	pipelineTo     = "0x1234567890abcdef1234567890abcdef12345678"
)

func testChainConfig() domain.ChainConfig {
	return domain.ChainConfig{
		ChainID:            1,
		SupportsEIP1559:    true,
		GasLimitMultiplier: 1.2,
		MinMaxFee:          big.NewInt(1_000_000_000),
		MaxMaxFee:          big.NewInt(500_000_000_000),
		MinPriorityFee:     big.NewInt(1_000_000_000),
		MaxPriorityFee:     big.NewInt(50_000_000_000),
		StuckThreshold:     3 * time.Minute,
		GasBumpInterval:    30 * time.Second,
	}
}

func newTestPipeline(ctrl *gomock.Controller) (*Pipeline, *mocks.MockRepository, *mocks.MockEthClient, *mocks.MockSigner) {
	repo := mocks.NewMockRepository(ctrl)
	client := mocks.NewMockEthClient(ctrl)
	signer := mocks.NewMockSigner(ctrl)
	gas := ethereum.NewGasEngine()
	chain := testChainConfig()
	log := slog.Default()

	p := NewPipeline(pipelineSender, chain, repo, client, signer, gas, log)
	return p, repo, client, signer
}

func testTransaction() *domain.Transaction {
	return &domain.Transaction{
		ID:                "tx-001",
		IdempotencyKey:    "idem-1",
		ChainID:           1,
		Sender:            pipelineSender.Hex(),
		ToAddress:         pipelineTo,
		Value:             big.NewInt(1_000_000_000_000_000_000), // 1 ETH
		Priority:          domain.PriorityNormal,
		Status:            domain.TxStatusQueued,
		TransferToken:     "ETH",
		TransferAmount:    "1.0",
		TransferRecipient: pipelineTo,
		CreatedAt:         time.Now().UTC(),
		UpdatedAt:         time.Now().UTC(),
	}
}

func TestPipeline_ProcessTx_HappyPath(t *testing.T) {
	ctrl := gomock.NewController(t)
	p, repo, client, signer := newTestPipeline(ctrl)
	ctx := context.Background()
	tx := testTransaction()
	actor := pipelineSender.Hex() + "-1"

	// State transition log QUEUED->PENDING
	repo.EXPECT().LogStateTransition(gomock.Any(), gomock.Any()).Return(nil).Times(2)

	// Nonce assignment
	repo.EXPECT().IncrementNonceCursor(gomock.Any(), pipelineSender.Hex(), uint64(1)).Return(uint64(5), nil)
	repo.EXPECT().MarkPending(gomock.Any(), "tx-001", uint64(5)).Return(nil)

	// Gas estimation
	toAddr := common.HexToAddress(pipelineTo)
	client.EXPECT().EstimateGas(gomock.Any(), toAddr, []byte(nil), tx.Value).Return(uint64(21000), nil)
	client.EXPECT().LatestBaseFee(gomock.Any()).Return(big.NewInt(30_000_000_000), nil)
	client.EXPECT().SuggestGasTipCap(gomock.Any()).Return(big.NewInt(2_000_000_000), nil)

	// Signing
	signer.EXPECT().Sign(gomock.Any(), pipelineSender, gomock.Any(), big.NewInt(1)).DoAndReturn(
		func(_ context.Context, _ common.Address, tx *types.Transaction, chainID *big.Int) (*types.Transaction, error) {
			// Return a properly signed tx using a test key
			return tx, nil // unsigned but sufficient for mock purposes
		},
	)

	// Store attempt
	repo.EXPECT().CreateAttempt(gomock.Any(), gomock.Any()).Return(nil)

	// Broadcast
	client.EXPECT().SendTransaction(gomock.Any(), gomock.Any()).Return(nil)

	// Mark submitted
	repo.EXPECT().MarkSubmitted(gomock.Any(), "tx-001", gomock.Any(), gomock.Nil()).Return(nil)

	p.processTx(ctx, tx, actor)
}

func TestPipeline_ProcessTx_NonceAssignmentFails(t *testing.T) {
	ctrl := gomock.NewController(t)
	p, repo, _, _ := newTestPipeline(ctrl)
	ctx := context.Background()
	tx := testTransaction()
	actor := "test-actor"

	// State transition log QUEUED->PENDING
	repo.EXPECT().LogStateTransition(gomock.Any(), gomock.Any()).Return(nil).AnyTimes()

	// Nonce assignment fails
	repo.EXPECT().IncrementNonceCursor(gomock.Any(), pipelineSender.Hex(), uint64(1)).Return(uint64(0), fmt.Errorf("db error"))

	// Should fail the transaction
	repo.EXPECT().MarkFailed(gomock.Any(), "tx-001", domain.ErrCodeInternalError, gomock.Any()).Return(nil)

	p.processTx(ctx, tx, actor)
}

func TestPipeline_ProcessTx_GasEstimationFails(t *testing.T) {
	ctrl := gomock.NewController(t)
	p, repo, client, _ := newTestPipeline(ctrl)
	ctx := context.Background()
	tx := testTransaction()
	actor := "test-actor"

	repo.EXPECT().LogStateTransition(gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
	repo.EXPECT().IncrementNonceCursor(gomock.Any(), pipelineSender.Hex(), uint64(1)).Return(uint64(5), nil)
	repo.EXPECT().MarkPending(gomock.Any(), "tx-001", uint64(5)).Return(nil)

	// Gas estimation fails
	toAddr := common.HexToAddress(pipelineTo)
	client.EXPECT().EstimateGas(gomock.Any(), toAddr, []byte(nil), tx.Value).Return(uint64(0), fmt.Errorf("execution reverted"))

	repo.EXPECT().MarkFailed(gomock.Any(), "tx-001", domain.ErrCodeEstimationReverted, gomock.Any()).Return(nil)

	p.processTx(ctx, tx, actor)
}

func TestPipeline_ProcessTx_SigningFails(t *testing.T) {
	ctrl := gomock.NewController(t)
	p, repo, client, signer := newTestPipeline(ctrl)
	ctx := context.Background()
	tx := testTransaction()
	actor := "test-actor"

	repo.EXPECT().LogStateTransition(gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
	repo.EXPECT().IncrementNonceCursor(gomock.Any(), pipelineSender.Hex(), uint64(1)).Return(uint64(5), nil)
	repo.EXPECT().MarkPending(gomock.Any(), "tx-001", uint64(5)).Return(nil)

	toAddr := common.HexToAddress(pipelineTo)
	client.EXPECT().EstimateGas(gomock.Any(), toAddr, []byte(nil), tx.Value).Return(uint64(21000), nil)
	client.EXPECT().LatestBaseFee(gomock.Any()).Return(big.NewInt(30_000_000_000), nil)
	client.EXPECT().SuggestGasTipCap(gomock.Any()).Return(big.NewInt(2_000_000_000), nil)

	// Signing fails
	signer.EXPECT().Sign(gomock.Any(), pipelineSender, gomock.Any(), big.NewInt(1)).Return(nil, fmt.Errorf("key not available"))

	repo.EXPECT().MarkFailed(gomock.Any(), "tx-001", domain.ErrCodeSigningFailed, gomock.Any()).Return(nil)

	p.processTx(ctx, tx, actor)
}

func TestPipeline_ProcessTx_BroadcastFailsAllRetries(t *testing.T) {
	ctrl := gomock.NewController(t)
	p, repo, client, signer := newTestPipeline(ctrl)

	// Use a short-lived context to avoid sleeping through retries
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	tx := testTransaction()
	actor := "test-actor"

	repo.EXPECT().LogStateTransition(gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
	repo.EXPECT().IncrementNonceCursor(gomock.Any(), pipelineSender.Hex(), uint64(1)).Return(uint64(5), nil)
	repo.EXPECT().MarkPending(gomock.Any(), "tx-001", uint64(5)).Return(nil)

	toAddr := common.HexToAddress(pipelineTo)
	client.EXPECT().EstimateGas(gomock.Any(), toAddr, []byte(nil), tx.Value).Return(uint64(21000), nil)
	client.EXPECT().LatestBaseFee(gomock.Any()).Return(big.NewInt(30_000_000_000), nil)
	client.EXPECT().SuggestGasTipCap(gomock.Any()).Return(big.NewInt(2_000_000_000), nil)

	signer.EXPECT().Sign(gomock.Any(), pipelineSender, gomock.Any(), big.NewInt(1)).DoAndReturn(
		func(_ context.Context, _ common.Address, tx *types.Transaction, _ *big.Int) (*types.Transaction, error) {
			return tx, nil
		},
	)

	repo.EXPECT().CreateAttempt(gomock.Any(), gomock.Any()).Return(nil)

	// Broadcast fails all 5 retries
	client.EXPECT().SendTransaction(gomock.Any(), gomock.Any()).Return(fmt.Errorf("connection refused")).Times(maxBroadcastRetries)

	repo.EXPECT().MarkFailed(gomock.Any(), "tx-001", domain.ErrCodeBroadcastFailed, gomock.Any()).Return(nil)

	p.processTx(ctx, tx, actor)
}

func TestPipeline_ProcessTx_BroadcastSucceedsOnRetry(t *testing.T) {
	ctrl := gomock.NewController(t)
	p, repo, client, signer := newTestPipeline(ctrl)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tx := testTransaction()
	actor := "test-actor"

	repo.EXPECT().LogStateTransition(gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
	repo.EXPECT().IncrementNonceCursor(gomock.Any(), pipelineSender.Hex(), uint64(1)).Return(uint64(5), nil)
	repo.EXPECT().MarkPending(gomock.Any(), "tx-001", uint64(5)).Return(nil)

	toAddr := common.HexToAddress(pipelineTo)
	client.EXPECT().EstimateGas(gomock.Any(), toAddr, []byte(nil), tx.Value).Return(uint64(21000), nil)
	client.EXPECT().LatestBaseFee(gomock.Any()).Return(big.NewInt(30_000_000_000), nil)
	client.EXPECT().SuggestGasTipCap(gomock.Any()).Return(big.NewInt(2_000_000_000), nil)

	signer.EXPECT().Sign(gomock.Any(), pipelineSender, gomock.Any(), big.NewInt(1)).DoAndReturn(
		func(_ context.Context, _ common.Address, tx *types.Transaction, _ *big.Int) (*types.Transaction, error) {
			return tx, nil
		},
	)

	repo.EXPECT().CreateAttempt(gomock.Any(), gomock.Any()).Return(nil)

	// Fail twice, then succeed
	gomock.InOrder(
		client.EXPECT().SendTransaction(gomock.Any(), gomock.Any()).Return(fmt.Errorf("temporary error")),
		client.EXPECT().SendTransaction(gomock.Any(), gomock.Any()).Return(fmt.Errorf("temporary error")),
		client.EXPECT().SendTransaction(gomock.Any(), gomock.Any()).Return(nil),
	)

	repo.EXPECT().MarkSubmitted(gomock.Any(), "tx-001", gomock.Any(), gomock.Nil()).Return(nil)

	p.processTx(ctx, tx, actor)
}

func TestPipeline_Run_ContextCancellation(t *testing.T) {
	ctrl := gomock.NewController(t)
	p, repo, _, _ := newTestPipeline(ctrl)

	ctx, cancel := context.WithCancel(context.Background())

	// When Run calls ClaimNextQueued, return nil (no work). Then cancel context.
	repo.EXPECT().ClaimNextQueued(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(
		func(ctx context.Context, sender string, chainID uint64, claimedBy string) (*domain.Transaction, error) {
			cancel() // cancel context to stop the pipeline
			return nil, nil
		},
	)

	// Run should exit cleanly
	done := make(chan struct{})
	go func() {
		p.Run(ctx)
		close(done)
	}()

	select {
	case <-done:
		// success
	case <-time.After(5 * time.Second):
		t.Fatal("pipeline did not stop after context cancellation")
	}
}

func TestPipeline_Notify(t *testing.T) {
	ctrl := gomock.NewController(t)
	p, _, _, _ := newTestPipeline(ctrl)

	// Should not block
	p.Notify()
	p.Notify() // second call should not block either (buffered channel)

	// Drain
	select {
	case <-p.notify:
	default:
		t.Fatal("expected notification in channel")
	}
}

func TestPipeline_ProcessTx_CorrectNonceInTransaction(t *testing.T) {
	ctrl := gomock.NewController(t)
	p, repo, client, signer := newTestPipeline(ctrl)
	ctx := context.Background()
	tx := testTransaction()
	actor := "test-actor"

	repo.EXPECT().LogStateTransition(gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
	repo.EXPECT().IncrementNonceCursor(gomock.Any(), pipelineSender.Hex(), uint64(1)).Return(uint64(42), nil)
	repo.EXPECT().MarkPending(gomock.Any(), "tx-001", uint64(42)).Return(nil)

	toAddr := common.HexToAddress(pipelineTo)
	client.EXPECT().EstimateGas(gomock.Any(), toAddr, []byte(nil), tx.Value).Return(uint64(21000), nil)
	client.EXPECT().LatestBaseFee(gomock.Any()).Return(big.NewInt(30_000_000_000), nil)
	client.EXPECT().SuggestGasTipCap(gomock.Any()).Return(big.NewInt(2_000_000_000), nil)

	// Verify the signed transaction has the correct nonce
	signer.EXPECT().Sign(gomock.Any(), pipelineSender, gomock.Any(), big.NewInt(1)).DoAndReturn(
		func(_ context.Context, _ common.Address, ethTx *types.Transaction, _ *big.Int) (*types.Transaction, error) {
			assert.Equal(t, uint64(42), ethTx.Nonce())
			return ethTx, nil
		},
	)

	repo.EXPECT().CreateAttempt(gomock.Any(), gomock.Any()).Return(nil)
	client.EXPECT().SendTransaction(gomock.Any(), gomock.Any()).Return(nil)
	repo.EXPECT().MarkSubmitted(gomock.Any(), "tx-001", gomock.Any(), gomock.Nil()).Return(nil)

	p.processTx(ctx, tx, actor)
}
