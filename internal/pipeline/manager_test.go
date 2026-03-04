package pipeline

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/apm-dev/evm-tx-sender/internal/domain"
	"github.com/apm-dev/evm-tx-sender/internal/infrastructure/ethereum"
	"github.com/apm-dev/evm-tx-sender/internal/mocks"
	"github.com/ethereum/go-ethereum/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
)

// addrHex returns the canonical lower-case hex representation of an address,
// matching the normalization used by the production pipeline code.
func addrHex(addr common.Address) string {
	return strings.ToLower(addr.Hex())
}

func newTestManager(ctrl *gomock.Controller) (*Manager, *mocks.MockRepository, *mocks.MockEthClient, *mocks.MockSigner) {
	repo := mocks.NewMockRepository(ctrl)
	client := mocks.NewMockEthClient(ctrl)
	signer := mocks.NewMockSigner(ctrl)
	gas := ethereum.NewGasEngine()
	chains := map[uint64]*domain.ChainConfig{
		1: testChainConfig(),
	}
	clients := map[uint64]domain.EthClient{
		1: client,
	}
	log := slog.Default()

	m := NewManager(repo, clients, signer, gas, chains, log)
	return m, repo, client, signer
}

func TestManager_InitNonces_ResetsPendingToQueued(t *testing.T) {
	ctrl := gomock.NewController(t)
	m, repo, client, signer := newTestManager(ctrl)
	ctx := context.Background()

	// Reset PENDING -> QUEUED (crash recovery)
	repo.EXPECT().ResetPendingToQueued(gomock.Any()).Return(3, nil)

	addr := common.HexToAddress("0xAb5801a7D398351b8bE11C439e05C5B3259aeC9B")
	signer.EXPECT().Addresses().Return([]common.Address{addr})

	// On-chain nonce
	client.EXPECT().PendingNonceAt(gomock.Any(), addr).Return(uint64(10), nil)

	// DB cursor doesn't exist yet
	repo.EXPECT().GetNonceCursor(gomock.Any(), addrHex(addr), uint64(1)).Return(nil, nil)
	repo.EXPECT().InitNonceCursor(gomock.Any(), addrHex(addr), uint64(1), uint64(10)).Return(nil)

	err := m.InitNonces(ctx)
	require.NoError(t, err)
}

func TestManager_InitNonces_DBAheadOfOnChain_AlignsToOnChain(t *testing.T) {
	ctrl := gomock.NewController(t)
	m, repo, client, signer := newTestManager(ctrl)
	ctx := context.Background()

	repo.EXPECT().ResetPendingToQueued(gomock.Any()).Return(0, nil)

	addr := common.HexToAddress("0xAb5801a7D398351b8bE11C439e05C5B3259aeC9B")
	signer.EXPECT().Addresses().Return([]common.Address{addr})

	// On-chain nonce is 10
	client.EXPECT().PendingNonceAt(gomock.Any(), addr).Return(uint64(10), nil)

	// DB cursor is ahead at 15 (txs were assigned but never broadcast before crash)
	repo.EXPECT().GetNonceCursor(gomock.Any(), addrHex(addr), uint64(1)).Return(&domain.NonceCursor{
		Sender:    addrHex(addr),
		ChainID:   1,
		NextNonce: 15,
		UpdatedAt: time.Now(),
	}, nil)

	// Should align to on-chain (10), not keep DB value (15)
	repo.EXPECT().SetNonceCursor(gomock.Any(), addrHex(addr), uint64(1), uint64(10)).Return(nil)

	err := m.InitNonces(ctx)
	require.NoError(t, err)
}

func TestManager_InitNonces_DBBehindOnChain_UsesOnChain(t *testing.T) {
	ctrl := gomock.NewController(t)
	m, repo, client, signer := newTestManager(ctrl)
	ctx := context.Background()

	repo.EXPECT().ResetPendingToQueued(gomock.Any()).Return(0, nil)

	addr := common.HexToAddress("0xAb5801a7D398351b8bE11C439e05C5B3259aeC9B")
	signer.EXPECT().Addresses().Return([]common.Address{addr})

	// On-chain nonce is 20
	client.EXPECT().PendingNonceAt(gomock.Any(), addr).Return(uint64(20), nil)

	// DB cursor is behind at 15
	repo.EXPECT().GetNonceCursor(gomock.Any(), addrHex(addr), uint64(1)).Return(&domain.NonceCursor{
		Sender:    addrHex(addr),
		ChainID:   1,
		NextNonce: 15,
		UpdatedAt: time.Now(),
	}, nil)

	// Should use on-chain (20)
	repo.EXPECT().SetNonceCursor(gomock.Any(), addrHex(addr), uint64(1), uint64(20)).Return(nil)

	err := m.InitNonces(ctx)
	require.NoError(t, err)
}

func TestManager_InitNonces_FreshCursorInit(t *testing.T) {
	ctrl := gomock.NewController(t)
	m, repo, client, signer := newTestManager(ctrl)
	ctx := context.Background()

	repo.EXPECT().ResetPendingToQueued(gomock.Any()).Return(0, nil)

	addr := common.HexToAddress("0xAb5801a7D398351b8bE11C439e05C5B3259aeC9B")
	signer.EXPECT().Addresses().Return([]common.Address{addr})

	client.EXPECT().PendingNonceAt(gomock.Any(), addr).Return(uint64(0), nil)

	// No cursor exists
	repo.EXPECT().GetNonceCursor(gomock.Any(), addrHex(addr), uint64(1)).Return(nil, nil)
	repo.EXPECT().InitNonceCursor(gomock.Any(), addrHex(addr), uint64(1), uint64(0)).Return(nil)

	err := m.InitNonces(ctx)
	require.NoError(t, err)
}

func TestManager_InitNonces_ResetPendingFails(t *testing.T) {
	ctrl := gomock.NewController(t)
	m, repo, _, _ := newTestManager(ctrl)
	ctx := context.Background()

	repo.EXPECT().ResetPendingToQueued(gomock.Any()).Return(0, fmt.Errorf("db error"))

	err := m.InitNonces(ctx)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "reset pending")
}

func TestManager_InitNonces_PendingNonceFails(t *testing.T) {
	ctrl := gomock.NewController(t)
	m, repo, client, signer := newTestManager(ctrl)
	ctx := context.Background()

	repo.EXPECT().ResetPendingToQueued(gomock.Any()).Return(0, nil)

	addr := common.HexToAddress("0xAb5801a7D398351b8bE11C439e05C5B3259aeC9B")
	signer.EXPECT().Addresses().Return([]common.Address{addr})

	client.EXPECT().PendingNonceAt(gomock.Any(), addr).Return(uint64(0), fmt.Errorf("rpc error"))

	err := m.InitNonces(ctx)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "pending nonce")
}

func TestManager_InitNonces_MultipleSendersMultipleChains(t *testing.T) {
	ctrl := gomock.NewController(t)
	repo := mocks.NewMockRepository(ctrl)
	client1 := mocks.NewMockEthClient(ctrl)
	client2 := mocks.NewMockEthClient(ctrl)
	signer := mocks.NewMockSigner(ctrl)
	gas := ethereum.NewGasEngine()

	chains := map[uint64]*domain.ChainConfig{
		1:   testChainConfig(),
		137: testChainConfig(),
	}
	clients := map[uint64]domain.EthClient{
		1:   client1,
		137: client2,
	}
	log := slog.Default()
	m := NewManager(repo, clients, signer, gas, chains, log)

	repo.EXPECT().ResetPendingToQueued(gomock.Any()).Return(0, nil)

	addr1 := common.HexToAddress("0xAb5801a7D398351b8bE11C439e05C5B3259aeC9B")
	addr2 := common.HexToAddress("0x1234567890abcdef1234567890abcdef12345678")
	signer.EXPECT().Addresses().Return([]common.Address{addr1, addr2})

	// 2 senders x 2 chains = 4 nonce init calls
	for _, addr := range []common.Address{addr1, addr2} {
		client1.EXPECT().PendingNonceAt(gomock.Any(), addr).Return(uint64(5), nil)
		client2.EXPECT().PendingNonceAt(gomock.Any(), addr).Return(uint64(10), nil)

		repo.EXPECT().GetNonceCursor(gomock.Any(), addrHex(addr), uint64(1)).Return(nil, nil)
		repo.EXPECT().GetNonceCursor(gomock.Any(), addrHex(addr), uint64(137)).Return(nil, nil)

		repo.EXPECT().InitNonceCursor(gomock.Any(), addrHex(addr), uint64(1), uint64(5)).Return(nil)
		repo.EXPECT().InitNonceCursor(gomock.Any(), addrHex(addr), uint64(137), uint64(10)).Return(nil)
	}

	err := m.InitNonces(context.Background())
	require.NoError(t, err)
}

func TestManager_NotifyPipeline_ExistingPipeline(t *testing.T) {
	ctrl := gomock.NewController(t)
	m, _, _, _ := newTestManager(ctrl)

	addr := common.HexToAddress("0xAb5801a7D398351b8bE11C439e05C5B3259aeC9B")
	key := fmt.Sprintf("%s-%d", addrHex(addr), 1)

	// Manually add a pipeline to the manager
	p := &Pipeline{notify: make(chan struct{}, 1)}
	m.pipelines[key] = p

	// NotifyPipeline should not panic even with unknown keys
	m.NotifyPipeline(addrHex(addr), 1)

	select {
	case <-p.notify:
		// success
	default:
		t.Fatal("pipeline was not notified")
	}
}

func TestManager_NotifyPipeline_NonExistentPipeline(t *testing.T) {
	ctrl := gomock.NewController(t)
	m, _, _, _ := newTestManager(ctrl)

	// Should not panic
	m.NotifyPipeline("0xunknown", 999)
}

func TestManager_PipelineStatus(t *testing.T) {
	ctrl := gomock.NewController(t)
	m, repo, _, _ := newTestManager(ctrl)

	addr := common.HexToAddress("0xAb5801a7D398351b8bE11C439e05C5B3259aeC9B")
	key := fmt.Sprintf("%s-%d", addrHex(addr), 1)

	p := &Pipeline{
		sender:  addr,
		chainID: 1,
		notify:  make(chan struct{}, 1),
	}
	m.pipelines[key] = p

	repo.EXPECT().CountQueuedTransactions(gomock.Any(), addrHex(addr), uint64(1)).Return(5, nil)

	status := m.PipelineStatus(context.Background())
	assert.Len(t, status, 1)
	assert.Equal(t, "running", status[key].Status)
	assert.Equal(t, 5, status[key].QueueDepth)
}
