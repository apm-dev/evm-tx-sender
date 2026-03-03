package domain

import (
	"math/big"
	"time"
)

type TxStatus string

const (
	TxStatusQueued    TxStatus = "QUEUED"
	TxStatusPending   TxStatus = "PENDING"
	TxStatusSubmitted TxStatus = "SUBMITTED"
	TxStatusIncluded  TxStatus = "INCLUDED"
	TxStatusConfirmed TxStatus = "CONFIRMED"
	TxStatusReverted  TxStatus = "REVERTED"
	TxStatusFailed    TxStatus = "FAILED"
)

func (s TxStatus) IsTerminal() bool {
	return s == TxStatusConfirmed || s == TxStatusReverted || s == TxStatusFailed
}

type AttemptStatus string

const (
	AttemptBroadcast AttemptStatus = "BROADCAST"
	AttemptConfirmed AttemptStatus = "CONFIRMED"
	AttemptReplaced  AttemptStatus = "REPLACED"
	AttemptDropped   AttemptStatus = "DROPPED"
)

type Priority string

const (
	PriorityLow    Priority = "low"
	PriorityNormal Priority = "normal"
	PriorityHigh   Priority = "high"
	PriorityUrgent Priority = "urgent"
)

func ParsePriority(s string) (Priority, bool) {
	switch s {
	case "low":
		return PriorityLow, true
	case "normal":
		return PriorityNormal, true
	case "high":
		return PriorityHigh, true
	case "urgent":
		return PriorityUrgent, true
	default:
		return "", false
	}
}

type Transaction struct {
	ID             string
	IdempotencyKey string
	ChainID        uint64
	Sender         string
	ToAddress      string
	Value          *big.Int
	Data           []byte
	GasLimit       *uint64
	Nonce          *uint64
	Priority       Priority

	Status      TxStatus
	ErrorReason string
	ErrorCode   string

	FinalTxHash       string
	BlockNumber       *uint64
	BlockHash         string
	GasUsed           *uint64
	EffectiveGasPrice *big.Int
	ReceiptStatus     *uint8
	ReceiptData       []byte // JSON
	Confirmations     int

	// Transfer metadata
	TransferToken     string
	TransferAmount    string
	TransferRecipient string
	TokenContract     string // empty for native
	TokenDecimals     uint8

	Metadata []byte // opaque JSON

	CreatedAt   time.Time
	UpdatedAt   time.Time
	SubmittedAt *time.Time
	ConfirmedAt *time.Time
	ClaimedBy   string
	ClaimedAt   *time.Time
}

type TxAttempt struct {
	ID            string
	TransactionID string
	AttemptNumber int
	TxHash        string

	GasLimit             uint64
	MaxFeePerGas         *big.Int
	MaxPriorityFeePerGas *big.Int
	GasPrice             *big.Int
	TxType               int // 0 = legacy, 2 = EIP-1559

	RawTx []byte

	Status      AttemptStatus
	ErrorReason string

	CreatedAt   time.Time
	ConfirmedAt *time.Time
}

type TxStateLog struct {
	ID            int64
	TransactionID string
	FromStatus    string
	ToStatus      string
	Actor         string
	Reason        string
	Metadata      []byte // JSON
	CreatedAt     time.Time
}

type NonceCursor struct {
	Sender    string
	ChainID   uint64
	NextNonce uint64
	UpdatedAt time.Time
}
