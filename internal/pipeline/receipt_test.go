package pipeline

import (
	"context"
	"fmt"
	"log/slog"
	"math/big"
	"testing"
	"time"

	"github.com/apm-dev/evm-tx-sender/internal/domain"
	"github.com/apm-dev/evm-tx-sender/internal/mocks"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"
)

func newTestReceiptPoller(ctrl *gomock.Controller) (*ReceiptPoller, *mocks.MockRepository, *mocks.MockEthClient) {
	repo := mocks.NewMockRepository(ctrl)
	client := mocks.NewMockEthClient(ctrl)
	log := slog.Default()

	rp := NewReceiptPoller(1, repo, client, 2*time.Second, log)
	return rp, repo, client
}

func submittedTx(id, txHash string) *domain.Transaction {
	now := time.Now().UTC()
	nonce := uint64(5)
	return &domain.Transaction{
		ID:          id,
		ChainID:     1,
		Sender:      pipelineSender.Hex(),
		ToAddress:   pipelineTo,
		Value:       big.NewInt(1_000_000_000_000_000_000),
		Status:      domain.TxStatusSubmitted,
		FinalTxHash: txHash,
		Nonce:       &nonce,
		SubmittedAt: &now,
	}
}

func TestReceiptPoller_Poll_ReceiptConfirmed(t *testing.T) {
	ctrl := gomock.NewController(t)
	rp, repo, client := newTestReceiptPoller(ctrl)
	ctx := context.Background()

	tx := submittedTx("tx-001", "0xabc123")

	repo.EXPECT().GetSubmittedTransactions(gomock.Any(), uint64(1)).Return([]*domain.Transaction{tx}, nil)

	receipt := &types.Receipt{
		Status:            1,
		BlockNumber:       big.NewInt(1000),
		BlockHash:         common.HexToHash("0xblockhash"),
		GasUsed:           21000,
		EffectiveGasPrice: big.NewInt(30_000_000_000),
		TxHash:            common.HexToHash("0xabc123"),
	}
	client.EXPECT().TransactionReceipt(gomock.Any(), common.HexToHash("0xabc123")).Return(receipt, nil)

	repo.EXPECT().MarkConfirmed(gomock.Any(), "tx-001", gomock.Any()).DoAndReturn(
		func(_ context.Context, id string, r *domain.TxReceipt) error {
			assert.Equal(t, uint8(1), r.Status)
			assert.Equal(t, uint64(1000), r.BlockNumber)
			assert.Equal(t, uint64(21000), r.GasUsed)
			return nil
		},
	)
	repo.EXPECT().LogStateTransition(gomock.Any(), gomock.Any()).Return(nil)

	// Mark attempt confirmed
	repo.EXPECT().GetAttemptsByTransactionID(gomock.Any(), "tx-001").Return([]*domain.TxAttempt{
		{ID: "att-1", TxHash: "0xabc123", Status: domain.AttemptBroadcast},
	}, nil)
	repo.EXPECT().MarkAttemptStatus(gomock.Any(), "att-1", domain.AttemptConfirmed).Return(nil)

	rp.poll(ctx)
}

func TestReceiptPoller_Poll_ReceiptReverted(t *testing.T) {
	ctrl := gomock.NewController(t)
	rp, repo, client := newTestReceiptPoller(ctrl)
	ctx := context.Background()

	tx := submittedTx("tx-002", "0xdef456")

	repo.EXPECT().GetSubmittedTransactions(gomock.Any(), uint64(1)).Return([]*domain.Transaction{tx}, nil)

	receipt := &types.Receipt{
		Status:            0, // reverted
		BlockNumber:       big.NewInt(1001),
		BlockHash:         common.HexToHash("0xblockhash2"),
		GasUsed:           21000,
		EffectiveGasPrice: big.NewInt(30_000_000_000),
		TxHash:            common.HexToHash("0xdef456"),
	}
	client.EXPECT().TransactionReceipt(gomock.Any(), common.HexToHash("0xdef456")).Return(receipt, nil)

	repo.EXPECT().MarkReverted(gomock.Any(), "tx-002", gomock.Any()).DoAndReturn(
		func(_ context.Context, id string, r *domain.TxReceipt) error {
			assert.Equal(t, uint8(0), r.Status)
			return nil
		},
	)
	repo.EXPECT().LogStateTransition(gomock.Any(), gomock.Any()).Return(nil)

	rp.poll(ctx)
}

func TestReceiptPoller_Poll_NoReceiptYet(t *testing.T) {
	ctrl := gomock.NewController(t)
	rp, repo, client := newTestReceiptPoller(ctrl)
	ctx := context.Background()

	tx := submittedTx("tx-003", "0xfoo789")

	repo.EXPECT().GetSubmittedTransactions(gomock.Any(), uint64(1)).Return([]*domain.Transaction{tx}, nil)

	// Receipt not available
	client.EXPECT().TransactionReceipt(gomock.Any(), common.HexToHash("0xfoo789")).Return(nil, fmt.Errorf("not found"))

	// No state changes should happen -- no MarkConfirmed, MarkReverted, etc.
	rp.poll(ctx)
}

func TestReceiptPoller_Poll_RPCErrorOnGetSubmitted(t *testing.T) {
	ctrl := gomock.NewController(t)
	rp, repo, _ := newTestReceiptPoller(ctrl)
	ctx := context.Background()

	repo.EXPECT().GetSubmittedTransactions(gomock.Any(), uint64(1)).Return(nil, fmt.Errorf("db error"))

	// Should not panic, just log and return
	rp.poll(ctx)
}

func TestReceiptPoller_Poll_EmptyTxHash(t *testing.T) {
	ctrl := gomock.NewController(t)
	rp, repo, _ := newTestReceiptPoller(ctrl)
	ctx := context.Background()

	// Transaction with empty FinalTxHash should be skipped
	tx := submittedTx("tx-004", "")

	repo.EXPECT().GetSubmittedTransactions(gomock.Any(), uint64(1)).Return([]*domain.Transaction{tx}, nil)

	// No receipt fetch should happen
	rp.poll(ctx)
}

func TestReceiptPoller_Poll_MultipleTransactions(t *testing.T) {
	ctrl := gomock.NewController(t)
	rp, repo, client := newTestReceiptPoller(ctrl)
	ctx := context.Background()

	tx1 := submittedTx("tx-005", "0xhash1")
	tx2 := submittedTx("tx-006", "0xhash2")

	repo.EXPECT().GetSubmittedTransactions(gomock.Any(), uint64(1)).Return([]*domain.Transaction{tx1, tx2}, nil)

	// tx1: confirmed
	receipt1 := &types.Receipt{
		Status:            1,
		BlockNumber:       big.NewInt(500),
		BlockHash:         common.HexToHash("0xbh1"),
		GasUsed:           21000,
		EffectiveGasPrice: big.NewInt(20_000_000_000),
		TxHash:            common.HexToHash("0xhash1"),
	}
	client.EXPECT().TransactionReceipt(gomock.Any(), common.HexToHash("0xhash1")).Return(receipt1, nil)
	repo.EXPECT().MarkConfirmed(gomock.Any(), "tx-005", gomock.Any()).Return(nil)
	repo.EXPECT().LogStateTransition(gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
	repo.EXPECT().GetAttemptsByTransactionID(gomock.Any(), "tx-005").Return(nil, nil)

	// tx2: no receipt yet
	client.EXPECT().TransactionReceipt(gomock.Any(), common.HexToHash("0xhash2")).Return(nil, fmt.Errorf("not found"))

	rp.poll(ctx)
}

func TestReceiptPoller_MarkAttemptConfirmed_ReplacesOthers(t *testing.T) {
	ctrl := gomock.NewController(t)
	rp, repo, client := newTestReceiptPoller(ctrl)
	ctx := context.Background()

	tx := submittedTx("tx-007", "0xhash_bump2")

	repo.EXPECT().GetSubmittedTransactions(gomock.Any(), uint64(1)).Return([]*domain.Transaction{tx}, nil)

	receipt := &types.Receipt{
		Status:            1,
		BlockNumber:       big.NewInt(2000),
		BlockHash:         common.HexToHash("0xbh"),
		GasUsed:           21000,
		EffectiveGasPrice: big.NewInt(40_000_000_000),
		TxHash:            common.HexToHash("0xhash_bump2"),
	}
	client.EXPECT().TransactionReceipt(gomock.Any(), common.HexToHash("0xhash_bump2")).Return(receipt, nil)
	repo.EXPECT().MarkConfirmed(gomock.Any(), "tx-007", gomock.Any()).Return(nil)
	repo.EXPECT().LogStateTransition(gomock.Any(), gomock.Any()).Return(nil)

	// Multiple attempts: the confirmed one + an older replaced one
	repo.EXPECT().GetAttemptsByTransactionID(gomock.Any(), "tx-007").Return([]*domain.TxAttempt{
		{ID: "att-old", TxHash: "0xhash_original", Status: domain.AttemptBroadcast},
		{ID: "att-new", TxHash: "0xhash_bump2", Status: domain.AttemptBroadcast},
	}, nil)

	// The confirmed attempt should be marked confirmed
	repo.EXPECT().MarkAttemptStatus(gomock.Any(), "att-new", domain.AttemptConfirmed).Return(nil)
	// The old attempt should be marked replaced
	repo.EXPECT().MarkAttemptStatus(gomock.Any(), "att-old", domain.AttemptReplaced).Return(nil)

	rp.poll(ctx)
}

func TestReceiptPoller_Run_StopsOnContextCancel(t *testing.T) {
	ctrl := gomock.NewController(t)
	rp, _, _ := newTestReceiptPoller(ctrl)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	done := make(chan struct{})
	go func() {
		rp.Run(ctx)
		close(done)
	}()

	select {
	case <-done:
		// success
	case <-time.After(5 * time.Second):
		t.Fatal("receipt poller did not stop after context cancellation")
	}
}
