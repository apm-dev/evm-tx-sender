package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/apm-dev/evm-tx-sender/internal/domain"
	"github.com/go-chi/chi/v5"
)

type TransactionHandler struct {
	repo domain.Repository
}

func NewTransactionHandler(repo domain.Repository) *TransactionHandler {
	return &TransactionHandler{repo: repo}
}

type TxResponse struct {
	ID                string          `json:"id"`
	IdempotencyKey    string          `json:"idempotency_key"`
	Status            string          `json:"status"`
	ChainID           uint64          `json:"chain_id"`
	Sender            string          `json:"sender"`
	ToAddress         string          `json:"to"`
	Value             string          `json:"value"`
	Data              string          `json:"data,omitempty"`
	Nonce             *uint64         `json:"nonce,omitempty"`
	TxHash            string          `json:"tx_hash,omitempty"`
	GasUsed           *uint64         `json:"gas_used,omitempty"`
	EffectiveGasPrice string          `json:"effective_gas_price,omitempty"`
	BlockNumber       *uint64         `json:"block_number,omitempty"`
	BlockHash         string          `json:"block_hash,omitempty"`
	Confirmations     int             `json:"confirmations"`
	ReceiptStatus     *uint8          `json:"receipt_status,omitempty"`
	Error             string          `json:"error,omitempty"`
	ErrorCode         string          `json:"error_code,omitempty"`
	TransferToken     string          `json:"transfer_token"`
	TransferAmount    string          `json:"transfer_amount"`
	TransferRecipient string          `json:"transfer_recipient"`
	TokenContract     string          `json:"token_contract,omitempty"`
	Metadata          json.RawMessage `json:"metadata,omitempty"`
	Attempts          []AttemptResp   `json:"attempts,omitempty"`
	CreatedAt         time.Time       `json:"created_at"`
	UpdatedAt         time.Time       `json:"updated_at"`
	SubmittedAt       *time.Time      `json:"submitted_at,omitempty"`
	ConfirmedAt       *time.Time      `json:"confirmed_at,omitempty"`
}

type AttemptResp struct {
	AttemptNumber        int    `json:"attempt_number"`
	TxHash               string `json:"tx_hash"`
	MaxFeePerGas         string `json:"max_fee_per_gas,omitempty"`
	MaxPriorityFeePerGas string `json:"max_priority_fee_per_gas,omitempty"`
	GasPrice             string `json:"gas_price,omitempty"`
	Status               string `json:"status"`
	SubmittedAt          string `json:"submitted_at"`
}

// Get retrieves a single transaction by ID.
//
// @Summary      Get transaction
// @Description  Fetch a transaction by its ULID identifier, including all broadcast attempts and on-chain receipt data.
// @Tags         transactions
// @Produce      json
// @Param        id   path      string       true  "Transaction ULID"
// @Success      200  {object}  TxResponse   "Transaction details"
// @Failure      404  {object}  ErrorResponse  "Transaction not found"
// @Failure      500  {object}  ErrorResponse  "Internal server error"
// @Router       /transactions/{id} [get]
func (h *TransactionHandler) Get(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		writeError(w, domain.NewAppError(domain.ErrCodeMissingField, 400, "id is required"))
		return
	}

	tx, err := h.repo.GetTransaction(r.Context(), id)
	if err != nil {
		writeError(w, domain.NewAppError(domain.ErrCodeInternalError, 500, "database error"))
		return
	}
	if tx == nil {
		writeError(w, domain.NewAppError(domain.ErrCodeInternalError, 404, "transaction not found"))
		return
	}

	attempts, _ := h.repo.GetAttemptsByTransactionID(r.Context(), id)

	resp := h.toResponse(tx, attempts)
	writeJSON(w, http.StatusOK, resp)
}

// List returns a paginated list of transactions with optional filters.
//
// @Summary      List transactions
// @Description  List transactions with optional filtering by chain, sender, or status.
// @Description  Supports cursor-based pagination via the `after` query parameter (pass the last transaction ID from the previous page).
// @Tags         transactions
// @Produce      json
// @Param        chain_id  query     integer         false  "Filter by chain ID"
// @Param        sender    query     string          false  "Filter by sender address (hex)"
// @Param        status    query     string          false  "Filter by status (QUEUED|PENDING|SUBMITTED|CONFIRMED|REVERTED|FAILED)"
// @Param        limit     query     integer         false  "Maximum number of results to return (default: 50)"
// @Param        after     query     string          false  "Cursor for pagination: ULID of the last transaction from the previous page"
// @Success      200       {array}   TxResponse      "List of transactions"
// @Failure      400       {object}  ErrorResponse   "Invalid query parameter"
// @Failure      500       {object}  ErrorResponse   "Internal server error"
// @Router       /transactions [get]
func (h *TransactionHandler) List(w http.ResponseWriter, r *http.Request) {
	filter := domain.TxFilter{
		Sender: strings.ToLower(r.URL.Query().Get("sender")),
		Status: strings.ToUpper(r.URL.Query().Get("status")),
		After:  r.URL.Query().Get("after"),
	}

	if chainStr := r.URL.Query().Get("chain_id"); chainStr != "" {
		chainID, err := strconv.ParseUint(chainStr, 10, 64)
		if err != nil {
			writeError(w, domain.NewAppError(domain.ErrCodeInvalidChain, 400, "invalid chain_id"))
			return
		}
		filter.ChainID = &chainID
	}

	if limitStr := r.URL.Query().Get("limit"); limitStr != "" {
		limit, _ := strconv.Atoi(limitStr)
		filter.Limit = limit
	}

	txs, err := h.repo.ListTransactions(r.Context(), filter)
	if err != nil {
		writeError(w, domain.NewAppError(domain.ErrCodeInternalError, 500, "database error"))
		return
	}

	responses := make([]TxResponse, 0, len(txs))
	for _, tx := range txs {
		responses = append(responses, h.toResponse(tx, nil))
	}

	writeJSON(w, http.StatusOK, responses)
}

func (h *TransactionHandler) toResponse(tx *domain.Transaction, attempts []*domain.TxAttempt) TxResponse {
	resp := TxResponse{
		ID:                tx.ID,
		IdempotencyKey:    tx.IdempotencyKey,
		Status:            string(tx.Status),
		ChainID:           tx.ChainID,
		Sender:            tx.Sender,
		ToAddress:         tx.ToAddress,
		Value:             tx.Value.String(),
		Nonce:             tx.Nonce,
		TxHash:            tx.FinalTxHash,
		GasUsed:           tx.GasUsed,
		BlockNumber:       tx.BlockNumber,
		BlockHash:         tx.BlockHash,
		Confirmations:     tx.Confirmations,
		ReceiptStatus:     tx.ReceiptStatus,
		Error:             tx.ErrorReason,
		ErrorCode:         tx.ErrorCode,
		TransferToken:     tx.TransferToken,
		TransferAmount:    tx.TransferAmount,
		TransferRecipient: tx.TransferRecipient,
		TokenContract:     tx.TokenContract,
		Metadata:          tx.Metadata,
		CreatedAt:         tx.CreatedAt,
		UpdatedAt:         tx.UpdatedAt,
		SubmittedAt:       tx.SubmittedAt,
		ConfirmedAt:       tx.ConfirmedAt,
	}

	if tx.EffectiveGasPrice != nil {
		resp.EffectiveGasPrice = tx.EffectiveGasPrice.String()
	}

	if tx.Data != nil {
		resp.Data = "0x" + encodeHex(tx.Data)
	}

	for _, a := range attempts {
		ar := AttemptResp{
			AttemptNumber: a.AttemptNumber,
			TxHash:        a.TxHash,
			Status:        string(a.Status),
			SubmittedAt:   a.CreatedAt.Format(time.RFC3339),
		}
		if a.MaxFeePerGas != nil {
			ar.MaxFeePerGas = a.MaxFeePerGas.String()
		}
		if a.MaxPriorityFeePerGas != nil {
			ar.MaxPriorityFeePerGas = a.MaxPriorityFeePerGas.String()
		}
		if a.GasPrice != nil {
			ar.GasPrice = a.GasPrice.String()
		}
		resp.Attempts = append(resp.Attempts, ar)
	}

	return resp
}

func encodeHex(b []byte) string {
	const hextable = "0123456789abcdef"
	dst := make([]byte, len(b)*2)
	for i, v := range b {
		dst[i*2] = hextable[v>>4]
		dst[i*2+1] = hextable[v&0x0f]
	}
	return string(dst)
}
