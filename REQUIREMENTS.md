# Requirements: EVM Transaction Sender

## What is this

A microservice that sends token transfers on EVM chains. It sits behind our solver infra and handles the ugly parts: nonce management, gas estimation, retries, receipts. Callers just say "send 100 USDC to 0x... on Arbitrum" and poll for the result.

Scale target: hundreds of TPS across all chains combined, multiple txs per block on any given chain.

## Core features

- Multi-chain: support any EVM chain, configured via env vars (RPC endpoints, chain ID, block time, etc.)
- Multi-EOA: send from multiple wallets, private keys loaded from env
- Nonce management that actually works under concurrency -- this is the hard part
- Gas estimation with EIP-1559 support (fallback to legacy for chains that don't support it)
- Auto-retry stuck transactions by bumping gas
- Idempotent submissions -- same idempotency key = same result, no double-sends
- Crash recovery -- if the process dies mid-flight, we pick up where we left off

## API

Single submission endpoint. We don't want callers constructing raw transactions -- too error-prone and a security risk. Instead, a transfer-oriented API:

- `POST /v1/transfers` -- submit a transfer (token, amount, recipient, chain, sender)
  - Accepts human-readable amounts ("100.5" not "100500000")
  - Resolves token symbol against a per-chain whitelist
  - Encodes ERC20 calldata internally
  - Returns 202 with a transaction ID immediately
- `GET /v1/transactions/:id` -- poll for status and receipt
- `GET /v1/transactions` -- list with filters (chain, sender, status)
- `GET /health` -- basic health check

The caller should never need to know about nonces, gas prices, or calldata encoding.

### Transfer request fields

- `idempotency_key` (required) -- client-provided, used to deduplicate
- `chain_id` -- which chain
- `sender` -- which EOA to send from (must be registered)
- `recipient` -- destination address
- `token` -- symbol like "ETH", "USDC", "USDT" -- native token triggers a value transfer, anything else looks up the ERC20 whitelist
- `amount` -- decimal string, converted using the token's decimals
- `priority` -- optional, one of low/normal/high/urgent (affects gas pricing)
- `metadata` -- optional opaque JSON blob, stored but not used by us

### Idempotency

If the same `idempotency_key` comes in with the same params, return the original response. If the key is reused with different params, return 409.

## Token whitelist

Each chain has a list of allowed ERC20 tokens with:
- Contract address
- Decimals
- Max transfer limit
- Enabled/disabled flag

Native token (ETH, MATIC, etc.) is implicitly supported per chain with its own max transfer limit. Anything not on the whitelist gets rejected. This is a security feature -- we don't want this service calling arbitrary contracts.

## Nonce management

This is the crux of the whole thing. Nonce gaps stall everything. Nonce collisions waste gas. Two approaches that don't work well: shared worker pool with locks, or querying the chain for pending nonce on every send.

What we want: a dedicated goroutine per (sender, chain) pair that processes transactions sequentially. The goroutine IS the lock. No races by construction. Maintain a local nonce cursor in the DB, increment atomically per send. Use on-chain nonce as a safety net / reconciliation, not as the primary source.

On startup, reconcile the DB cursor against on-chain state. Reset any in-flight (PENDING) transactions back to QUEUED and reprocess.

## Transaction lifecycle

```
QUEUED -> PENDING -> SUBMITTED -> INCLUDED -> CONFIRMED
                                           \-> REVERTED
                  \-> FAILED
```

- QUEUED: written to DB, waiting for pipeline
- PENDING: pipeline claimed it, assigning nonce
- SUBMITTED: broadcast to the network, waiting for receipt
- INCLUDED: receipt found but not enough confirmations yet (optional, for chains where we want confirmation depth)
- CONFIRMED: receipt with status=1 and enough confirmations
- REVERTED: receipt with status=0 (on-chain revert)
- FAILED: couldn't even get it on-chain after retries (bad gas estimate, signing error, etc.)

## Gas strategy

- EIP-1559 by default on chains that support it, legacy fallback otherwise
- Four priority tiers with different fee multipliers
- If a tx is stuck (no receipt after chain-specific timeout), bump gas by ~15% and resubmit with the same nonce
- Cap bumps at some reasonable max (don't want runaway gas costs)
- Track each bump as a separate "attempt" linked to the same logical transaction

## RPC handling

- Multiple RPC endpoints per chain for redundancy
- Health checking, automatic failover
- For sendRawTransaction: broadcast to ALL healthy endpoints simultaneously (maximizes mempool propagation)
- For reads: use any healthy endpoint

## Persistence

PostgreSQL. Transactions are money -- we need ACID.

Key tables:
- `transactions` -- the main state, one row per logical transfer
- `tx_attempts` -- each broadcast attempt (original + gas bumps), tracks gas params and tx hash
- `nonce_cursors` -- current nonce per (sender, chain)
- `tx_state_log` -- audit trail of every state transition

Embedded migrations, run on startup.

## Background workers

- **Receipt poller** -- batch-poll for receipts of SUBMITTED txs, also check INCLUDED txs for enough confirmations
- **Stuck detector** -- find txs that have been SUBMITTED too long, trigger gas bumps
- **Reorg handling** -- if a confirmed tx's block hash changes, revert to SUBMITTED and re-check

Receipt polling should be paginated (don't try to load 10k submitted txs into memory at once).

## Error handling

- Structured error codes on all API responses (not just "internal server error")
- Every state transition logged to the audit table
- Failed txs should have a clear error reason (gas estimation failed, max bumps exceeded, signing error, etc.)
- Don't silently swallow errors in background workers

## Security considerations

- No arbitrary calldata -- only transfers through the whitelist
- Per-token max transfer limits
- Private keys in env vars (HSM integration is a future thing)
- Validate all addresses, reject zero address recipients
- Don't let callers send to the token contract itself

## Non-goals (for now)

- No frontend
- No auth (assumed to be behind a private network / VPN)
- No multi-instance pipeline coordination (single instance runs pipelines, can scale API separately later)
- No WebSocket subscriptions (polling is fine for our use case)
- No support for arbitrary contract calls
- No MEV protection (future improvement)

## Tech stack

- Go
- PostgreSQL
- chi router
- go-ethereum for all EVM interaction
- pgx for Postgres
