package ethereum

import (
	"context"
	"fmt"
	"math/big"

	"github.com/apm-dev/evm-tx-sender/internal/domain"
	"github.com/ethereum/go-ethereum/common"
)

// GasEngine estimates gas parameters for transactions.
type GasEngine struct{}

func NewGasEngine() *GasEngine {
	return &GasEngine{}
}

// Priority fee percentile targets per priority tier.
var priorityMultipliers = map[domain.Priority]struct {
	tipMultiplier int64 // multiplier applied to suggested tip (100 = 1x)
	feeMultiplier int64 // applied to baseFee (100 = 1x)
}{
	domain.PriorityLow:    {tipMultiplier: 80, feeMultiplier: 150},
	domain.PriorityNormal: {tipMultiplier: 100, feeMultiplier: 200},
	domain.PriorityHigh:   {tipMultiplier: 150, feeMultiplier: 250},
	domain.PriorityUrgent: {tipMultiplier: 250, feeMultiplier: 300},
}

func (g *GasEngine) Estimate(
	ctx context.Context,
	client domain.EthClient,
	chain *domain.ChainConfig,
	to common.Address,
	data []byte,
	value *big.Int,
	priority domain.Priority,
) (domain.GasParams, error) {
	// 1. Estimate gas limit
	gasLimit, err := client.EstimateGas(ctx, to, data, value)
	if err != nil {
		return domain.GasParams{}, fmt.Errorf("gas estimation failed: %w", err)
	}
	gasLimit = uint64(float64(gasLimit) * chain.GasLimitMultiplier)

	// 2. Try EIP-1559
	if chain.SupportsEIP1559 {
		params, err := g.estimateEIP1559(ctx, client, chain, priority, gasLimit)
		if err == nil {
			return params, nil
		}
		// Fall back to legacy
	}

	return g.estimateLegacy(ctx, client, chain, gasLimit)
}

func (g *GasEngine) estimateEIP1559(
	ctx context.Context,
	client domain.EthClient,
	chain *domain.ChainConfig,
	priority domain.Priority,
	gasLimit uint64,
) (domain.GasParams, error) {
	baseFee, err := client.LatestBaseFee(ctx)
	if err != nil {
		return domain.GasParams{}, err
	}

	suggestedTip, err := client.SuggestGasTipCap(ctx)
	if err != nil {
		// Default to 1 gwei
		suggestedTip = big.NewInt(1_000_000_000)
	}

	mult, ok := priorityMultipliers[priority]
	if !ok {
		mult = priorityMultipliers[domain.PriorityNormal]
	}

	// Priority fee = suggestedTip * tipMultiplier / 100
	priorityFee := new(big.Int).Mul(suggestedTip, big.NewInt(mult.tipMultiplier))
	priorityFee.Div(priorityFee, big.NewInt(100))

	// MaxFee = baseFee * feeMultiplier / 100 + priorityFee
	maxFee := new(big.Int).Mul(baseFee, big.NewInt(mult.feeMultiplier))
	maxFee.Div(maxFee, big.NewInt(100))
	maxFee.Add(maxFee, priorityFee)

	// Clamp
	priorityFee = clamp(priorityFee, chain.MinPriorityFee, chain.MaxPriorityFee)
	maxFee = clamp(maxFee, chain.MinMaxFee, chain.MaxMaxFee)

	// Ensure maxFee >= priorityFee
	if maxFee.Cmp(priorityFee) < 0 {
		maxFee = new(big.Int).Set(priorityFee)
	}

	return domain.GasParams{
		GasLimit:             gasLimit,
		MaxFeePerGas:         maxFee,
		MaxPriorityFeePerGas: priorityFee,
		TxType:               2,
	}, nil
}

func (g *GasEngine) estimateLegacy(
	ctx context.Context,
	client domain.EthClient,
	chain *domain.ChainConfig,
	gasLimit uint64,
) (domain.GasParams, error) {
	gasPrice, err := client.SuggestGasPrice(ctx)
	if err != nil {
		return domain.GasParams{}, fmt.Errorf("gas price suggestion failed: %w", err)
	}

	gasPrice = clamp(gasPrice, chain.MinMaxFee, chain.MaxMaxFee)

	return domain.GasParams{
		GasLimit: gasLimit,
		GasPrice: gasPrice,
		TxType:   0,
	}, nil
}

// BumpGas returns new gas params with at least 15% increase.
func (g *GasEngine) BumpGas(current domain.GasParams, bumpNumber int) domain.GasParams {
	// Bump percentage: 15% per bump, capped at 100% (double)
	bumpPct := int64(15 * bumpNumber)
	if bumpPct > 100 {
		bumpPct = 100
	}

	bumped := current
	if current.TxType == 2 {
		bumped.MaxFeePerGas = bumpBigInt(current.MaxFeePerGas, bumpPct)
		bumped.MaxPriorityFeePerGas = bumpBigInt(current.MaxPriorityFeePerGas, bumpPct)
	} else {
		bumped.GasPrice = bumpBigInt(current.GasPrice, bumpPct)
	}

	return bumped
}

func bumpBigInt(v *big.Int, pct int64) *big.Int {
	if v == nil {
		return nil
	}
	bump := new(big.Int).Mul(v, big.NewInt(pct))
	bump.Div(bump, big.NewInt(100))
	result := new(big.Int).Add(v, bump)
	return result
}

func clamp(v, min, max *big.Int) *big.Int {
	if v == nil {
		return v
	}
	if min != nil && v.Cmp(min) < 0 {
		return new(big.Int).Set(min)
	}
	if max != nil && v.Cmp(max) > 0 {
		return new(big.Int).Set(max)
	}
	return v
}
