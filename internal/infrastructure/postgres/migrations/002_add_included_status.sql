-- Add INCLUDED status to the partial index for active transaction queries.
DROP INDEX IF EXISTS idx_transactions_status_chain_sender;
CREATE INDEX idx_transactions_status_chain_sender
    ON transactions (status, chain_id, sender)
    WHERE status IN ('QUEUED', 'PENDING', 'SUBMITTED', 'INCLUDED');
