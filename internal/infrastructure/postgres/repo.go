package postgres

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/apm-dev/evm-tx-sender/internal/domain"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

type Repo struct {
	pool *pgxpool.Pool
}

func NewRepo(pool *pgxpool.Pool) *Repo {
	return &Repo{pool: pool}
}

func (r *Repo) Migrate(ctx context.Context) error {
	sql, err := migrationsFS.ReadFile("migrations/001_init.sql")
	if err != nil {
		return fmt.Errorf("reading migration: %w", err)
	}
	_, err = r.pool.Exec(ctx, string(sql))
	return err
}

func (r *Repo) CreateTransaction(ctx context.Context, tx *domain.Transaction) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO transactions (
			id, idempotency_key, chain_id, sender, to_address, value, data, priority,
			status, transfer_token, transfer_amount, transfer_recipient, token_contract,
			token_decimals, metadata, created_at, updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)`,
		tx.ID, tx.IdempotencyKey, tx.ChainID, tx.Sender, tx.ToAddress,
		tx.Value.String(), tx.Data, string(tx.Priority),
		string(tx.Status), tx.TransferToken, tx.TransferAmount, tx.TransferRecipient,
		nullIfEmpty(tx.TokenContract), tx.TokenDecimals, tx.Metadata,
		tx.CreatedAt, tx.UpdatedAt,
	)
	return err
}

func (r *Repo) GetTransaction(ctx context.Context, id string) (*domain.Transaction, error) {
	row := r.pool.QueryRow(ctx, `SELECT `+txColumns+` FROM transactions WHERE id = $1`, id)
	return scanTransaction(row)
}

func (r *Repo) GetTransactionByIdempotencyKey(ctx context.Context, key string) (*domain.Transaction, error) {
	row := r.pool.QueryRow(ctx, `SELECT `+txColumns+` FROM transactions WHERE idempotency_key = $1`, key)
	tx, err := scanTransaction(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return tx, err
}

func (r *Repo) ListTransactions(ctx context.Context, filter domain.TxFilter) ([]*domain.Transaction, error) {
	query := `SELECT ` + txColumns + ` FROM transactions WHERE 1=1`
	args := []any{}
	idx := 1

	if filter.ChainID != nil {
		query += fmt.Sprintf(" AND chain_id = $%d", idx)
		args = append(args, *filter.ChainID)
		idx++
	}
	if filter.Sender != "" {
		query += fmt.Sprintf(" AND sender = $%d", idx)
		args = append(args, filter.Sender)
		idx++
	}
	if filter.Status != "" {
		query += fmt.Sprintf(" AND status = $%d", idx)
		args = append(args, filter.Status)
		idx++
	}
	if filter.After != "" {
		query += fmt.Sprintf(" AND id > $%d", idx)
		args = append(args, filter.After)
		idx++
	}

	query += " ORDER BY id ASC"

	limit := filter.Limit
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	query += fmt.Sprintf(" LIMIT $%d", idx)
	args = append(args, limit)

	rows, err := r.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var txs []*domain.Transaction
	for rows.Next() {
		tx, err := scanTransactionRows(rows)
		if err != nil {
			return nil, err
		}
		txs = append(txs, tx)
	}
	return txs, rows.Err()
}

func (r *Repo) ClaimNextQueued(ctx context.Context, sender string, chainID uint64, claimedBy string) (*domain.Transaction, error) {
	now := time.Now().UTC()
	row := r.pool.QueryRow(ctx, `
		UPDATE transactions
		SET status = 'PENDING', claimed_by = $1, claimed_at = $2, updated_at = $2
		WHERE id = (
			SELECT id FROM transactions
			WHERE status = 'QUEUED' AND sender = $3 AND chain_id = $4
			ORDER BY created_at ASC
			LIMIT 1
			FOR UPDATE SKIP LOCKED
		)
		RETURNING `+txColumns,
		claimedBy, now, sender, chainID,
	)
	tx, err := scanTransaction(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return tx, err
}

func (r *Repo) MarkPending(ctx context.Context, id string, nonce uint64) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE transactions SET nonce = $1, updated_at = NOW() WHERE id = $2`,
		nonce, id,
	)
	return err
}

func (r *Repo) MarkSubmitted(ctx context.Context, id string, txHash string, _ *big.Int) error {
	now := time.Now().UTC()
	_, err := r.pool.Exec(ctx, `
		UPDATE transactions
		SET status = 'SUBMITTED', final_tx_hash = $1, submitted_at = $2, updated_at = $2
		WHERE id = $3`,
		txHash, now, id,
	)
	return err
}

func (r *Repo) MarkConfirmed(ctx context.Context, id string, receipt *domain.TxReceipt) error {
	now := time.Now().UTC()
	_, err := r.pool.Exec(ctx, `
		UPDATE transactions
		SET status = 'CONFIRMED', final_tx_hash = $1, block_number = $2, block_hash = $3,
			gas_used = $4, effective_gas_price = $5, receipt_status = $6, receipt_data = $7,
			confirmed_at = $8, updated_at = $8
		WHERE id = $9`,
		receipt.TxHash, receipt.BlockNumber, receipt.BlockHash,
		receipt.GasUsed, receipt.EffectiveGasPrice.String(), receipt.Status, receipt.ReceiptJSON,
		now, id,
	)
	return err
}

func (r *Repo) MarkReverted(ctx context.Context, id string, receipt *domain.TxReceipt) error {
	now := time.Now().UTC()
	_, err := r.pool.Exec(ctx, `
		UPDATE transactions
		SET status = 'REVERTED', final_tx_hash = $1, block_number = $2, block_hash = $3,
			gas_used = $4, effective_gas_price = $5, receipt_status = $6, receipt_data = $7,
			confirmed_at = $8, updated_at = $8
		WHERE id = $9`,
		receipt.TxHash, receipt.BlockNumber, receipt.BlockHash,
		receipt.GasUsed, receipt.EffectiveGasPrice.String(), receipt.Status, receipt.ReceiptJSON,
		now, id,
	)
	return err
}

func (r *Repo) MarkFailed(ctx context.Context, id string, errCode domain.ErrorCode, errReason string) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE transactions
		SET status = 'FAILED', error_code = $1, error_reason = $2, updated_at = NOW()
		WHERE id = $3`,
		string(errCode), errReason, id,
	)
	return err
}

func (r *Repo) ResetPendingToQueued(ctx context.Context) (int, error) {
	tag, err := r.pool.Exec(ctx, `
		UPDATE transactions
		SET status = 'QUEUED', claimed_by = NULL, claimed_at = NULL, nonce = NULL, updated_at = NOW()
		WHERE status = 'PENDING'`)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// Attempts

func (r *Repo) CreateAttempt(ctx context.Context, a *domain.TxAttempt) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO tx_attempts (
			id, transaction_id, attempt_number, tx_hash, gas_limit,
			max_fee_per_gas, max_priority_fee_per_gas, gas_price, tx_type,
			raw_tx, status, created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
		a.ID, a.TransactionID, a.AttemptNumber, a.TxHash,
		a.GasLimit, bigIntToString(a.MaxFeePerGas), bigIntToString(a.MaxPriorityFeePerGas),
		bigIntToString(a.GasPrice), a.TxType,
		a.RawTx, string(a.Status), a.CreatedAt,
	)
	return err
}

func (r *Repo) GetAttemptsByTransactionID(ctx context.Context, txID string) ([]*domain.TxAttempt, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT `+attemptColumns+`
		FROM tx_attempts WHERE transaction_id = $1 ORDER BY attempt_number ASC`, txID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var attempts []*domain.TxAttempt
	for rows.Next() {
		a, err := scanAttempt(rows)
		if err != nil {
			return nil, err
		}
		attempts = append(attempts, a)
	}
	return attempts, rows.Err()
}

func (r *Repo) MarkAttemptStatus(ctx context.Context, attemptID string, status domain.AttemptStatus) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE tx_attempts SET status = $1 WHERE id = $2`,
		string(status), attemptID,
	)
	return err
}

func (r *Repo) GetBroadcastAttemptsByChain(ctx context.Context, chainID uint64) ([]*domain.TxAttempt, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT `+attemptColumns+`
		FROM tx_attempts a
		JOIN transactions t ON a.transaction_id = t.id
		WHERE t.chain_id = $1 AND a.status = 'BROADCAST'
		ORDER BY a.created_at ASC`, chainID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var attempts []*domain.TxAttempt
	for rows.Next() {
		a, err := scanAttempt(rows)
		if err != nil {
			return nil, err
		}
		attempts = append(attempts, a)
	}
	return attempts, rows.Err()
}

// Nonce

func (r *Repo) GetNonceCursor(ctx context.Context, sender string, chainID uint64) (*domain.NonceCursor, error) {
	var nc domain.NonceCursor
	err := r.pool.QueryRow(ctx, `
		SELECT sender, chain_id, next_nonce, updated_at
		FROM nonce_cursors WHERE sender = $1 AND chain_id = $2`,
		sender, chainID,
	).Scan(&nc.Sender, &nc.ChainID, &nc.NextNonce, &nc.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &nc, nil
}

func (r *Repo) InitNonceCursor(ctx context.Context, sender string, chainID uint64, nonce uint64) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO nonce_cursors (sender, chain_id, next_nonce, updated_at)
		VALUES ($1, $2, $3, NOW())
		ON CONFLICT (sender, chain_id) DO UPDATE SET next_nonce = GREATEST(nonce_cursors.next_nonce, $3), updated_at = NOW()`,
		sender, chainID, nonce,
	)
	return err
}

func (r *Repo) IncrementNonceCursor(ctx context.Context, sender string, chainID uint64) (uint64, error) {
	var assigned uint64
	err := r.pool.QueryRow(ctx, `
		UPDATE nonce_cursors
		SET next_nonce = next_nonce + 1, updated_at = NOW()
		WHERE sender = $1 AND chain_id = $2
		RETURNING next_nonce - 1`,
		sender, chainID,
	).Scan(&assigned)
	return assigned, err
}

func (r *Repo) SetNonceCursor(ctx context.Context, sender string, chainID uint64, nonce uint64) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE nonce_cursors SET next_nonce = $1, updated_at = NOW()
		WHERE sender = $2 AND chain_id = $3`,
		nonce, sender, chainID,
	)
	return err
}

// State log

func (r *Repo) LogStateTransition(ctx context.Context, log *domain.TxStateLog) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO tx_state_log (transaction_id, from_status, to_status, actor, reason, metadata, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, NOW())`,
		log.TransactionID, log.FromStatus, log.ToStatus, log.Actor, log.Reason, log.Metadata,
	)
	return err
}

// Queries for background workers

func (r *Repo) GetSubmittedTransactions(ctx context.Context, chainID uint64) ([]*domain.Transaction, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT `+txColumns+` FROM transactions WHERE status = 'SUBMITTED' AND chain_id = $1 ORDER BY submitted_at ASC`, chainID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var txs []*domain.Transaction
	for rows.Next() {
		tx, err := scanTransactionRows(rows)
		if err != nil {
			return nil, err
		}
		txs = append(txs, tx)
	}
	return txs, rows.Err()
}

func (r *Repo) GetStuckTransactions(ctx context.Context, chainID uint64, _ big.Int) ([]*domain.Transaction, error) {
	// Not used in the primary flow; the stuck detection is done via time check in the background worker.
	return r.GetSubmittedTransactions(ctx, chainID)
}

func (r *Repo) CountQueuedTransactions(ctx context.Context, sender string, chainID uint64) (int, error) {
	var count int
	err := r.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM transactions WHERE status = 'QUEUED' AND sender = $1 AND chain_id = $2`,
		sender, chainID,
	).Scan(&count)
	return count, err
}

// Helpers

const txColumns = `id, idempotency_key, chain_id, sender, to_address, value, data, gas_limit, nonce, priority,
	status, error_reason, error_code, final_tx_hash, block_number, block_hash, gas_used,
	effective_gas_price, receipt_status, receipt_data, confirmations,
	transfer_token, transfer_amount, transfer_recipient, token_contract, token_decimals,
	metadata, created_at, updated_at, submitted_at, confirmed_at, claimed_by, claimed_at`

func scanTransaction(row pgx.Row) (*domain.Transaction, error) {
	var tx domain.Transaction
	var valueStr string
	var effectiveGasPriceStr *string
	var gasLimit, nonce, blockNumber, gasUsed *int64
	var receiptStatus *int16

	err := row.Scan(
		&tx.ID, &tx.IdempotencyKey, &tx.ChainID, &tx.Sender, &tx.ToAddress,
		&valueStr, &tx.Data, &gasLimit, &nonce, &tx.Priority,
		&tx.Status, &tx.ErrorReason, &tx.ErrorCode,
		&tx.FinalTxHash, &blockNumber, &tx.BlockHash, &gasUsed,
		&effectiveGasPriceStr, &receiptStatus, &tx.ReceiptData, &tx.Confirmations,
		&tx.TransferToken, &tx.TransferAmount, &tx.TransferRecipient, &tx.TokenContract, &tx.TokenDecimals,
		&tx.Metadata, &tx.CreatedAt, &tx.UpdatedAt, &tx.SubmittedAt, &tx.ConfirmedAt,
		&tx.ClaimedBy, &tx.ClaimedAt,
	)
	if err != nil {
		return nil, err
	}

	tx.Value, _ = new(big.Int).SetString(valueStr, 10)
	if tx.Value == nil {
		tx.Value = big.NewInt(0)
	}

	if gasLimit != nil {
		v := uint64(*gasLimit)
		tx.GasLimit = &v
	}
	if nonce != nil {
		v := uint64(*nonce)
		tx.Nonce = &v
	}
	if blockNumber != nil {
		v := uint64(*blockNumber)
		tx.BlockNumber = &v
	}
	if gasUsed != nil {
		v := uint64(*gasUsed)
		tx.GasUsed = &v
	}
	if receiptStatus != nil {
		v := uint8(*receiptStatus)
		tx.ReceiptStatus = &v
	}
	if effectiveGasPriceStr != nil {
		tx.EffectiveGasPrice, _ = new(big.Int).SetString(*effectiveGasPriceStr, 10)
	}

	return &tx, nil
}

func scanTransactionRows(rows pgx.Rows) (*domain.Transaction, error) {
	var tx domain.Transaction
	var valueStr string
	var effectiveGasPriceStr *string
	var gasLimit, nonce, blockNumber, gasUsed *int64
	var receiptStatus *int16

	err := rows.Scan(
		&tx.ID, &tx.IdempotencyKey, &tx.ChainID, &tx.Sender, &tx.ToAddress,
		&valueStr, &tx.Data, &gasLimit, &nonce, &tx.Priority,
		&tx.Status, &tx.ErrorReason, &tx.ErrorCode,
		&tx.FinalTxHash, &blockNumber, &tx.BlockHash, &gasUsed,
		&effectiveGasPriceStr, &receiptStatus, &tx.ReceiptData, &tx.Confirmations,
		&tx.TransferToken, &tx.TransferAmount, &tx.TransferRecipient, &tx.TokenContract, &tx.TokenDecimals,
		&tx.Metadata, &tx.CreatedAt, &tx.UpdatedAt, &tx.SubmittedAt, &tx.ConfirmedAt,
		&tx.ClaimedBy, &tx.ClaimedAt,
	)
	if err != nil {
		return nil, err
	}

	tx.Value, _ = new(big.Int).SetString(valueStr, 10)
	if tx.Value == nil {
		tx.Value = big.NewInt(0)
	}

	if gasLimit != nil {
		v := uint64(*gasLimit)
		tx.GasLimit = &v
	}
	if nonce != nil {
		v := uint64(*nonce)
		tx.Nonce = &v
	}
	if blockNumber != nil {
		v := uint64(*blockNumber)
		tx.BlockNumber = &v
	}
	if gasUsed != nil {
		v := uint64(*gasUsed)
		tx.GasUsed = &v
	}
	if receiptStatus != nil {
		v := uint8(*receiptStatus)
		tx.ReceiptStatus = &v
	}
	if effectiveGasPriceStr != nil {
		tx.EffectiveGasPrice, _ = new(big.Int).SetString(*effectiveGasPriceStr, 10)
	}

	return &tx, nil
}

const attemptColumns = `id, transaction_id, attempt_number, tx_hash, gas_limit,
	max_fee_per_gas, max_priority_fee_per_gas, gas_price, tx_type,
	raw_tx, status, error_reason, created_at, confirmed_at`

func scanAttempt(rows pgx.Rows) (*domain.TxAttempt, error) {
	var a domain.TxAttempt
	var maxFeeStr, maxPriorityStr, gasPriceStr *string

	err := rows.Scan(
		&a.ID, &a.TransactionID, &a.AttemptNumber, &a.TxHash, &a.GasLimit,
		&maxFeeStr, &maxPriorityStr, &gasPriceStr, &a.TxType,
		&a.RawTx, &a.Status, &a.ErrorReason, &a.CreatedAt, &a.ConfirmedAt,
	)
	if err != nil {
		return nil, err
	}

	if maxFeeStr != nil {
		a.MaxFeePerGas, _ = new(big.Int).SetString(*maxFeeStr, 10)
	}
	if maxPriorityStr != nil {
		a.MaxPriorityFeePerGas, _ = new(big.Int).SetString(*maxPriorityStr, 10)
	}
	if gasPriceStr != nil {
		a.GasPrice, _ = new(big.Int).SetString(*gasPriceStr, 10)
	}

	return &a, nil
}

func bigIntToString(b *big.Int) *string {
	if b == nil {
		return nil
	}
	s := b.String()
	return &s
}

func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
