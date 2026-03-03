package ethereum

import (
	"context"
	"fmt"
	"math/big"
	"testing"

	"github.com/apm-dev/evm-tx-sender/internal/domain"
	"github.com/apm-dev/evm-tx-sender/internal/mocks"
	"github.com/ethereum/go-ethereum/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
)

func gwei(n int64) *big.Int {
	return new(big.Int).Mul(big.NewInt(n), big.NewInt(1_000_000_000))
}

func defaultChain(eip1559 bool) *domain.ChainConfig {
	return &domain.ChainConfig{
		ChainID:            1,
		SupportsEIP1559:    eip1559,
		GasLimitMultiplier: 1.2,
		MinMaxFee:          gwei(1),
		MaxMaxFee:          gwei(500),
		MinPriorityFee:     gwei(1),
		MaxPriorityFee:     gwei(50),
	}
}

func TestGasEngine_Estimate_EIP1559_NormalPriority(t *testing.T) {
	ctrl := gomock.NewController(t)
	client := mocks.NewMockEthClient(ctrl)
	engine := NewGasEngine()
	chain := defaultChain(true)
	to := common.HexToAddress("0x1234567890abcdef1234567890abcdef12345678")

	client.EXPECT().EstimateGas(gomock.Any(), to, []byte(nil), big.NewInt(0)).Return(uint64(21000), nil)
	client.EXPECT().LatestBaseFee(gomock.Any()).Return(gwei(30), nil)
	client.EXPECT().SuggestGasTipCap(gomock.Any()).Return(gwei(2), nil)

	params, err := engine.Estimate(context.Background(), client, chain, to, nil, big.NewInt(0), domain.PriorityNormal)
	require.NoError(t, err)

	assert.Equal(t, 2, params.TxType)
	assert.Equal(t, uint64(25200), params.GasLimit) // 21000 * 1.2

	// priorityFee = 2 gwei * 100 / 100 = 2 gwei (clamped min 1, max 50 -> 2)
	assert.Equal(t, gwei(2).String(), params.MaxPriorityFeePerGas.String())

	// maxFee = 30 gwei * 200 / 100 + 2 gwei = 62 gwei
	assert.Equal(t, gwei(62).String(), params.MaxFeePerGas.String())
}

func TestGasEngine_Estimate_EIP1559_UrgentPriority(t *testing.T) {
	ctrl := gomock.NewController(t)
	client := mocks.NewMockEthClient(ctrl)
	engine := NewGasEngine()
	chain := defaultChain(true)
	to := common.HexToAddress("0x1234567890abcdef1234567890abcdef12345678")

	client.EXPECT().EstimateGas(gomock.Any(), to, []byte(nil), big.NewInt(0)).Return(uint64(21000), nil)
	client.EXPECT().LatestBaseFee(gomock.Any()).Return(gwei(30), nil)
	client.EXPECT().SuggestGasTipCap(gomock.Any()).Return(gwei(2), nil)

	params, err := engine.Estimate(context.Background(), client, chain, to, nil, big.NewInt(0), domain.PriorityUrgent)
	require.NoError(t, err)

	// priorityFee = 2 gwei * 250 / 100 = 5 gwei
	assert.Equal(t, gwei(5).String(), params.MaxPriorityFeePerGas.String())

	// maxFee = 30 gwei * 300 / 100 + 5 gwei = 95 gwei
	assert.Equal(t, gwei(95).String(), params.MaxFeePerGas.String())
}

func TestGasEngine_Estimate_EIP1559_LowPriority(t *testing.T) {
	ctrl := gomock.NewController(t)
	client := mocks.NewMockEthClient(ctrl)
	engine := NewGasEngine()
	chain := defaultChain(true)
	to := common.HexToAddress("0x1234567890abcdef1234567890abcdef12345678")

	client.EXPECT().EstimateGas(gomock.Any(), to, []byte(nil), big.NewInt(0)).Return(uint64(21000), nil)
	client.EXPECT().LatestBaseFee(gomock.Any()).Return(gwei(30), nil)
	client.EXPECT().SuggestGasTipCap(gomock.Any()).Return(gwei(2), nil)

	params, err := engine.Estimate(context.Background(), client, chain, to, nil, big.NewInt(0), domain.PriorityLow)
	require.NoError(t, err)

	// priorityFee = 2 * 80 / 100 = 1.6 gwei -> integer division = 1600000000
	expectedTip := new(big.Int).Mul(gwei(2), big.NewInt(80))
	expectedTip.Div(expectedTip, big.NewInt(100))
	assert.Equal(t, expectedTip.String(), params.MaxPriorityFeePerGas.String())

	// maxFee = 30 * 150/100 + tip = 45 + 1.6 = 46.6 gwei
	expectedFee := new(big.Int).Mul(gwei(30), big.NewInt(150))
	expectedFee.Div(expectedFee, big.NewInt(100))
	expectedFee.Add(expectedFee, expectedTip)
	assert.Equal(t, expectedFee.String(), params.MaxFeePerGas.String())
}

func TestGasEngine_Estimate_EIP1559_ClampsMinPriorityFee(t *testing.T) {
	ctrl := gomock.NewController(t)
	client := mocks.NewMockEthClient(ctrl)
	engine := NewGasEngine()
	chain := defaultChain(true)
	chain.MinPriorityFee = gwei(5) // min is 5 gwei
	to := common.HexToAddress("0x1234567890abcdef1234567890abcdef12345678")

	client.EXPECT().EstimateGas(gomock.Any(), to, []byte(nil), big.NewInt(0)).Return(uint64(21000), nil)
	client.EXPECT().LatestBaseFee(gomock.Any()).Return(gwei(30), nil)
	client.EXPECT().SuggestGasTipCap(gomock.Any()).Return(gwei(2), nil) // would result in 2 gwei < min

	params, err := engine.Estimate(context.Background(), client, chain, to, nil, big.NewInt(0), domain.PriorityNormal)
	require.NoError(t, err)

	// Should be clamped to min 5 gwei
	assert.Equal(t, gwei(5).String(), params.MaxPriorityFeePerGas.String())
}

func TestGasEngine_Estimate_EIP1559_ClampsMaxFee(t *testing.T) {
	ctrl := gomock.NewController(t)
	client := mocks.NewMockEthClient(ctrl)
	engine := NewGasEngine()
	chain := defaultChain(true)
	chain.MaxMaxFee = gwei(50)
	to := common.HexToAddress("0x1234567890abcdef1234567890abcdef12345678")

	client.EXPECT().EstimateGas(gomock.Any(), to, []byte(nil), big.NewInt(0)).Return(uint64(21000), nil)
	client.EXPECT().LatestBaseFee(gomock.Any()).Return(gwei(100), nil) // high base fee -> maxFee would be 200+ gwei
	client.EXPECT().SuggestGasTipCap(gomock.Any()).Return(gwei(2), nil)

	params, err := engine.Estimate(context.Background(), client, chain, to, nil, big.NewInt(0), domain.PriorityNormal)
	require.NoError(t, err)

	assert.Equal(t, gwei(50).String(), params.MaxFeePerGas.String())
}

func TestGasEngine_Estimate_LegacyFallback(t *testing.T) {
	ctrl := gomock.NewController(t)
	client := mocks.NewMockEthClient(ctrl)
	engine := NewGasEngine()
	chain := defaultChain(false) // no EIP-1559
	to := common.HexToAddress("0x1234567890abcdef1234567890abcdef12345678")

	client.EXPECT().EstimateGas(gomock.Any(), to, []byte(nil), big.NewInt(0)).Return(uint64(21000), nil)
	client.EXPECT().SuggestGasPrice(gomock.Any()).Return(gwei(20), nil)

	params, err := engine.Estimate(context.Background(), client, chain, to, nil, big.NewInt(0), domain.PriorityNormal)
	require.NoError(t, err)

	assert.Equal(t, 0, params.TxType)
	assert.Equal(t, gwei(20).String(), params.GasPrice.String())
	assert.Nil(t, params.MaxFeePerGas)
	assert.Nil(t, params.MaxPriorityFeePerGas)
}

func TestGasEngine_Estimate_EIP1559FallsBackToLegacy(t *testing.T) {
	ctrl := gomock.NewController(t)
	client := mocks.NewMockEthClient(ctrl)
	engine := NewGasEngine()
	chain := defaultChain(true)
	to := common.HexToAddress("0x1234567890abcdef1234567890abcdef12345678")

	client.EXPECT().EstimateGas(gomock.Any(), to, []byte(nil), big.NewInt(0)).Return(uint64(21000), nil)
	client.EXPECT().LatestBaseFee(gomock.Any()).Return(nil, fmt.Errorf("not supported"))
	client.EXPECT().SuggestGasPrice(gomock.Any()).Return(gwei(20), nil)

	params, err := engine.Estimate(context.Background(), client, chain, to, nil, big.NewInt(0), domain.PriorityNormal)
	require.NoError(t, err)

	assert.Equal(t, 0, params.TxType) // legacy fallback
}

func TestGasEngine_Estimate_GasEstimationFailure(t *testing.T) {
	ctrl := gomock.NewController(t)
	client := mocks.NewMockEthClient(ctrl)
	engine := NewGasEngine()
	chain := defaultChain(true)
	to := common.HexToAddress("0x1234567890abcdef1234567890abcdef12345678")

	client.EXPECT().EstimateGas(gomock.Any(), to, []byte(nil), big.NewInt(0)).Return(uint64(0), fmt.Errorf("execution reverted"))

	_, err := engine.Estimate(context.Background(), client, chain, to, nil, big.NewInt(0), domain.PriorityNormal)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "gas estimation failed")
}

func TestGasEngine_Estimate_TipCapFallsBackTo1Gwei(t *testing.T) {
	ctrl := gomock.NewController(t)
	client := mocks.NewMockEthClient(ctrl)
	engine := NewGasEngine()
	chain := defaultChain(true)
	chain.MinPriorityFee = nil
	chain.MaxPriorityFee = nil
	to := common.HexToAddress("0x1234567890abcdef1234567890abcdef12345678")

	client.EXPECT().EstimateGas(gomock.Any(), to, []byte(nil), big.NewInt(0)).Return(uint64(21000), nil)
	client.EXPECT().LatestBaseFee(gomock.Any()).Return(gwei(30), nil)
	client.EXPECT().SuggestGasTipCap(gomock.Any()).Return(nil, fmt.Errorf("not supported"))

	params, err := engine.Estimate(context.Background(), client, chain, to, nil, big.NewInt(0), domain.PriorityNormal)
	require.NoError(t, err)

	// Default tip is 1 gwei * 100/100 = 1 gwei
	assert.Equal(t, gwei(1).String(), params.MaxPriorityFeePerGas.String())
}

func TestGasEngine_BumpGas_EIP1559_FirstBump(t *testing.T) {
	engine := NewGasEngine()
	current := domain.GasParams{
		GasLimit:             100000,
		MaxFeePerGas:         gwei(100),
		MaxPriorityFeePerGas: gwei(2),
		TxType:               2,
	}

	bumped := engine.BumpGas(current, 1)

	// 15% bump for bumpNumber=1
	expectedMaxFee := new(big.Int).Add(gwei(100), new(big.Int).Div(new(big.Int).Mul(gwei(100), big.NewInt(15)), big.NewInt(100)))
	expectedTip := new(big.Int).Add(gwei(2), new(big.Int).Div(new(big.Int).Mul(gwei(2), big.NewInt(15)), big.NewInt(100)))

	assert.Equal(t, expectedMaxFee.String(), bumped.MaxFeePerGas.String())
	assert.Equal(t, expectedTip.String(), bumped.MaxPriorityFeePerGas.String())
	assert.Equal(t, current.GasLimit, bumped.GasLimit)
}

func TestGasEngine_BumpGas_EIP1559_SecondBump(t *testing.T) {
	engine := NewGasEngine()
	current := domain.GasParams{
		GasLimit:             100000,
		MaxFeePerGas:         gwei(100),
		MaxPriorityFeePerGas: gwei(2),
		TxType:               2,
	}

	bumped := engine.BumpGas(current, 2)

	// 30% bump for bumpNumber=2
	expectedMaxFee := new(big.Int).Add(gwei(100), new(big.Int).Div(new(big.Int).Mul(gwei(100), big.NewInt(30)), big.NewInt(100)))
	assert.Equal(t, expectedMaxFee.String(), bumped.MaxFeePerGas.String())
}

func TestGasEngine_BumpGas_CapAt100Percent(t *testing.T) {
	engine := NewGasEngine()
	current := domain.GasParams{
		GasLimit:             100000,
		MaxFeePerGas:         gwei(100),
		MaxPriorityFeePerGas: gwei(2),
		TxType:               2,
	}

	// bumpNumber=7 -> 15*7=105, capped at 100
	bumped := engine.BumpGas(current, 7)

	// 100% bump = double
	expectedMaxFee := new(big.Int).Mul(gwei(100), big.NewInt(2))
	assert.Equal(t, expectedMaxFee.String(), bumped.MaxFeePerGas.String())
}

func TestGasEngine_BumpGas_Legacy(t *testing.T) {
	engine := NewGasEngine()
	current := domain.GasParams{
		GasLimit: 100000,
		GasPrice: gwei(20),
		TxType:   0,
	}

	bumped := engine.BumpGas(current, 1)

	// 15% bump
	expectedPrice := new(big.Int).Add(gwei(20), new(big.Int).Div(new(big.Int).Mul(gwei(20), big.NewInt(15)), big.NewInt(100)))
	assert.Equal(t, expectedPrice.String(), bumped.GasPrice.String())
	assert.Nil(t, bumped.MaxFeePerGas)
	assert.Nil(t, bumped.MaxPriorityFeePerGas)
}

func TestGasEngine_BumpGas_SequentialBumps(t *testing.T) {
	engine := NewGasEngine()

	// Simulate 5 sequential bumps (each applied to the ORIGINAL, not compounding)
	// But note: BumpGas is called with bumpNumber, not applied sequentially to result
	original := domain.GasParams{
		GasLimit: 100000,
		GasPrice: gwei(20),
		TxType:   0,
	}

	for i := 1; i <= 5; i++ {
		bumped := engine.BumpGas(original, i)
		bumpPct := int64(15 * i)
		if bumpPct > 100 {
			bumpPct = 100
		}
		expectedPrice := new(big.Int).Add(gwei(20), new(big.Int).Div(new(big.Int).Mul(gwei(20), big.NewInt(bumpPct)), big.NewInt(100)))
		assert.Equal(t, expectedPrice.String(), bumped.GasPrice.String(), "bump %d", i)
	}
}

func TestGasEngine_BumpGas_NilValues(t *testing.T) {
	engine := NewGasEngine()
	current := domain.GasParams{
		GasLimit: 100000,
		TxType:   2,
		// MaxFeePerGas and MaxPriorityFeePerGas are nil
	}

	bumped := engine.BumpGas(current, 1)

	assert.Nil(t, bumped.MaxFeePerGas)
	assert.Nil(t, bumped.MaxPriorityFeePerGas)
}

func TestGasEngine_Estimate_LegacyClampsGasPrice(t *testing.T) {
	ctrl := gomock.NewController(t)
	client := mocks.NewMockEthClient(ctrl)
	engine := NewGasEngine()
	chain := defaultChain(false)
	chain.MaxMaxFee = gwei(10)
	to := common.HexToAddress("0x1234567890abcdef1234567890abcdef12345678")

	client.EXPECT().EstimateGas(gomock.Any(), to, []byte(nil), big.NewInt(0)).Return(uint64(21000), nil)
	client.EXPECT().SuggestGasPrice(gomock.Any()).Return(gwei(50), nil) // exceeds max

	params, err := engine.Estimate(context.Background(), client, chain, to, nil, big.NewInt(0), domain.PriorityNormal)
	require.NoError(t, err)

	assert.Equal(t, gwei(10).String(), params.GasPrice.String()) // clamped to max
}
