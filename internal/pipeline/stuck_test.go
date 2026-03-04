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

func newTestStuckDetector(ctrl *gomock.Controller) (*StuckDetector, *mocks.MockRepository, *mocks.MockEthClient, *mocks.MockSigner) {
	repo := mocks.NewMockRepository(ctrl)
	client := mocks.NewMockEthClient(ctrl)
	signer := mocks.NewMockSigner(ctrl)
	gas := ethereum.NewGasEngine()
	chain := testChainConfig()
	log := slog.Default()

	sd := NewStuckDetector(chain, repo, client, signer, gas, 30*time.Second, log)
	return sd, repo, client, signer
}

func stuckTransaction(id string, submittedAgo time.Duration) *domain.Transaction {
	now := time.Now().UTC()
	submittedAt := now.Add(-submittedAgo)
	nonce := uint64(10)
	return &domain.Transaction{
		ID:          id,
		ChainID:     1,
		Sender:      pipelineSender.Hex(),
		ToAddress:   pipelineTo,
		Value:       big.NewInt(1_000_000_000_000_000_000),
		Status:      domain.TxStatusSubmitted,
		FinalTxHash: "0xoriginalhash",
		Nonce:       &nonce,
		SubmittedAt: &submittedAt,
	}
}

func TestStuckDetector_Check_TransactionPastThreshold_GasBumped(t *testing.T) {
	ctrl := gomock.NewController(t)
	sd, repo, client, signer := newTestStuckDetector(ctrl)
	ctx := context.Background()

	tx := stuckTransaction("tx-stuck-1", 5*time.Minute) // older than 3min threshold

	repo.EXPECT().GetSubmittedTransactions(gomock.Any(), uint64(1), gomock.Any(), gomock.Any()).Return([]*domain.Transaction{tx}, nil)

	lastAttemptCreated := time.Now().UTC().Add(-2 * time.Minute) // older than GasBumpInterval (30s)
	repo.EXPECT().GetAttemptsByTransactionID(gomock.Any(), "tx-stuck-1").Return([]*domain.TxAttempt{
		{
			ID:                   "att-1",
			TransactionID:        "tx-stuck-1",
			AttemptNumber:        1,
			TxHash:               "0xoriginalhash",
			GasLimit:             25200,
			MaxFeePerGas:         big.NewInt(62_000_000_000),
			MaxPriorityFeePerGas: big.NewInt(2_000_000_000),
			TxType:               2,
			Status:               domain.AttemptBroadcast,
			CreatedAt:            lastAttemptCreated,
		},
	}, nil)

	// Sign the bumped tx
	signer.EXPECT().Sign(gomock.Any(), common.HexToAddress(tx.Sender), gomock.Any(), big.NewInt(1)).DoAndReturn(
		func(_ context.Context, _ common.Address, ethTx *types.Transaction, _ *big.Int) (*types.Transaction, error) {
			// Verify nonce is preserved
			assert.Equal(t, uint64(10), ethTx.Nonce())
			// Verify gas was bumped (15% for bump 1)
			return ethTx, nil
		},
	)

	// Store new attempt
	repo.EXPECT().CreateAttempt(gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, a *domain.TxAttempt) error {
			assert.Equal(t, 2, a.AttemptNumber)
			assert.Equal(t, domain.AttemptBroadcast, a.Status)
			// Verify 15% bump on MaxFeePerGas: 62 * 1.15 = 71.3 gwei
			expectedMaxFee := new(big.Int).Add(
				big.NewInt(62_000_000_000),
				new(big.Int).Div(new(big.Int).Mul(big.NewInt(62_000_000_000), big.NewInt(15)), big.NewInt(100)),
			)
			assert.Equal(t, expectedMaxFee.String(), a.MaxFeePerGas.String())
			return nil
		},
	)

	// Broadcast bumped tx
	client.EXPECT().SendTransaction(gomock.Any(), gomock.Any()).Return(nil)

	// Update main transaction hash
	repo.EXPECT().MarkSubmitted(gomock.Any(), "tx-stuck-1", gomock.Any(), gomock.Nil()).Return(nil)
	repo.EXPECT().LogStateTransition(gomock.Any(), gomock.Any()).Return(nil)

	sd.check(ctx)
}

func TestStuckDetector_Check_MaxBumpsExceeded_MarksFailed(t *testing.T) {
	ctrl := gomock.NewController(t)
	sd, repo, _, _ := newTestStuckDetector(ctrl)
	ctx := context.Background()

	tx := stuckTransaction("tx-stuck-2", 30*time.Minute)

	repo.EXPECT().GetSubmittedTransactions(gomock.Any(), uint64(1), gomock.Any(), gomock.Any()).Return([]*domain.Transaction{tx}, nil)

	// More than maxGasBumps (5) attempts
	attempts := make([]*domain.TxAttempt, maxGasBumps+1)
	for i := range attempts {
		attempts[i] = &domain.TxAttempt{
			ID:            fmt.Sprintf("att-%d", i),
			AttemptNumber: i + 1,
			TxHash:        fmt.Sprintf("0xhash_%d", i),
			Status:        domain.AttemptBroadcast,
			CreatedAt:     time.Now().UTC().Add(-time.Duration(maxGasBumps-i) * time.Minute),
		}
	}

	repo.EXPECT().GetAttemptsByTransactionID(gomock.Any(), "tx-stuck-2").Return(attempts, nil)
	repo.EXPECT().MarkFailed(gomock.Any(), "tx-stuck-2", domain.ErrCodeMaxRetriesExceeded, gomock.Any()).Return(nil)
	repo.EXPECT().LogStateTransition(gomock.Any(), gomock.Any()).Return(nil)

	sd.check(ctx)
}

func TestStuckDetector_Check_NotPastThreshold_Skipped(t *testing.T) {
	ctrl := gomock.NewController(t)
	sd, repo, _, _ := newTestStuckDetector(ctrl)
	ctx := context.Background()

	// Submitted only 1 minute ago, threshold is 3 minutes
	tx := stuckTransaction("tx-fresh", 1*time.Minute)

	repo.EXPECT().GetSubmittedTransactions(gomock.Any(), uint64(1), gomock.Any(), gomock.Any()).Return([]*domain.Transaction{tx}, nil)

	// Should not call GetAttemptsByTransactionID or any gas bump operations
	sd.check(ctx)
}

func TestStuckDetector_Check_GasBumpIntervalNotElapsed_Skipped(t *testing.T) {
	ctrl := gomock.NewController(t)
	sd, repo, _, _ := newTestStuckDetector(ctrl)
	ctx := context.Background()

	tx := stuckTransaction("tx-stuck-3", 5*time.Minute)

	repo.EXPECT().GetSubmittedTransactions(gomock.Any(), uint64(1), gomock.Any(), gomock.Any()).Return([]*domain.Transaction{tx}, nil)

	// Last attempt was only 10 seconds ago (GasBumpInterval is 30s)
	repo.EXPECT().GetAttemptsByTransactionID(gomock.Any(), "tx-stuck-3").Return([]*domain.TxAttempt{
		{
			ID:                   "att-1",
			AttemptNumber:        1,
			TxHash:               "0xhash",
			GasLimit:             25200,
			MaxFeePerGas:         big.NewInt(62_000_000_000),
			MaxPriorityFeePerGas: big.NewInt(2_000_000_000),
			TxType:               2,
			Status:               domain.AttemptBroadcast,
			CreatedAt:            time.Now().UTC().Add(-10 * time.Second), // recent
		},
	}, nil)

	// Should not sign, broadcast, or store anything
	sd.check(ctx)
}

func TestStuckDetector_Check_NilNonce_Skipped(t *testing.T) {
	ctrl := gomock.NewController(t)
	sd, repo, _, _ := newTestStuckDetector(ctrl)
	ctx := context.Background()

	tx := stuckTransaction("tx-stuck-4", 5*time.Minute)
	tx.Nonce = nil // no nonce assigned

	repo.EXPECT().GetSubmittedTransactions(gomock.Any(), uint64(1), gomock.Any(), gomock.Any()).Return([]*domain.Transaction{tx}, nil)

	repo.EXPECT().GetAttemptsByTransactionID(gomock.Any(), "tx-stuck-4").Return([]*domain.TxAttempt{
		{
			ID:                   "att-1",
			AttemptNumber:        1,
			GasLimit:             25200,
			MaxFeePerGas:         big.NewInt(62_000_000_000),
			MaxPriorityFeePerGas: big.NewInt(2_000_000_000),
			TxType:               2,
			Status:               domain.AttemptBroadcast,
			CreatedAt:            time.Now().UTC().Add(-2 * time.Minute),
		},
	}, nil)

	// bumpGas should return early when nonce is nil
	sd.check(ctx)
}

func TestStuckDetector_Check_NilSubmittedAt_Skipped(t *testing.T) {
	ctrl := gomock.NewController(t)
	sd, repo, _, _ := newTestStuckDetector(ctrl)
	ctx := context.Background()

	tx := stuckTransaction("tx-stuck-5", 5*time.Minute)
	tx.SubmittedAt = nil // no submitted_at

	repo.EXPECT().GetSubmittedTransactions(gomock.Any(), uint64(1), gomock.Any(), gomock.Any()).Return([]*domain.Transaction{tx}, nil)

	// Should be skipped in the loop
	sd.check(ctx)
}

func TestStuckDetector_Check_SigningFailure_Continues(t *testing.T) {
	ctrl := gomock.NewController(t)
	sd, repo, _, signer := newTestStuckDetector(ctrl)
	ctx := context.Background()

	tx := stuckTransaction("tx-stuck-6", 5*time.Minute)

	repo.EXPECT().GetSubmittedTransactions(gomock.Any(), uint64(1), gomock.Any(), gomock.Any()).Return([]*domain.Transaction{tx}, nil)
	repo.EXPECT().GetAttemptsByTransactionID(gomock.Any(), "tx-stuck-6").Return([]*domain.TxAttempt{
		{
			ID:                   "att-1",
			AttemptNumber:        1,
			GasLimit:             25200,
			MaxFeePerGas:         big.NewInt(62_000_000_000),
			MaxPriorityFeePerGas: big.NewInt(2_000_000_000),
			TxType:               2,
			Status:               domain.AttemptBroadcast,
			CreatedAt:            time.Now().UTC().Add(-2 * time.Minute),
		},
	}, nil)

	signer.EXPECT().Sign(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, fmt.Errorf("HSM unavailable"))

	// Should log error and return, not panic
	sd.check(ctx)
}

func TestStuckDetector_Check_BroadcastFailure_NonFatal(t *testing.T) {
	ctrl := gomock.NewController(t)
	sd, repo, client, signer := newTestStuckDetector(ctrl)
	ctx := context.Background()

	tx := stuckTransaction("tx-stuck-7", 5*time.Minute)

	repo.EXPECT().GetSubmittedTransactions(gomock.Any(), uint64(1), gomock.Any(), gomock.Any()).Return([]*domain.Transaction{tx}, nil)
	repo.EXPECT().GetAttemptsByTransactionID(gomock.Any(), "tx-stuck-7").Return([]*domain.TxAttempt{
		{
			ID:                   "att-1",
			AttemptNumber:        1,
			GasLimit:             25200,
			MaxFeePerGas:         big.NewInt(62_000_000_000),
			MaxPriorityFeePerGas: big.NewInt(2_000_000_000),
			TxType:               2,
			Status:               domain.AttemptBroadcast,
			CreatedAt:            time.Now().UTC().Add(-2 * time.Minute),
		},
	}, nil)

	signer.EXPECT().Sign(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, _ common.Address, ethTx *types.Transaction, _ *big.Int) (*types.Transaction, error) {
			return ethTx, nil
		},
	)

	repo.EXPECT().CreateAttempt(gomock.Any(), gomock.Any()).Return(nil)

	// Broadcast fails -- should be non-fatal
	client.EXPECT().SendTransaction(gomock.Any(), gomock.Any()).Return(fmt.Errorf("connection refused"))

	// Should not call MarkSubmitted since broadcast failed
	sd.check(ctx)
}

func TestStuckDetector_Check_LegacyTxBump(t *testing.T) {
	ctrl := gomock.NewController(t)
	sd, repo, client, signer := newTestStuckDetector(ctrl)
	ctx := context.Background()

	tx := stuckTransaction("tx-stuck-8", 5*time.Minute)

	repo.EXPECT().GetSubmittedTransactions(gomock.Any(), uint64(1), gomock.Any(), gomock.Any()).Return([]*domain.Transaction{tx}, nil)
	repo.EXPECT().GetAttemptsByTransactionID(gomock.Any(), "tx-stuck-8").Return([]*domain.TxAttempt{
		{
			ID:            "att-1",
			AttemptNumber: 1,
			GasLimit:      25200,
			GasPrice:      big.NewInt(20_000_000_000),
			TxType:        0, // legacy
			Status:        domain.AttemptBroadcast,
			CreatedAt:     time.Now().UTC().Add(-2 * time.Minute),
		},
	}, nil)

	signer.EXPECT().Sign(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, _ common.Address, ethTx *types.Transaction, _ *big.Int) (*types.Transaction, error) {
			assert.Equal(t, uint64(10), ethTx.Nonce())
			return ethTx, nil
		},
	)

	repo.EXPECT().CreateAttempt(gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, a *domain.TxAttempt) error {
			assert.Equal(t, 0, a.TxType)
			// 15% bump on gas price
			expectedPrice := new(big.Int).Add(
				big.NewInt(20_000_000_000),
				new(big.Int).Div(new(big.Int).Mul(big.NewInt(20_000_000_000), big.NewInt(15)), big.NewInt(100)),
			)
			assert.Equal(t, expectedPrice.String(), a.GasPrice.String())
			return nil
		},
	)

	client.EXPECT().SendTransaction(gomock.Any(), gomock.Any()).Return(nil)
	repo.EXPECT().MarkSubmitted(gomock.Any(), "tx-stuck-8", gomock.Any(), gomock.Nil()).Return(nil)
	repo.EXPECT().LogStateTransition(gomock.Any(), gomock.Any()).Return(nil)

	sd.check(ctx)
}

func TestStuckDetector_Run_StopsOnContextCancel(t *testing.T) {
	ctrl := gomock.NewController(t)
	sd, _, _, _ := newTestStuckDetector(ctrl)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan struct{})
	go func() {
		sd.Run(ctx)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("stuck detector did not stop after context cancellation")
	}
}

func TestStuckDetector_Check_DBErrorOnGetSubmitted(t *testing.T) {
	ctrl := gomock.NewController(t)
	sd, repo, _, _ := newTestStuckDetector(ctrl)
	ctx := context.Background()

	repo.EXPECT().GetSubmittedTransactions(gomock.Any(), uint64(1), gomock.Any(), gomock.Any()).Return(nil, fmt.Errorf("db error"))

	// Should not panic
	sd.check(ctx)
}
