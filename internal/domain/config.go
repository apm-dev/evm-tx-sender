package domain

import (
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

type ChainConfig struct {
	ChainID            uint64
	Name               string
	RPCEndpoints       []string
	BlockTime          time.Duration
	ConfirmationBlocks int
	SupportsEIP1559    bool

	GasLimitMultiplier float64
	MinMaxFee          *big.Int
	MaxMaxFee          *big.Int
	MinPriorityFee     *big.Int
	MaxPriorityFee     *big.Int

	ReceiptChunkSize int

	StuckThreshold  time.Duration
	GasBumpInterval time.Duration

	NativeTokenSymbol   string
	NativeTokenDecimals uint8
	NativeMaxTransfer   *big.Int

	TokenWhitelist map[string]TokenConfig
}

type TokenConfig struct {
	Symbol          string
	ContractAddress common.Address
	Decimals        uint8
	MaxTransfer     *big.Int
	Enabled         bool
}
