package api

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/apm-dev/evm-tx-sender/internal/domain"
	"github.com/apm-dev/evm-tx-sender/internal/infrastructure/ethereum"
	"github.com/apm-dev/evm-tx-sender/internal/mocks"
	"github.com/apm-dev/evm-tx-sender/internal/pipeline"
	"github.com/ethereum/go-ethereum/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
)

var (
	testSender    = common.HexToAddress("0xAb5801a7D398351b8bE11C439e05C5B3259aeC9B")
	testRecipient = "0x1234567890abcdef1234567890abcdef12345678"
	testUSDCAddr  = common.HexToAddress("0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48")
)

func setupTransferHandler(t *testing.T) (*mocks.MockRepository, *mocks.MockSigner, *TransferHandler) {
	t.Helper()
	ctrl := gomock.NewController(t)

	repo := mocks.NewMockRepository(ctrl)
	signer := mocks.NewMockSigner(ctrl)

	chains := map[uint64]domain.ChainConfig{
		1: {
			ChainID:             1,
			NativeTokenSymbol:   "ETH",
			NativeTokenDecimals: 18,
			NativeMaxTransfer:   new(big.Int).Mul(big.NewInt(100), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)), // 100 ETH
			SupportsEIP1559:     true,
			TokenWhitelist: map[string]domain.TokenConfig{
				"USDC": {
					Symbol:          "USDC",
					ContractAddress: testUSDCAddr,
					Decimals:        6,
					MaxTransfer:     big.NewInt(1_000_000_000_000), // 1M USDC
					Enabled:         true,
				},
				"DAI": {
					Symbol:          "DAI",
					ContractAddress: common.HexToAddress("0x6B175474E89094C44Da98b954EedeAC495271d0F"),
					Decimals:        18,
					Enabled:         false,
				},
			},
		},
	}

	log := slog.Default()
	manager := pipeline.NewManager(repo, nil, signer, ethereum.NewGasEngine(), chains, log)

	handler := NewTransferHandler(repo, signer, chains, manager)
	return repo, signer, handler
}

func doTransferRequest(handler *TransferHandler, body TransferRequest) *httptest.ResponseRecorder {
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/v1/transfers", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	handler.Create(rr, req)
	return rr
}

func TestTransfer_HappyPath_Native(t *testing.T) {
	repo, signer, handler := setupTransferHandler(t)

	signer.EXPECT().HasAddress(testSender).Return(true)
	repo.EXPECT().GetTransactionByIdempotencyKey(gomock.Any(), "test-key-1").Return(nil, nil)
	repo.EXPECT().CreateTransaction(gomock.Any(), gomock.Any()).Return(nil)
	repo.EXPECT().LogStateTransition(gomock.Any(), gomock.Any()).Return(nil)

	rr := doTransferRequest(handler, TransferRequest{
		IdempotencyKey: "test-key-1",
		ChainID:        1,
		Sender:         testSender.Hex(),
		Recipient:      testRecipient,
		Token:          "ETH",
		Amount:         "1.5",
	})

	assert.Equal(t, http.StatusAccepted, rr.Code)

	var resp TransferResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	assert.Equal(t, "QUEUED", resp.Status)
	assert.Equal(t, "native", resp.TransferType)
	assert.Equal(t, "1.5", resp.Amount)
	assert.Equal(t, "ETH", resp.Token)
	assert.NotEmpty(t, resp.ID)
}

func TestTransfer_HappyPath_ERC20(t *testing.T) {
	repo, signer, handler := setupTransferHandler(t)

	signer.EXPECT().HasAddress(testSender).Return(true)
	repo.EXPECT().GetTransactionByIdempotencyKey(gomock.Any(), "test-key-2").Return(nil, nil)
	repo.EXPECT().CreateTransaction(gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ interface{}, tx *domain.Transaction) error {
			assert.NotEmpty(t, tx.Data, "ERC20 transfer should have calldata")
			assert.Equal(t, strings.ToLower(testUSDCAddr.Hex()), tx.ToAddress)
			assert.Equal(t, big.NewInt(0).String(), tx.Value.String())
			return nil
		},
	)
	repo.EXPECT().LogStateTransition(gomock.Any(), gomock.Any()).Return(nil)

	rr := doTransferRequest(handler, TransferRequest{
		IdempotencyKey: "test-key-2",
		ChainID:        1,
		Sender:         testSender.Hex(),
		Recipient:      testRecipient,
		Token:          "USDC",
		Amount:         "100.50",
	})

	assert.Equal(t, http.StatusAccepted, rr.Code)

	var resp TransferResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	assert.Equal(t, "QUEUED", resp.Status)
	assert.Equal(t, "erc20", resp.TransferType)
	assert.NotEmpty(t, resp.TokenContract)
}

func TestTransfer_MissingIdempotencyKey(t *testing.T) {
	_, signer, handler := setupTransferHandler(t)
	_ = signer

	rr := doTransferRequest(handler, TransferRequest{
		ChainID:   1,
		Sender:    testSender.Hex(),
		Recipient: testRecipient,
		Token:     "ETH",
		Amount:    "1.0",
	})

	assert.Equal(t, http.StatusBadRequest, rr.Code)
	var resp ErrorResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	assert.Equal(t, string(domain.ErrCodeMissingField), resp.Code)
}

func TestTransfer_MissingChainID(t *testing.T) {
	_, _, handler := setupTransferHandler(t)

	rr := doTransferRequest(handler, TransferRequest{
		IdempotencyKey: "key",
		Sender:         testSender.Hex(),
		Recipient:      testRecipient,
		Token:          "ETH",
		Amount:         "1.0",
	})

	assert.Equal(t, http.StatusBadRequest, rr.Code)
}

func TestTransfer_UnsupportedChainID(t *testing.T) {
	_, _, handler := setupTransferHandler(t)

	rr := doTransferRequest(handler, TransferRequest{
		IdempotencyKey: "key",
		ChainID:        999,
		Sender:         testSender.Hex(),
		Recipient:      testRecipient,
		Token:          "ETH",
		Amount:         "1.0",
	})

	assert.Equal(t, http.StatusBadRequest, rr.Code)
	var resp ErrorResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	assert.Equal(t, string(domain.ErrCodeInvalidChain), resp.Code)
}

func TestTransfer_MissingSender(t *testing.T) {
	_, _, handler := setupTransferHandler(t)

	rr := doTransferRequest(handler, TransferRequest{
		IdempotencyKey: "key",
		ChainID:        1,
		Recipient:      testRecipient,
		Token:          "ETH",
		Amount:         "1.0",
	})

	assert.Equal(t, http.StatusBadRequest, rr.Code)
}

func TestTransfer_InvalidSenderAddress(t *testing.T) {
	_, _, handler := setupTransferHandler(t)

	rr := doTransferRequest(handler, TransferRequest{
		IdempotencyKey: "key",
		ChainID:        1,
		Sender:         "not-an-address",
		Recipient:      testRecipient,
		Token:          "ETH",
		Amount:         "1.0",
	})

	assert.Equal(t, http.StatusBadRequest, rr.Code)
	var resp ErrorResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	assert.Equal(t, string(domain.ErrCodeInvalidAddress), resp.Code)
}

func TestTransfer_UnknownSender(t *testing.T) {
	_, signer, handler := setupTransferHandler(t)

	signer.EXPECT().HasAddress(testSender).Return(false)

	rr := doTransferRequest(handler, TransferRequest{
		IdempotencyKey: "key",
		ChainID:        1,
		Sender:         testSender.Hex(),
		Recipient:      testRecipient,
		Token:          "ETH",
		Amount:         "1.0",
	})

	assert.Equal(t, http.StatusNotFound, rr.Code)
	var resp ErrorResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	assert.Equal(t, string(domain.ErrCodeUnknownSender), resp.Code)
}

func TestTransfer_MissingRecipient(t *testing.T) {
	_, signer, handler := setupTransferHandler(t)

	signer.EXPECT().HasAddress(testSender).Return(true)

	rr := doTransferRequest(handler, TransferRequest{
		IdempotencyKey: "key",
		ChainID:        1,
		Sender:         testSender.Hex(),
		Token:          "ETH",
		Amount:         "1.0",
	})

	assert.Equal(t, http.StatusBadRequest, rr.Code)
}

func TestTransfer_InvalidRecipient(t *testing.T) {
	_, signer, handler := setupTransferHandler(t)

	signer.EXPECT().HasAddress(testSender).Return(true)

	rr := doTransferRequest(handler, TransferRequest{
		IdempotencyKey: "key",
		ChainID:        1,
		Sender:         testSender.Hex(),
		Recipient:      "not-valid",
		Token:          "ETH",
		Amount:         "1.0",
	})

	assert.Equal(t, http.StatusBadRequest, rr.Code)
	var resp ErrorResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	assert.Equal(t, string(domain.ErrCodeInvalidRecipient), resp.Code)
}

func TestTransfer_ZeroAddressRecipient(t *testing.T) {
	_, signer, handler := setupTransferHandler(t)

	signer.EXPECT().HasAddress(testSender).Return(true)

	rr := doTransferRequest(handler, TransferRequest{
		IdempotencyKey: "key",
		ChainID:        1,
		Sender:         testSender.Hex(),
		Recipient:      "0x0000000000000000000000000000000000000000",
		Token:          "ETH",
		Amount:         "1.0",
	})

	assert.Equal(t, http.StatusBadRequest, rr.Code)
	var resp ErrorResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	assert.Equal(t, string(domain.ErrCodeZeroAddressRecipient), resp.Code)
}

func TestTransfer_MissingToken(t *testing.T) {
	_, signer, handler := setupTransferHandler(t)

	signer.EXPECT().HasAddress(testSender).Return(true)

	rr := doTransferRequest(handler, TransferRequest{
		IdempotencyKey: "key",
		ChainID:        1,
		Sender:         testSender.Hex(),
		Recipient:      testRecipient,
		Amount:         "1.0",
	})

	assert.Equal(t, http.StatusBadRequest, rr.Code)
}

func TestTransfer_MissingAmount(t *testing.T) {
	_, signer, handler := setupTransferHandler(t)

	signer.EXPECT().HasAddress(testSender).Return(true)

	rr := doTransferRequest(handler, TransferRequest{
		IdempotencyKey: "key",
		ChainID:        1,
		Sender:         testSender.Hex(),
		Recipient:      testRecipient,
		Token:          "ETH",
	})

	assert.Equal(t, http.StatusBadRequest, rr.Code)
}

func TestTransfer_ZeroAmount(t *testing.T) {
	repo, signer, handler := setupTransferHandler(t)

	signer.EXPECT().HasAddress(testSender).Return(true)
	repo.EXPECT().GetTransactionByIdempotencyKey(gomock.Any(), "key").Return(nil, nil)

	rr := doTransferRequest(handler, TransferRequest{
		IdempotencyKey: "key",
		ChainID:        1,
		Sender:         testSender.Hex(),
		Recipient:      testRecipient,
		Token:          "ETH",
		Amount:         "0",
	})

	assert.Equal(t, 422, rr.Code)
	var resp ErrorResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	assert.Equal(t, string(domain.ErrCodeZeroAmount), resp.Code)
}

func TestTransfer_TokenNotWhitelisted(t *testing.T) {
	repo, signer, handler := setupTransferHandler(t)

	signer.EXPECT().HasAddress(testSender).Return(true)
	repo.EXPECT().GetTransactionByIdempotencyKey(gomock.Any(), "key").Return(nil, nil)

	rr := doTransferRequest(handler, TransferRequest{
		IdempotencyKey: "key",
		ChainID:        1,
		Sender:         testSender.Hex(),
		Recipient:      testRecipient,
		Token:          "WBTC",
		Amount:         "1.0",
	})

	assert.Equal(t, http.StatusForbidden, rr.Code)
	var resp ErrorResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	assert.Equal(t, string(domain.ErrCodeTokenNotWhitelisted), resp.Code)
}

func TestTransfer_TokenDisabled(t *testing.T) {
	repo, signer, handler := setupTransferHandler(t)

	signer.EXPECT().HasAddress(testSender).Return(true)
	repo.EXPECT().GetTransactionByIdempotencyKey(gomock.Any(), "key").Return(nil, nil)

	rr := doTransferRequest(handler, TransferRequest{
		IdempotencyKey: "key",
		ChainID:        1,
		Sender:         testSender.Hex(),
		Recipient:      testRecipient,
		Token:          "DAI",
		Amount:         "100",
	})

	assert.Equal(t, http.StatusForbidden, rr.Code)
	var resp ErrorResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	assert.Equal(t, string(domain.ErrCodeTokenDisabled), resp.Code)
}

func TestTransfer_ExceedsNativeTransferLimit(t *testing.T) {
	repo, signer, handler := setupTransferHandler(t)

	signer.EXPECT().HasAddress(testSender).Return(true)
	repo.EXPECT().GetTransactionByIdempotencyKey(gomock.Any(), "key").Return(nil, nil)

	rr := doTransferRequest(handler, TransferRequest{
		IdempotencyKey: "key",
		ChainID:        1,
		Sender:         testSender.Hex(),
		Recipient:      testRecipient,
		Token:          "ETH",
		Amount:         "200", // max is 100 ETH
	})

	assert.Equal(t, 422, rr.Code)
	var resp ErrorResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	assert.Equal(t, string(domain.ErrCodeExceedsTransferLimit), resp.Code)
}

func TestTransfer_ExceedsERC20TransferLimit(t *testing.T) {
	repo, signer, handler := setupTransferHandler(t)

	signer.EXPECT().HasAddress(testSender).Return(true)
	repo.EXPECT().GetTransactionByIdempotencyKey(gomock.Any(), "key").Return(nil, nil)

	rr := doTransferRequest(handler, TransferRequest{
		IdempotencyKey: "key",
		ChainID:        1,
		Sender:         testSender.Hex(),
		Recipient:      testRecipient,
		Token:          "USDC",
		Amount:         "2000000", // max is 1M USDC
	})

	assert.Equal(t, 422, rr.Code)
	var resp ErrorResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	assert.Equal(t, string(domain.ErrCodeExceedsTransferLimit), resp.Code)
}

func TestTransfer_IdempotencyKey_SameParams_ReturnsExisting(t *testing.T) {
	repo, signer, handler := setupTransferHandler(t)

	signer.EXPECT().HasAddress(testSender).Return(true)

	existing := &domain.Transaction{
		ID:                "existing-id",
		IdempotencyKey:    "dup-key",
		ChainID:           1,
		Sender:            strings.ToLower(testSender.Hex()),
		TransferRecipient: strings.ToLower(common.HexToAddress(testRecipient).Hex()),
		TransferToken:     "ETH",
		TransferAmount:    "1.5",
		Status:            domain.TxStatusSubmitted,
		Value:             big.NewInt(0),
	}

	repo.EXPECT().GetTransactionByIdempotencyKey(gomock.Any(), "dup-key").Return(existing, nil)

	rr := doTransferRequest(handler, TransferRequest{
		IdempotencyKey: "dup-key",
		ChainID:        1,
		Sender:         testSender.Hex(),
		Recipient:      testRecipient,
		Token:          "ETH",
		Amount:         "1.5",
	})

	assert.Equal(t, http.StatusAccepted, rr.Code)
	var resp TransferResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	assert.Equal(t, "existing-id", resp.ID)
	assert.Equal(t, "SUBMITTED", resp.Status)
}

func TestTransfer_IdempotencyKey_DifferentParams_Returns409(t *testing.T) {
	repo, signer, handler := setupTransferHandler(t)

	signer.EXPECT().HasAddress(testSender).Return(true)

	existing := &domain.Transaction{
		ID:                "existing-id",
		IdempotencyKey:    "dup-key",
		ChainID:           1,
		Sender:            strings.ToLower(testSender.Hex()),
		TransferRecipient: strings.ToLower(common.HexToAddress(testRecipient).Hex()),
		TransferToken:     "ETH",
		TransferAmount:    "1.5",
		Status:            domain.TxStatusSubmitted,
		Value:             big.NewInt(0),
	}

	repo.EXPECT().GetTransactionByIdempotencyKey(gomock.Any(), "dup-key").Return(existing, nil)

	rr := doTransferRequest(handler, TransferRequest{
		IdempotencyKey: "dup-key",
		ChainID:        1,
		Sender:         testSender.Hex(),
		Recipient:      testRecipient,
		Token:          "ETH",
		Amount:         "2.0", // different amount
	})

	assert.Equal(t, http.StatusConflict, rr.Code)
	var resp ErrorResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	assert.Equal(t, string(domain.ErrCodeIdempotencyConflict), resp.Code)
}

func TestTransfer_InvalidPriority(t *testing.T) {
	_, signer, handler := setupTransferHandler(t)

	signer.EXPECT().HasAddress(testSender).Return(true)

	rr := doTransferRequest(handler, TransferRequest{
		IdempotencyKey: "key",
		ChainID:        1,
		Sender:         testSender.Hex(),
		Recipient:      testRecipient,
		Token:          "ETH",
		Amount:         "1.0",
		Priority:       "super-duper",
	})

	assert.Equal(t, http.StatusBadRequest, rr.Code)
	var resp ErrorResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	assert.Equal(t, string(domain.ErrCodeInvalidPriority), resp.Code)
}

func TestTransfer_InvalidJSON(t *testing.T) {
	_, _, handler := setupTransferHandler(t)

	req := httptest.NewRequest(http.MethodPost, "/v1/transfers", strings.NewReader("{invalid json"))
	rr := httptest.NewRecorder()
	handler.Create(rr, req)

	assert.Equal(t, http.StatusBadRequest, rr.Code)
}

func TestTransfer_IdempotencyKeyTooLong(t *testing.T) {
	_, _, handler := setupTransferHandler(t)

	longKey := strings.Repeat("x", 257)

	rr := doTransferRequest(handler, TransferRequest{
		IdempotencyKey: longKey,
		ChainID:        1,
		Sender:         testSender.Hex(),
		Recipient:      testRecipient,
		Token:          "ETH",
		Amount:         "1.0",
	})

	assert.Equal(t, http.StatusBadRequest, rr.Code)
	var resp ErrorResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	assert.Equal(t, string(domain.ErrCodeIdempotencyKeyTooLong), resp.Code)
}

func TestTransfer_ERC20_RecipientIsTokenContract(t *testing.T) {
	repo, signer, handler := setupTransferHandler(t)

	signer.EXPECT().HasAddress(testSender).Return(true)
	repo.EXPECT().GetTransactionByIdempotencyKey(gomock.Any(), "key").Return(nil, nil)

	rr := doTransferRequest(handler, TransferRequest{
		IdempotencyKey: "key",
		ChainID:        1,
		Sender:         testSender.Hex(),
		Recipient:      testUSDCAddr.Hex(), // sending USDC to the USDC contract
		Token:          "USDC",
		Amount:         "100",
	})

	assert.Equal(t, http.StatusBadRequest, rr.Code)
	var resp ErrorResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	assert.Equal(t, string(domain.ErrCodeSelfTransferToken), resp.Code)
}

func TestTransfer_ValidPriority(t *testing.T) {
	repo, signer, handler := setupTransferHandler(t)

	signer.EXPECT().HasAddress(testSender).Return(true)
	repo.EXPECT().GetTransactionByIdempotencyKey(gomock.Any(), "key").Return(nil, nil)
	repo.EXPECT().CreateTransaction(gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ interface{}, tx *domain.Transaction) error {
			assert.Equal(t, domain.PriorityUrgent, tx.Priority)
			return nil
		},
	)
	repo.EXPECT().LogStateTransition(gomock.Any(), gomock.Any()).Return(nil)

	rr := doTransferRequest(handler, TransferRequest{
		IdempotencyKey: "key",
		ChainID:        1,
		Sender:         testSender.Hex(),
		Recipient:      testRecipient,
		Token:          "ETH",
		Amount:         "1.0",
		Priority:       "urgent",
	})

	assert.Equal(t, http.StatusAccepted, rr.Code)
}
