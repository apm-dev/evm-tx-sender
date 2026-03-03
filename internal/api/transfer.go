package api

import (
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/apm-dev/evm-tx-sender/internal/domain"
	"github.com/apm-dev/evm-tx-sender/internal/pipeline"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/oklog/ulid/v2"
)

var erc20ABI abi.ABI

func init() {
	parsed, err := abi.JSON(strings.NewReader(`[{
		"name": "transfer",
		"type": "function",
		"inputs": [
			{"name": "recipient", "type": "address"},
			{"name": "amount", "type": "uint256"}
		],
		"outputs": [{"name": "", "type": "bool"}]
	}]`))
	if err != nil {
		panic("failed to parse ERC20 ABI: " + err.Error())
	}
	erc20ABI = parsed
}

type TransferHandler struct {
	repo    domain.Repository
	signer  domain.Signer
	chains  map[uint64]domain.ChainConfig
	manager *pipeline.Manager
}

func NewTransferHandler(
	repo domain.Repository,
	signer domain.Signer,
	chains map[uint64]domain.ChainConfig,
	manager *pipeline.Manager,
) *TransferHandler {
	return &TransferHandler{
		repo:    repo,
		signer:  signer,
		chains:  chains,
		manager: manager,
	}
}

type TransferRequest struct {
	IdempotencyKey string          `json:"idempotency_key"`
	ChainID        uint64          `json:"chain_id"`
	Sender         string          `json:"sender"`
	Recipient      string          `json:"recipient"`
	Token          string          `json:"token"`
	Amount         string          `json:"amount"`
	Priority       string          `json:"priority"`
	Metadata       json.RawMessage `json:"metadata"`
}

type TransferResponse struct {
	ID            string    `json:"id"`
	Status        string    `json:"status"`
	ChainID       uint64    `json:"chain_id"`
	Sender        string    `json:"sender"`
	Recipient     string    `json:"recipient"`
	Token         string    `json:"token"`
	Amount        string    `json:"amount"`
	TransferType  string    `json:"transfer_type"`
	TokenContract string    `json:"token_contract,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
}

func (h *TransferHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req TransferRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, domain.NewAppError(domain.ErrCodeInvalidValue, 400, "invalid JSON body"))
		return
	}

	if appErr := h.validate(req); appErr != nil {
		writeError(w, appErr)
		return
	}

	// Check idempotency
	existing, err := h.repo.GetTransactionByIdempotencyKey(r.Context(), req.IdempotencyKey)
	if err != nil {
		writeError(w, domain.NewAppError(domain.ErrCodeInternalError, 500, "database error"))
		return
	}
	if existing != nil {
		// Check for parameter mismatch
		if !h.idempotencyMatch(existing, req) {
			writeError(w, domain.NewAppError(domain.ErrCodeIdempotencyConflict, 409, "idempotency key already used with different parameters"))
			return
		}
		writeJSON(w, http.StatusAccepted, h.txToTransferResponse(existing))
		return
	}

	// Resolve transfer to transaction parameters
	chain := h.chains[req.ChainID]
	txParams, tokenContract, transferType, decimals, appErr := h.resolveTransfer(req, chain)
	if appErr != nil {
		writeError(w, appErr)
		return
	}

	// Determine priority
	priority := domain.PriorityNormal
	if req.Priority != "" {
		p, ok := domain.ParsePriority(req.Priority)
		if !ok {
			writeError(w, domain.NewAppError(domain.ErrCodeInvalidPriority, 400, "invalid priority"))
			return
		}
		priority = p
	}

	senderAddr := common.HexToAddress(req.Sender)

	now := time.Now().UTC()
	tx := &domain.Transaction{
		ID:                ulid.Make().String(),
		IdempotencyKey:    req.IdempotencyKey,
		ChainID:           req.ChainID,
		Sender:            strings.ToLower(senderAddr.Hex()),
		ToAddress:         strings.ToLower(txParams.To.Hex()),
		Value:             txParams.Value,
		Data:              txParams.Data,
		Priority:          priority,
		Status:            domain.TxStatusQueued,
		TransferToken:     strings.ToUpper(req.Token),
		TransferAmount:    req.Amount,
		TransferRecipient: strings.ToLower(common.HexToAddress(req.Recipient).Hex()),
		TokenContract:     tokenContract,
		TokenDecimals:     decimals,
		Metadata:          req.Metadata,
		CreatedAt:         now,
		UpdatedAt:         now,
	}

	if err := h.repo.CreateTransaction(r.Context(), tx); err != nil {
		writeError(w, domain.NewAppError(domain.ErrCodeInternalError, 500, "failed to create transaction"))
		return
	}

	_ = h.repo.LogStateTransition(r.Context(), &domain.TxStateLog{
		TransactionID: tx.ID,
		ToStatus:      string(domain.TxStatusQueued),
		Actor:         "api",
		Reason:        fmt.Sprintf("transfer %s %s to %s", req.Amount, req.Token, req.Recipient),
	})

	// Notify pipeline
	h.manager.NotifyPipeline(tx.Sender, tx.ChainID)

	resp := TransferResponse{
		ID:            tx.ID,
		Status:        string(domain.TxStatusQueued),
		ChainID:       tx.ChainID,
		Sender:        tx.Sender,
		Recipient:     tx.TransferRecipient,
		Token:         tx.TransferToken,
		Amount:        tx.TransferAmount,
		TransferType:  transferType,
		TokenContract: tokenContract,
		CreatedAt:     tx.CreatedAt,
	}

	writeJSON(w, http.StatusAccepted, resp)
}

type txParams struct {
	To    common.Address
	Value *big.Int
	Data  []byte
}

func (h *TransferHandler) resolveTransfer(req TransferRequest, chain domain.ChainConfig) (*txParams, string, string, uint8, *domain.AppError) {
	token := strings.ToUpper(req.Token)
	recipientAddr := common.HexToAddress(req.Recipient)

	// Native token transfer
	if token == strings.ToUpper(chain.NativeTokenSymbol) {
		amountWei, err := domain.ParseAmountToSmallestUnit(req.Amount, chain.NativeTokenDecimals)
		if err != nil {
			return nil, "", "", 0, domain.NewAppError(domain.ErrCodeInvalidAmount, 400, err.Error())
		}
		if amountWei.Sign() <= 0 {
			return nil, "", "", 0, domain.NewAppError(domain.ErrCodeZeroAmount, 422, "amount must be positive")
		}
		if amountWei.Cmp(domain.MaxUint256) > 0 {
			return nil, "", "", 0, domain.NewAppError(domain.ErrCodeAmountOverflow, 422, "amount exceeds uint256")
		}
		if chain.NativeMaxTransfer != nil && amountWei.Cmp(chain.NativeMaxTransfer) > 0 {
			return nil, "", "", 0, domain.NewAppError(domain.ErrCodeExceedsTransferLimit, 422, "exceeds native transfer limit")
		}

		return &txParams{
			To:    recipientAddr,
			Value: amountWei,
			Data:  nil,
		}, "", "native", chain.NativeTokenDecimals, nil
	}

	// ERC20 transfer
	tokenCfg, ok := chain.TokenWhitelist[token]
	if !ok {
		return nil, "", "", 0, domain.NewAppError(domain.ErrCodeTokenNotWhitelisted, 403, fmt.Sprintf("token %s not whitelisted on chain %d", token, chain.ChainID))
	}
	if !tokenCfg.Enabled {
		return nil, "", "", 0, domain.NewAppError(domain.ErrCodeTokenDisabled, 403, fmt.Sprintf("token %s is disabled", token))
	}

	amountSmallest, err := domain.ParseAmountToSmallestUnit(req.Amount, tokenCfg.Decimals)
	if err != nil {
		return nil, "", "", 0, domain.NewAppError(domain.ErrCodeInvalidAmount, 400, err.Error())
	}
	if amountSmallest.Sign() <= 0 {
		return nil, "", "", 0, domain.NewAppError(domain.ErrCodeZeroAmount, 422, "amount must be positive")
	}
	if amountSmallest.Cmp(domain.MaxUint256) > 0 {
		return nil, "", "", 0, domain.NewAppError(domain.ErrCodeAmountOverflow, 422, "amount exceeds uint256")
	}
	if tokenCfg.MaxTransfer != nil && amountSmallest.Cmp(tokenCfg.MaxTransfer) > 0 {
		return nil, "", "", 0, domain.NewAppError(domain.ErrCodeExceedsTransferLimit, 422, fmt.Sprintf("amount exceeds %s transfer limit", token))
	}

	// Check recipient is not the token contract
	if recipientAddr == tokenCfg.ContractAddress {
		return nil, "", "", 0, domain.NewAppError(domain.ErrCodeSelfTransferToken, 400, "recipient cannot be the token contract address")
	}

	// Encode ERC20 transfer calldata
	data, err := erc20ABI.Pack("transfer", recipientAddr, amountSmallest)
	if err != nil {
		return nil, "", "", 0, domain.NewAppError(domain.ErrCodeInternalError, 500, "failed to encode transfer calldata")
	}

	contractAddr := strings.ToLower(tokenCfg.ContractAddress.Hex())

	return &txParams{
		To:    tokenCfg.ContractAddress,
		Value: big.NewInt(0),
		Data:  data,
	}, contractAddr, "erc20", tokenCfg.Decimals, nil
}

func (h *TransferHandler) validate(req TransferRequest) *domain.AppError {
	if req.IdempotencyKey == "" {
		return domain.NewAppError(domain.ErrCodeMissingField, 400, "idempotency_key is required")
	}
	if len(req.IdempotencyKey) > 256 {
		return domain.NewAppError(domain.ErrCodeIdempotencyKeyTooLong, 400, "idempotency_key max 256 characters")
	}
	if req.ChainID == 0 {
		return domain.NewAppError(domain.ErrCodeMissingField, 400, "chain_id is required")
	}
	if _, ok := h.chains[req.ChainID]; !ok {
		return domain.NewAppError(domain.ErrCodeInvalidChain, 400, fmt.Sprintf("unsupported chain_id %d", req.ChainID))
	}
	if req.Sender == "" {
		return domain.NewAppError(domain.ErrCodeMissingField, 400, "sender is required")
	}
	if !common.IsHexAddress(req.Sender) {
		return domain.NewAppError(domain.ErrCodeInvalidAddress, 400, "invalid sender address")
	}
	senderAddr := common.HexToAddress(req.Sender)
	if !h.signer.HasAddress(senderAddr) {
		return domain.NewAppError(domain.ErrCodeUnknownSender, 404, "sender key not registered")
	}
	if req.Recipient == "" {
		return domain.NewAppError(domain.ErrCodeMissingField, 400, "recipient is required")
	}
	if !common.IsHexAddress(req.Recipient) {
		return domain.NewAppError(domain.ErrCodeInvalidRecipient, 400, "invalid recipient address")
	}
	recipientAddr := common.HexToAddress(req.Recipient)
	if recipientAddr == (common.Address{}) {
		return domain.NewAppError(domain.ErrCodeZeroAddressRecipient, 400, "recipient cannot be zero address")
	}
	if req.Token == "" {
		return domain.NewAppError(domain.ErrCodeMissingField, 400, "token is required")
	}
	if req.Amount == "" {
		return domain.NewAppError(domain.ErrCodeMissingField, 400, "amount is required")
	}
	if req.Priority != "" {
		if _, ok := domain.ParsePriority(req.Priority); !ok {
			return domain.NewAppError(domain.ErrCodeInvalidPriority, 400, "priority must be low, normal, high, or urgent")
		}
	}
	return nil
}

func (h *TransferHandler) idempotencyMatch(existing *domain.Transaction, req TransferRequest) bool {
	return existing.ChainID == req.ChainID &&
		strings.EqualFold(existing.Sender, common.HexToAddress(req.Sender).Hex()) &&
		strings.EqualFold(existing.TransferRecipient, common.HexToAddress(req.Recipient).Hex()) &&
		strings.EqualFold(existing.TransferToken, strings.ToUpper(req.Token)) &&
		existing.TransferAmount == req.Amount
}

func (h *TransferHandler) txToTransferResponse(tx *domain.Transaction) TransferResponse {
	transferType := "native"
	if tx.TokenContract != "" {
		transferType = "erc20"
	}
	return TransferResponse{
		ID:            tx.ID,
		Status:        string(tx.Status),
		ChainID:       tx.ChainID,
		Sender:        tx.Sender,
		Recipient:     tx.TransferRecipient,
		Token:         tx.TransferToken,
		Amount:        tx.TransferAmount,
		TransferType:  transferType,
		TokenContract: tx.TokenContract,
		CreatedAt:     tx.CreatedAt,
	}
}
