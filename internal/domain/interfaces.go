package domain

//go:generate mockgen -destination=../mocks/mock_repository.go -package=mocks github.com/apm-dev/evm-tx-sender/internal/domain Repository
//go:generate mockgen -destination=../mocks/mock_ethclient.go -package=mocks github.com/apm-dev/evm-tx-sender/internal/domain EthClient
//go:generate mockgen -destination=../mocks/mock_signer.go -package=mocks github.com/apm-dev/evm-tx-sender/internal/domain Signer

import (
	"context"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

type Repository interface {
	// Migrations
	Migrate(ctx context.Context) error

	// Transactions
	CreateTransaction(ctx context.Context, tx *Transaction) error
	GetTransaction(ctx context.Context, id string) (*Transaction, error)
	GetTransactionByIdempotencyKey(ctx context.Context, key string) (*Transaction, error)
	ListTransactions(ctx context.Context, filter TxFilter) ([]*Transaction, error)

	// Pipeline operations
	ClaimNextQueued(ctx context.Context, sender string, chainID uint64, claimedBy string) (*Transaction, error)
	MarkPending(ctx context.Context, id string, nonce uint64) error
	MarkSubmitted(ctx context.Context, id string, txHash string, submittedAt *big.Int) error
	MarkConfirmed(ctx context.Context, id string, receipt *TxReceipt) error
	MarkIncluded(ctx context.Context, id string, receipt *TxReceipt) error
	RevertToSubmitted(ctx context.Context, id string) error
	MarkReverted(ctx context.Context, id string, receipt *TxReceipt) error
	MarkFailed(ctx context.Context, id string, errCode ErrorCode, errReason string) error
	ResetPendingToQueued(ctx context.Context) (int, error)

	// Attempts
	CreateAttempt(ctx context.Context, attempt *TxAttempt) error
	GetAttemptsByTransactionID(ctx context.Context, txID string) ([]*TxAttempt, error)
	MarkAttemptStatus(ctx context.Context, attemptID string, status AttemptStatus) error
	GetBroadcastAttemptsByChain(ctx context.Context, chainID uint64) ([]*TxAttempt, error)

	// Nonce
	GetNonceCursor(ctx context.Context, sender string, chainID uint64) (*NonceCursor, error)
	InitNonceCursor(ctx context.Context, sender string, chainID uint64, nonce uint64) error
	IncrementNonceCursor(ctx context.Context, sender string, chainID uint64) (uint64, error)
	SetNonceCursor(ctx context.Context, sender string, chainID uint64, nonce uint64) error

	// State log
	LogStateTransition(ctx context.Context, log *TxStateLog) error

	// Submitted tx queries for background workers
	GetSubmittedTransactions(ctx context.Context, chainID uint64, limit int, afterID string) ([]*Transaction, error)
	GetIncludedTransactions(ctx context.Context, chainID uint64, limit int, afterID string) ([]*Transaction, error)
	GetStuckTransactions(ctx context.Context, chainID uint64, submittedBefore time.Time) ([]*Transaction, error)
	CountQueuedTransactions(ctx context.Context, sender string, chainID uint64) (int, error)
}

type TxFilter struct {
	ChainID *uint64
	Sender  string
	Status  string
	Limit   int
	After   string
}

type TxReceipt struct {
	TxHash            string
	BlockNumber       uint64
	BlockHash         string
	GasUsed           uint64
	EffectiveGasPrice *big.Int
	Status            uint8 // 1 = success, 0 = revert
	ReceiptJSON       []byte
}

type EthClient interface {
	ChainID() uint64
	BlockNumber(ctx context.Context) (uint64, error)
	PendingNonceAt(ctx context.Context, addr common.Address) (uint64, error)
	NonceAt(ctx context.Context, addr common.Address) (uint64, error)
	SuggestGasPrice(ctx context.Context) (*big.Int, error)
	SuggestGasTipCap(ctx context.Context) (*big.Int, error)
	EstimateGas(ctx context.Context, to common.Address, data []byte, value *big.Int) (uint64, error)
	LatestBaseFee(ctx context.Context) (*big.Int, error)
	SendTransaction(ctx context.Context, tx *types.Transaction) error
	TransactionReceipt(ctx context.Context, txHash common.Hash) (*types.Receipt, error)
	BatchTransactionReceipts(ctx context.Context, txHashes []common.Hash) (map[common.Hash]*types.Receipt, error)
	CodeAt(ctx context.Context, addr common.Address) ([]byte, error)
	Healthy() bool
}

type Signer interface {
	Sign(ctx context.Context, sender common.Address, tx *types.Transaction, chainID *big.Int) (*types.Transaction, error)
	HasAddress(addr common.Address) bool
	Addresses() []common.Address
}
