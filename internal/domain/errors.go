package domain

import "errors"

type ErrorCode string

const (
	ErrCodeInvalidAddress        ErrorCode = "INVALID_ADDRESS"
	ErrCodeInvalidChain          ErrorCode = "INVALID_CHAIN"
	ErrCodeInvalidValue          ErrorCode = "INVALID_VALUE"
	ErrCodeUnknownSender         ErrorCode = "UNKNOWN_SENDER"
	ErrCodeIdempotencyConflict   ErrorCode = "IDEMPOTENCY_CONFLICT"
	ErrCodeEstimationReverted    ErrorCode = "ESTIMATION_REVERTED"
	ErrCodeInsufficientFunds     ErrorCode = "INSUFFICIENT_FUNDS"
	ErrCodeNonceTooLow           ErrorCode = "NONCE_TOO_LOW"
	ErrCodeRPCUnavailable        ErrorCode = "RPC_UNAVAILABLE"
	ErrCodeBroadcastFailed       ErrorCode = "BROADCAST_FAILED"
	ErrCodeMaxRetriesExceeded    ErrorCode = "MAX_RETRIES_EXCEEDED"
	ErrCodeSigningFailed         ErrorCode = "SIGNING_FAILED"
	ErrCodeDBUnavailable         ErrorCode = "DB_UNAVAILABLE"
	ErrCodeInternalError         ErrorCode = "INTERNAL_ERROR"
	ErrCodeTokenNotWhitelisted   ErrorCode = "TOKEN_NOT_WHITELISTED"
	ErrCodeTokenDisabled         ErrorCode = "TOKEN_DISABLED"
	ErrCodeInvalidAmount         ErrorCode = "INVALID_AMOUNT"
	ErrCodeAmountTooManyDecimals ErrorCode = "AMOUNT_TOO_MANY_DECIMALS"
	ErrCodeZeroAmount            ErrorCode = "ZERO_AMOUNT"
	ErrCodeExceedsTransferLimit  ErrorCode = "EXCEEDS_TRANSFER_LIMIT"
	ErrCodeAmountOverflow        ErrorCode = "AMOUNT_OVERFLOW"
	ErrCodeZeroAddressRecipient  ErrorCode = "ZERO_ADDRESS_RECIPIENT"
	ErrCodeSelfTransferToken     ErrorCode = "SELF_TRANSFER_TOKEN"
	ErrCodeInvalidRecipient      ErrorCode = "INVALID_RECIPIENT"
	ErrCodeInvalidPriority       ErrorCode = "INVALID_PRIORITY"
	ErrCodeMissingField          ErrorCode = "MISSING_FIELD"
	ErrCodeIdempotencyKeyTooLong ErrorCode = "IDEMPOTENCY_KEY_TOO_LONG"
)

type AppError struct {
	Code    ErrorCode
	Message string
	HTTP    int
}

func (e *AppError) Error() string {
	return string(e.Code) + ": " + e.Message
}

func NewAppError(code ErrorCode, httpStatus int, message string) *AppError {
	return &AppError{Code: code, HTTP: httpStatus, Message: message}
}

var (
	ErrNotFound = errors.New("not found")
)
