package domain

import "math/big"

type GasParams struct {
	GasLimit             uint64
	MaxFeePerGas         *big.Int
	MaxPriorityFeePerGas *big.Int
	GasPrice             *big.Int
	TxType               int // 0 = legacy, 2 = EIP-1559
}
