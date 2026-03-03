# EVM Transaction Sender -- Architecture Document

## Table of Contents

1. [Executive Summary](#1-executive-summary)
2. [Technology Choices](#2-technology-choices)
3. [High-Level Architecture](#3-high-level-architecture)
4. [Component Deep-Dives](#4-component-deep-dives)
5. [API Design](#5-api-design)
6. [Token Whitelist & Transfer Abstraction](#6-token-whitelist--transfer-abstraction)
7. [Nonce Management](#7-nonce-management)
8. [Gas Strategy](#8-gas-strategy)
9. [Transaction Lifecycle & State Machine](#9-transaction-lifecycle--state-machine)
10. [Retry & Recovery](#10-retry--recovery)
11. [Persistence & Schema](#11-persistence--schema)
12. [Security](#12-security)
13. [Observability](#13-observability)
14. [Configuration Model](#14-configuration-model)
15. [Error Taxonomy](#15-error-taxonomy)
16. [Edge Cases & Failure Modes](#16-edge-cases--failure-modes)
17. [Assumptions](#17-assumptions)
18. [Future Improvements](#18-future-improvements)

---

## 1. Executive Summary

This document specifies the architecture for a production-grade EVM transaction sender
microservice that powers solver infrastructure. The system must reliably send hundreds of
transactions per second across multiple EVM chains from multiple EOAs, with correct nonce
management, gas optimization, comprehensive retry logic, and full observability.

**The single hardest problem is nonce management under concurrency.** Every architectural
decision flows from this constraint. A nonce gap stalls all subsequent transactions from that
sender on that chain. A nonce collision wastes gas and creates unpredictable behavior. A stuck
transaction at nonce N blocks nonces N+1, N+2, ... indefinitely. The architecture is designed
around making nonce management correct first, then fast.

**Key architectural decisions:**

- **Golang** for the service -- low-latency, excellent concurrency primitives, strong ecosystem
  for EVM interaction.
- **Per-sender-per-chain serial nonce pipeline** -- each (EOA, chain) pair gets a dedicated
  goroutine that processes transactions sequentially with respect to nonce assignment. This
  eliminates nonce races by construction rather than by locking.
- **PostgreSQL** for persistence -- transactions are money; we need ACID guarantees, durable
  state, and queryability.
- **Asynchronous submission model** -- clients submit a request, receive an ID immediately, and
  poll or subscribe for the receipt. This decouples submission latency from confirmation latency,
  which can be 2 seconds to 15 minutes depending on the chain and gas conditions.
- **Purpose-built for token transfers** -- a single `POST /v1/transfers` endpoint handles native
  and ERC20 token transfers exclusively. The endpoint handles calldata encoding, decimal
  conversion, and enforces a per-chain token whitelist. Arbitrary contract interactions are
  intentionally not supported -- this constraint is a security feature, not a limitation. A
  solver infrastructure handling real money should never expose a general-purpose transaction
  submission interface.
- **Token whitelist** -- ERC20 tokens must be explicitly whitelisted per chain with verified
  contract addresses, decimals, and transfer limits. The transfer endpoint rejects any token not
  on the whitelist. Only whitelisted tokens can be moved through this service. This prevents
  interaction with malicious or incorrect contracts and provides a clear, auditable manifest of
  what the service can do.

---

## 2. Technology Choices

### Language: Go 1.22+

**Why Go:**
- Goroutines are the natural primitive for "one pipeline per sender per chain" -- we will have
  O(senders x chains) long-lived goroutines, trivial in Go, awkward in most other languages.
- `go-ethereum` (`geth`) client library is the canonical EVM interaction library, maintained by the
  Ethereum Foundation. It handles ABI encoding, transaction signing, RPC calls, receipt parsing,
  and EIP-1559 fee structures natively.
- Predictable latency -- no GC pauses that matter at our scale (sub-millisecond P99 GC pauses
  with Go 1.22+ generational GC).
- Static binary, simple deployment.
- Strong standard library for HTTP servers, context propagation, structured concurrency.

**Why not Node.js/TypeScript:** While viem/ethers.js are excellent, Node's single-threaded event
loop plus the need for O(senders x chains) independent pipelines would require either worker
threads (complex) or a message-queue-per-pipeline architecture (over-engineered). Go's goroutines
are a strictly better fit.

**Why not Rust:** Excellent technically, but slower iteration speed and the EVM library ecosystem
(`alloy`/`ethers-rs`) is less battle-tested than `go-ethereum` for production transaction sending.
For a solver infrastructure where correctness matters more than squeezing the last microsecond of
latency, Go's pragmatism wins.

### Database: PostgreSQL 15+

**Why Postgres:**
- ACID transactions for nonce reservation and state transitions -- we cannot lose track of a
  pending transaction.
- `SERIALIZABLE` or `SELECT ... FOR UPDATE` isolation for nonce slot assignment.
- JSONB for chain-specific metadata that varies across EVM chains.
- Excellent operational tooling, battle-tested at scale.
- Partitioning by chain_id for query performance as the table grows.
- Advisory locks for distributed nonce coordination if we ever need multiple instances.

**Why not Redis:** Redis is fast but not durable by default. A Redis restart could lose pending
nonce state, which is catastrophic. We use Postgres as the source of truth. We could add Redis as
a cache layer later, but it is not needed at our scale.

**Why not SQLite:** Cannot handle concurrent writers from multiple goroutines efficiently. No
network-accessible locking for future multi-instance deployment.

### HTTP Framework: Standard library `net/http` + `chi` router

Minimal dependencies. chi provides composable middleware (logging, recovery, request ID) without
framework lock-in. No need for gRPC at this stage -- the API surface is small and the callers are
internal services that speak HTTP/JSON.

### Key Libraries

| Library                | Purpose                                    |
|------------------------|--------------------------------------------|
| `github.com/ethereum/go-ethereum` | EVM interaction, tx signing, ABI, RPC |
| `github.com/jackc/pgx/v5`        | PostgreSQL driver (pure Go, pool-aware) |
| `github.com/go-chi/chi/v5`       | HTTP router                             |
| `go.uber.org/zap`                | Structured logging                       |
| `github.com/prometheus/client_golang` | Metrics                             |
| `github.com/google/uuid`         | Request IDs / idempotency keys           |

---

## 3. High-Level Architecture

```
                          +-----------------------+
                          |    API Server (chi)   |
                          |                       |
                          |  POST /v1/transfers   | <-- token + amount + recipient
                          |  GET  /v1/tx/:id      |
                          +-----------+-----------+
                                      |
                      write request   |   read status
                      to DB           |   from DB
                                      |
                          +-----------v-----------+
                          |      PostgreSQL       |
                          |                       |
                          |  transactions table   |
                          |  nonce_cursors table  |
                          +-----------+-----------+
                                      ^
                       poll for       |  update status,
                       pending txs    |  nonce, hash, receipt
                                      |
              +-----------------------+------------------------+
              |                       |                        |
    +---------v--------+  +----------v--------+  +------------v------+
    | Sender Pipeline  |  | Sender Pipeline   |  | Sender Pipeline   |
    | EOA_A @ chain 1  |  | EOA_A @ chain 137 |  | EOA_B @ chain 1   |
    |                  |  |                    |  |                   |
    | 1. Claim next tx |  | 1. Claim next tx  |  | 1. Claim next tx  |
    | 2. Assign nonce  |  | 2. Assign nonce   |  | 2. Assign nonce   |
    | 3. Estimate gas  |  | 3. Estimate gas   |  | 3. Estimate gas   |
    | 4. Sign & send   |  | 4. Sign & send    |  | 4. Sign & send    |
    | 5. Wait receipt  |  | 5. Wait receipt   |  | 5. Wait receipt   |
    | 6. Update DB     |  | 6. Update DB      |  | 6. Update DB      |
    +--------+---------+  +---------+----------+  +----------+--------+
             |                      |                        |
             v                      v                        v
    +--------+---------+  +---------+----------+  +----------+--------+
    | RPC Pool         |  | RPC Pool           |  | RPC Pool          |
    | chain 1          |  | chain 137          |  | chain 1           |
    | (with failover)  |  | (with failover)    |  | (with failover)   |
    +------------------+  +--------------------+  +-------------------+


    +------------------------------------------------------------------+
    |                     Background Workers                           |
    |                                                                  |
    |  [Receipt Poller]     - polls for receipts of sent txs           |
    |  [Stuck TX Detector]  - finds txs not mined within threshold     |
    |  [Gas Bumper]         - replaces stuck txs with higher gas       |
    |  [Reorg Watcher]      - detects chain reorgs, re-verifies txs   |
    |  [Nonce Reconciler]   - periodic on-chain nonce sync             |
    +------------------------------------------------------------------+
```

### Data Flow

1. Client calls `POST /v1/transfers` with `{token, amount, recipient, chain_id, sender, idempotency_key}`.
2. API server resolves `token` against the per-chain whitelist. For native tokens (`ETH`, `MATIC`,
   etc.), it sets `to=recipient, value=amount_in_wei, data=0x`. For whitelisted ERC20 tokens, it
   sets `to=token_contract, value=0, data=abi.encode("transfer(address,uint256)", recipient, amount_in_smallest_unit)`.
3. Validates amount against token decimals and per-token transfer limits.
4. Writes the resolved transaction to the `transactions` table with transfer metadata, returns
   the transaction ID (HTTP 202 Accepted).
5. The sender pipeline for `(sender, chain_id)` picks up the queued transaction (oldest first).
6. Pipeline assigns the next nonce, estimates gas, signs the transaction, and broadcasts via RPC.
7. Status transitions: `QUEUED -> PENDING -> SUBMITTED -> CONFIRMED / FAILED`.
8. Client polls `GET /v1/transactions/:id` to get current status, tx hash, and eventual receipt.

### Why This Shape

**Why not synchronous send-and-wait in the API handler?**
Because transaction confirmation takes 2-60+ seconds. Holding HTTP connections open that long is
wasteful, fragile (timeouts, load balancer limits), and makes the caller's error handling harder.
The async model is standard practice for transaction submission APIs (see Alchemy, Infura, any
custodial wallet API).

**Why per-sender-per-chain goroutines instead of a shared worker pool?**
Nonces are scoped to (address, chain). If two goroutines from a pool both try to send from EOA_A
on chain 1, they must coordinate on nonce assignment. This coordination requires either:
- A database lock (adds latency per transaction, potential deadlocks)
- An in-memory mutex (not safe across process restarts, complex recovery)
- A channel/pipeline (which is exactly what we are doing, just with extra indirection)

The dedicated pipeline eliminates the coordination problem entirely. The pipeline IS the lock. At
our scale (say 10 EOAs x 5 chains = 50 goroutines), this is negligible overhead.

---

## 4. Component Deep-Dives

### 4.1 API Server

Responsibilities:
- Request validation (address checksums, chain support, sender authorization)
- Idempotency enforcement via `idempotency_key`
- Token whitelist resolution and transfer-to-transaction conversion
- ERC20 calldata encoding (`transfer(address,uint256)`) and decimal/amount validation
- Writing `QUEUED` transactions to the database
- Serving transaction status queries
- Health checks and metrics endpoints

The API server is stateless. Multiple instances can run behind a load balancer, all writing to
the same Postgres database. However, **only one instance should run the sender pipelines** to
avoid nonce races. In a multi-instance deployment, you would use a leader election mechanism
(Postgres advisory locks or a distributed lock) to ensure only one instance runs pipelines.

For the initial single-instance deployment, the API server and pipeline runner live in the same
process.

### 4.2 Sender Pipelines

Each `(EOA, chain)` pair has a dedicated goroutine structured as:

```go
func (p *Pipeline) Run(ctx context.Context) {
    for {
        select {
        case <-ctx.Done():
            return
        default:
        }

        // 1. Claim the next QUEUED transaction for this (sender, chain)
        tx, err := p.store.ClaimNextQueued(ctx, p.sender, p.chainID)
        if err != nil { /* backoff and retry */ }
        if tx == nil {
            // No work. Wait for notification or poll interval.
            select {
            case <-ctx.Done():
                return
            case <-p.notify:    // signaled by API server on new insert
            case <-time.After(p.pollInterval):  // fallback poll
            }
            continue
        }

        // 2. Assign nonce
        nonce, err := p.assignNonce(ctx)

        // 3. Estimate gas and build transaction
        gasParams, err := p.estimateGas(ctx, tx)

        // 4. Sign transaction
        signedTx, err := p.sign(tx, nonce, gasParams)

        // 5. Broadcast
        txHash, err := p.broadcast(ctx, signedTx)

        // 6. Update DB with hash and status SUBMITTED
        p.store.MarkSubmitted(ctx, tx.ID, txHash, nonce, gasParams)
    }
}
```

**Notification mechanism:** When the API server inserts a new `QUEUED` transaction, it sends on
a channel to wake the relevant pipeline. This avoids constant polling. The `time.After` is a
fallback in case the notification is missed (e.g., process restart).

**What if step 3, 4, or 5 fails?** The transaction stays `PENDING` (claimed but not submitted).
The pipeline retries with backoff. If it fails N times, the transaction is marked `FAILED` with
an error reason, and the nonce is released. See section 10 (Retry & Recovery) for details.

### 4.3 RPC Pool

Each chain has an RPC pool that provides:
- **Multiple endpoints** for redundancy (primary + fallback RPCs)
- **Health checking** -- periodic `eth_blockNumber` calls, automatic failover on errors
- **Rate limit awareness** -- backoff when HTTP 429 is received
- **Request routing** -- read-heavy calls (gas estimation, nonce queries) can go to any healthy
  node; write calls (sendRawTransaction) are sent to all healthy nodes simultaneously for faster
  propagation

```go
type RPCPool struct {
    chainID   uint64
    endpoints []*Endpoint   // ordered by priority
    mu        sync.RWMutex
}

type Endpoint struct {
    url       string
    client    *ethclient.Client
    healthy   atomic.Bool
    latencyMA float64       // moving average latency
    errorRate float64       // recent error rate
}
```

**Broadcast strategy for sendRawTransaction:** Send to ALL healthy endpoints simultaneously.
Return success if ANY succeeds. This maximizes the chance of mempool inclusion and reduces
reliance on any single RPC provider. Most RPC providers deduplicate based on tx hash, so this is
safe.

### 4.4 Background Workers

#### Receipt Poller
- Polls `eth_getTransactionReceipt` for all transactions in `SUBMITTED` status.
- Batch queries: groups transactions by chain, uses batch JSON-RPC where supported.
- On receipt found: updates status to `CONFIRMED` (if `receipt.status == 1`) or `REVERTED`
  (if `receipt.status == 0`). Stores full receipt data.
- On no receipt after chain-specific timeout: flags transaction for gas bumping.

#### Stuck Transaction Detector
- Finds `SUBMITTED` transactions that have been pending longer than a chain-specific threshold
  (e.g., 3 minutes on Ethereum, 30 seconds on Polygon).
- Considers current base fee -- a transaction might just be waiting for base fee to drop.
- Marks eligible transactions for gas bumping.

#### Gas Bumper
- Takes stuck transactions and creates **replacement** transactions (same nonce, higher gas).
- EIP-1559 bumping: increase `maxPriorityFeePerGas` by at least 10% (required by most nodes to
  accept replacement), and increase `maxFeePerGas` proportionally.
- Legacy bumping: increase `gasPrice` by at least 10%.
- Records the replacement as a new `attempt` linked to the same logical transaction.
- Caps total gas spend per transaction to prevent runaway costs during gas spikes.

#### Reorg Watcher
- Tracks the latest confirmed block for each chain.
- When a reorg is detected (block hash at a previously-seen height changes), re-checks all
  transactions confirmed in the reorged range.
- If a transaction is no longer in the canonical chain, reverts its status to `SUBMITTED` (it may
  re-confirm in the new chain, or need re-submission).
- Reorg depth tracking: configurable per chain (e.g., wait for 12 confirmations on Ethereum
  mainnet, 64 on Polygon, 1 on Arbitrum).

#### Nonce Reconciler
- Periodic (every 60s per chain) comparison of our local nonce cursor vs on-chain
  `eth_getTransactionCount(address, "pending")`.
- If on-chain nonce > local cursor: something sent transactions outside our system. Log a
  critical alert and advance local cursor.
- If on-chain nonce < local cursor: there is a nonce gap. Identify the gap and either re-submit
  the missing transaction or send a zero-value self-transfer to fill the gap.
- This is a safety net, not the primary nonce management mechanism.

---

## 5. API Design

### Base URL: `/v1`

### `POST /v1/transfers`

Submit a token transfer (native or whitelisted ERC20). This is the sole transaction submission
endpoint. It handles calldata encoding, decimal conversion, and token contract resolution
internally, constructing a signed EVM transaction and feeding it into the sender pipeline.

**Request:**
```json
{
  "idempotency_key": "solver-settlement-xyz-1",
  "chain_id": 1,
  "sender": "0xSenderAddress",
  "recipient": "0xRecipientAddress",
  "token": "USDC",
  "amount": "1500.50",
  "priority": "high",
  "metadata": {
    "order_id": "xyz",
    "solver": "my-solver"
  }
}
```

| Field             | Type   | Required | Description                                                              |
|-------------------|--------|----------|--------------------------------------------------------------------------|
| `idempotency_key` | string | Yes      | Client-provided unique key. Same key = same result.                      |
| `chain_id`        | uint64 | Yes      | Target EVM chain ID.                                                     |
| `sender`          | string | Yes      | EOA address to send from. Must be registered.                            |
| `recipient`       | string | Yes      | Recipient address (0x-prefixed, checksummed). Must not be zero address.  |
| `token`           | string | Yes      | Token symbol: `"ETH"`, `"MATIC"`, `"USDC"`, `"USDT"`, etc. Resolved against the per-chain whitelist. The native token symbol for the chain (e.g., `"ETH"` on Ethereum, `"MATIC"` on Polygon) triggers a native value transfer. Any other symbol must match a whitelisted ERC20 on that chain. |
| `amount`          | string | Yes      | Human-readable amount as a decimal string (e.g., `"1500.50"`, `"0.001"`). Converted to the token's smallest unit using the configured decimals. Must be positive. Must not exceed the per-token max transfer limit. |
| `priority`        | string | No       | `"low"`, `"normal"`, `"high"`, `"urgent"`. Default `"normal"`.           |
| `metadata`        | object | No       | Opaque JSON. Stored, never interpreted.                                  |

**Response (202 Accepted):**
```json
{
  "id": "01HQXK5...",
  "status": "QUEUED",
  "chain_id": 1,
  "sender": "0xSenderAddress",
  "recipient": "0xRecipientAddress",
  "token": "USDC",
  "amount": "1500.50",
  "transfer_type": "erc20",
  "token_contract": "0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48",
  "created_at": "2026-03-01T12:00:00Z"
}
```

**Internal conversion logic:**

```go
func (h *TransferHandler) resolveTransfer(req TransferRequest) (*TxParams, error) {
    chain := h.config.Chains[req.ChainID]
    if chain == nil {
        return nil, ErrInvalidChain
    }

    // Check if this is a native token transfer
    if req.Token == chain.NativeTokenSymbol {
        // Native transfer: to=recipient, value=amount_in_wei, data=empty
        amountWei, err := parseAmountToSmallestUnit(req.Amount, chain.NativeTokenDecimals)
        if err != nil {
            return nil, fmt.Errorf("invalid amount: %w", err)
        }
        if amountWei.Sign() <= 0 {
            return nil, ErrZeroAmount
        }
        if chain.NativeMaxTransfer != nil && amountWei.Cmp(chain.NativeMaxTransfer) > 0 {
            return nil, ErrExceedsTransferLimit
        }
        return &TxParams{
            To:    req.Recipient,
            Value: amountWei,
            Data:  nil, // empty calldata = simple transfer
        }, nil
    }

    // ERC20 transfer: look up token in whitelist
    token, ok := chain.TokenWhitelist[req.Token]
    if !ok {
        return nil, ErrTokenNotWhitelisted
    }
    if !token.Enabled {
        return nil, ErrTokenDisabled
    }

    amountSmallest, err := parseAmountToSmallestUnit(req.Amount, token.Decimals)
    if err != nil {
        return nil, fmt.Errorf("invalid amount for %s: %w", req.Token, err)
    }
    if amountSmallest.Sign() <= 0 {
        return nil, ErrZeroAmount
    }
    if token.MaxTransfer != nil && amountSmallest.Cmp(token.MaxTransfer) > 0 {
        return nil, ErrExceedsTransferLimit
    }

    // Encode ERC20 transfer(address,uint256) calldata
    transferFnSig := crypto.Keccak256([]byte("transfer(address,uint256)"))[:4]
    data := append(transferFnSig,
        common.LeftPadBytes(common.HexToAddress(req.Recipient).Bytes(), 32)...)
    data = append(data, common.LeftPadBytes(amountSmallest.Bytes(), 32)...)

    return &TxParams{
        To:    token.ContractAddress, // send to the ERC20 contract, NOT the recipient
        Value: big.NewInt(0),         // no native value for ERC20 transfers
        Data:  data,
    }, nil
}
```

**Why `parseAmountToSmallestUnit` uses string arithmetic, not float:**

Token amounts are decimal strings (e.g., `"1500.50"` USDC). Converting to the smallest unit
(1500.50 * 10^6 = 1500500000 for USDC's 6 decimals) MUST use fixed-point string parsing, never
`float64`. Floating-point arithmetic introduces rounding errors that can cause off-by-one in the
smallest unit. The implementation splits on the decimal point, pads/truncates the fractional part
to `decimals` digits, and constructs a `*big.Int` from the concatenated string.

```go
// parseAmountToSmallestUnit converts "1500.50" with decimals=6 to big.Int 1500500000.
// Returns error if amount has more decimal places than the token supports.
func parseAmountToSmallestUnit(amount string, decimals uint8) (*big.Int, error) {
    parts := strings.SplitN(amount, ".", 2)
    intPart := parts[0]
    fracPart := ""
    if len(parts) == 2 {
        fracPart = parts[1]
    }

    // Reject if fractional part has more digits than token decimals
    if len(fracPart) > int(decimals) {
        return nil, fmt.Errorf("amount has %d decimal places, token supports %d", len(fracPart), decimals)
    }

    // Pad fractional part to exactly `decimals` digits
    fracPart = fracPart + strings.Repeat("0", int(decimals)-len(fracPart))

    // Combine: "1500" + "500000" = "1500500000"
    combined := intPart + fracPart
    result, ok := new(big.Int).SetString(combined, 10)
    if !ok {
        return nil, fmt.Errorf("invalid numeric amount: %s", amount)
    }
    return result, nil
}
```

**Error responses (in addition to standard transaction errors):**

| Status | Condition                                                  |
|--------|------------------------------------------------------------|
| 400    | Invalid amount format, negative amount, too many decimals  |
| 400    | Recipient is zero address (`0x0000...0000`)                |
| 400    | Invalid recipient address                                  |
| 403    | Token not whitelisted on this chain                        |
| 403    | Token is disabled (whitelisted but temporarily suspended)  |
| 422    | Amount exceeds per-token transfer limit                    |
| 422    | Amount is zero                                             |

**Note on allowances:** The transfer endpoint only supports `transfer(address,uint256)`, which
transfers tokens FROM the sender EOA. This requires the sender EOA to hold the ERC20 balance
directly. It does NOT use `transferFrom`, which would require prior approval. Since our sender
EOAs are the direct holders of tokens, `transfer` is the correct function. Delegated transfers
(`transferFrom`) are intentionally not supported. If they are needed in the future, it would
require an explicit new endpoint with its own validation and security controls.

### `GET /v1/transactions/:id`

Get the current state of a transaction.

**Response (200 OK):**
```json
{
  "id": "01HQXK5...",
  "idempotency_key": "solver-order-abc123-attempt-1",
  "status": "CONFIRMED",
  "chain_id": 1,
  "sender": "0xSenderAddress",
  "to": "0xRecipientAddress",
  "value": "1000000000000000000",
  "data": "0xabcdef...",
  "nonce": 42,
  "tx_hash": "0xabc123...",
  "gas_used": 21000,
  "effective_gas_price": "30000000000",
  "block_number": 19000000,
  "block_hash": "0xdef456...",
  "confirmations": 12,
  "receipt": { /* full receipt JSON */ },
  "attempts": [
    {
      "attempt_number": 1,
      "tx_hash": "0xfirst...",
      "max_fee_per_gas": "30000000000",
      "max_priority_fee_per_gas": "2000000000",
      "submitted_at": "2026-03-01T12:00:01Z",
      "status": "REPLACED"
    },
    {
      "attempt_number": 2,
      "tx_hash": "0xabc123...",
      "max_fee_per_gas": "45000000000",
      "max_priority_fee_per_gas": "3000000000",
      "submitted_at": "2026-03-01T12:00:45Z",
      "status": "CONFIRMED"
    }
  ],
  "error": null,
  "metadata": { "order_id": "abc123" },
  "created_at": "2026-03-01T12:00:00Z",
  "updated_at": "2026-03-01T12:01:15Z"
}
```

### `GET /v1/transactions?chain_id=1&sender=0x...&status=SUBMITTED&limit=50`

List transactions with filters. Pagination via cursor (`?after=<id>`).

### `GET /v1/health`

```json
{
  "status": "healthy",
  "chains": {
    "1": { "healthy": true, "block_number": 19000000, "rpc_endpoints": 3, "healthy_endpoints": 2 },
    "137": { "healthy": true, "block_number": 55000000, "rpc_endpoints": 2, "healthy_endpoints": 2 }
  },
  "pipelines": {
    "0xA-1": { "status": "running", "queue_depth": 5 },
    "0xA-137": { "status": "running", "queue_depth": 0 }
  },
  "db": "connected"
}
```

### `GET /v1/metrics`

Prometheus-format metrics endpoint. See section 13.

---

## 6. Token Whitelist & Transfer Abstraction

This section describes how the system supports both native token transfers and whitelisted ERC20
token transfers through a safe, validated abstraction layer.

### Design Philosophy

This service is purpose-built for token transfers. It intentionally does NOT support arbitrary
contract calls. A general-purpose transaction sender that accepts raw calldata is a security
liability in solver infrastructure handling real money -- it allows any caller to interact with
any contract, which opens the door to fund theft, malicious contract calls, and unintended
state changes. By restricting the API surface to token transfers only, we:

1. Restrict ERC20 interactions to whitelisted contracts only.
2. Handle decimal-to-smallest-unit conversion (no risk of callers sending 10^18 too much or
   too little).
3. Enforce per-token transfer limits as a safety net.
4. Provide a clean audit trail of token movements with human-readable amounts.
5. Eliminate the possibility of callers accidentally encoding calldata wrong.
6. Make it impossible for a compromised or buggy caller to trigger arbitrary contract calls.

This constraint is a deliberate security feature. If arbitrary contract interactions are ever
needed (contract deployments, DEX swaps, governance calls), they would require a separate
service with its own security controls, access policies, and audit trail.

### Token Whitelist Model

Each chain has a list of whitelisted ERC20 tokens, plus a native token configuration. Only tokens
on the whitelist can be transferred through the `/v1/transfers` endpoint.

```go
type TokenConfig struct {
    Symbol          string         // "USDC", "USDT", "WETH", etc.
    ContractAddress common.Address // the ERC20 contract address on this chain
    Decimals        uint8          // token decimals (6 for USDC, 18 for WETH, etc.)
    MaxTransfer     *big.Int       // max amount in smallest unit per single transfer (nil = no limit)
    Enabled         bool           // can be disabled without removing from config (e.g., during incidents)
}

// Embedded in ChainConfig:
type ChainConfig struct {
    // ... existing fields ...

    // Native token configuration
    NativeTokenSymbol   string   // "ETH", "MATIC", "AVAX", "BNB", etc.
    NativeTokenDecimals uint8    // always 18 for EVM native tokens, but explicit for safety
    NativeMaxTransfer   *big.Int // max native transfer per tx in wei (nil = no limit)

    // ERC20 whitelist: keyed by uppercase symbol
    TokenWhitelist map[string]TokenConfig
}
```

### Whitelist Configuration (YAML / Environment)

```yaml
chains:
  1:  # Ethereum Mainnet
    name: "ethereum"
    native_token_symbol: "ETH"
    native_token_decimals: 18
    native_max_transfer: "100000000000000000000"  # 100 ETH in wei
    token_whitelist:
      USDC:
        contract_address: "0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48"
        decimals: 6
        max_transfer: "10000000000"  # 10,000 USDC in smallest unit (10000 * 10^6)
        enabled: true
      USDT:
        contract_address: "0xdAC17F958D2ee523a2206206994597C13D831ec7"
        decimals: 6
        max_transfer: "10000000000"  # 10,000 USDT
        enabled: true
      WETH:
        contract_address: "0xC02aaA39b223FE8D0A0e5C4F27eAD9083C756Cc2"
        decimals: 18
        max_transfer: "100000000000000000000"  # 100 WETH
        enabled: true
      DAI:
        contract_address: "0x6B175474E89094C44Da98b954EedeAC495271d0F"
        decimals: 18
        max_transfer: "10000000000000000000000"  # 10,000 DAI
        enabled: true

  137:  # Polygon
    name: "polygon"
    native_token_symbol: "MATIC"
    native_token_decimals: 18
    native_max_transfer: "1000000000000000000000"  # 1000 MATIC
    token_whitelist:
      USDC:
        contract_address: "0x3c499c542cEF5E3811e1192ce70d8cC03d5c3359"  # native USDC on Polygon
        decimals: 6
        max_transfer: "10000000000"
        enabled: true
      USDT:
        contract_address: "0xc2132D05D31c914a87C6611C10748AEb04B58e8F"
        decimals: 6
        max_transfer: "10000000000"
        enabled: true
```

**Environment variable override format:**

```bash
# Whitelist tokens via env vars (supplements or overrides config file)
TX_SENDER_CHAIN_1_TOKEN_USDC_ADDRESS="0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48"
TX_SENDER_CHAIN_1_TOKEN_USDC_DECIMALS="6"
TX_SENDER_CHAIN_1_TOKEN_USDC_MAX_TRANSFER="10000000000"
TX_SENDER_CHAIN_1_TOKEN_USDC_ENABLED="true"

TX_SENDER_CHAIN_1_NATIVE_SYMBOL="ETH"
TX_SENDER_CHAIN_1_NATIVE_DECIMALS="18"
TX_SENDER_CHAIN_1_NATIVE_MAX_TRANSFER="100000000000000000000"
```

### Startup Validation

On startup, the token whitelist is validated:

1. **Contract address format:** Every whitelisted token must have a valid 0x-prefixed 20-byte
   address. Reject misconfigured entries.
2. **No duplicate addresses:** The same contract address must not appear under two different
   symbols on the same chain (prevents confusion and double-counting in metrics).
3. **No duplicate symbols:** Each symbol is unique per chain. The native token symbol must not
   conflict with any ERC20 symbol.
4. **Decimals sanity check:** ERC20 decimals are typically 0-18. Warn (but do not reject) if
   decimals > 18, as some tokens use unusual values.
5. **Contract code verification (optional, recommended):** On startup, call `eth_getCode` for
   each whitelisted contract address. If the address has no code (is an EOA), log a CRITICAL
   error and disable that token. This catches misconfigured addresses before any transfers are
   attempted.
6. **Max transfer sanity:** If `max_transfer` is set, verify it is positive and fits in uint256.

### Transfer Resolution Flow

```
Client: POST /v1/transfers { token: "USDC", amount: "1500.50", recipient: "0xR...", chain_id: 1 }
                |
                v
     +---------------------+
     | Input Validation     |
     | - valid addresses    |
     | - valid chain_id     |
     | - known sender       |
     | - recipient != 0x0   |
     +----------+----------+
                |
                v
     +---------------------+
     | Token Resolution     |
     | - Is "USDC" the      |
     |   native symbol?     |
     |   NO -> ERC20 path   |
     +----------+----------+
                |
                v
     +---------------------+
     | Whitelist Lookup      |
     | - USDC found in       |
     |   chain 1 whitelist?  |
     |   YES                 |
     | - Enabled? YES        |
     +----------+----------+
                |
                v
     +---------------------+
     | Amount Conversion     |
     | "1500.50" * 10^6     |
     | = 1500500000         |
     | (string arithmetic)  |
     +----------+----------+
                |
                v
     +---------------------+
     | Limit Check           |
     | 1500500000 <= max?    |
     | YES                   |
     +----------+----------+
                |
                v
     +---------------------+
     | Build Transaction     |
     | to: 0xA0b8...eB48    |
     | value: 0             |
     | data: 0xa9059cbb     |  (transfer selector)
     |   + recipient padded |
     |   + amount padded    |
     +----------+----------+
                |
                v
     +---------------------+
     | Write to DB           |
     | (transactions table  |
     |  + transfer metadata)|
     | Status: QUEUED        |
     +----------+----------+
                |
                v
     Into sender pipeline (nonce, sign, broadcast)
```

### Native Token Transfer Details

When `token` matches the chain's `NativeTokenSymbol`:

- **Gas limit:** Set to 21000 (the fixed cost of a simple ETH transfer). No need to call
  `eth_estimateGas` because the cost is deterministic for transfers to EOAs. If the recipient
  is a contract (which could have a `receive()` or `fallback()` function), gas estimation IS
  called because execution cost is unpredictable. The system detects this by checking
  `eth_getCode(recipient)` -- if code length > 0, estimate gas; otherwise use 21000.
- **Value:** The human-readable amount converted to wei (18 decimals for all EVM native tokens).
- **Data:** Empty (`0x`).
- **Gas cost note:** The sender must hold enough native token to cover both the transfer `value`
  AND the gas cost. The system does not deduct gas cost from the transfer amount.

### ERC20 Transfer Calldata Encoding

The ERC20 `transfer` function has the following ABI:

```
transfer(address recipient, uint256 amount)
```

Function selector: `0xa9059cbb` (first 4 bytes of `keccak256("transfer(address,uint256)")`).

The calldata is exactly 68 bytes:
```
0xa9059cbb                                                           (4 bytes: function selector)
000000000000000000000000<recipient_address_20_bytes>                  (32 bytes: address, left-padded)
<amount_uint256>                                                     (32 bytes: uint256, left-padded)
```

This is encoded using `go-ethereum`'s ABI encoder for correctness, not manual byte concatenation:

```go
import "github.com/ethereum/go-ethereum/accounts/abi"

var erc20ABI, _ = abi.JSON(strings.NewReader(`[{
    "name": "transfer",
    "type": "function",
    "inputs": [
        {"name": "recipient", "type": "address"},
        {"name": "amount", "type": "uint256"}
    ],
    "outputs": [{"name": "", "type": "bool"}]
}]`))

func encodeERC20Transfer(recipient common.Address, amount *big.Int) ([]byte, error) {
    return erc20ABI.Pack("transfer", recipient, amount)
}
```

### Why a Whitelist Instead of Allowing Any ERC20

1. **Security:** Without a whitelist, a caller could set `token` to any contract address. A
   malicious or buggy caller could trigger calls to arbitrary contracts. The whitelist ensures
   only vetted, known-good ERC20 contracts are callable through this path.
2. **Correctness:** Not all contracts that implement `transfer(address,uint256)` are ERC20
   tokens. Some contracts use the same function signature for entirely different purposes. The
   whitelist ensures we only interact with verified ERC20 implementations.
3. **Decimal safety:** Different tokens have different decimals. By registering decimals in the
   whitelist, we prevent callers from needing to know (and potentially getting wrong) the decimal
   count. `"1500.50" USDC` always means 1500.50 USDC, regardless of whether the caller knows it
   is 6 decimals.
4. **Transfer limits:** The whitelist enables per-token transfer limits. This is a critical safety
   net: a bug in the calling service might try to transfer 10 billion USDC instead of 10 USDC.
   The limit catches this before the transaction is submitted.
5. **Auditability:** The whitelist provides a clear manifest of which tokens the system can move.
   This is essential for security audits and operational reviews.

---

## 7. Nonce Management

This is the most critical section of the entire design. Getting nonces wrong means stuck
transactions, double-spends, or lost funds.

### The Nonce Problem

Every EVM transaction from an EOA must include a nonce -- a sequential counter starting from 0.
The rules:
- Transaction with nonce N can only be mined after nonce N-1 is mined.
- If nonce N is in the mempool and you submit another transaction with nonce N, it is a
  **replacement** (only accepted if gas price is sufficiently higher).
- A gap in nonces (e.g., N is mined but N+1 was never submitted) **blocks all subsequent
  transactions** from that EOA on that chain.
- `eth_getTransactionCount(address, "pending")` returns the next expected nonce including
  pending mempool transactions -- but this is unreliable across different RPC providers and
  under high concurrency.

### Strategy: Local Nonce Cursor with On-Chain Reconciliation

We maintain a **local nonce cursor** in the database for each `(sender, chain_id)` pair. This
cursor represents the next nonce to assign. The pipeline goroutine is the ONLY writer to this
cursor, eliminating races.

```
                    Database: nonce_cursors
                    +---------------------------+
                    | sender    | chain_id | next_nonce |
                    | 0xA       | 1        | 42         |
                    | 0xA       | 137      | 100        |
                    | 0xB       | 1        | 7          |
                    +---------------------------+

    Pipeline (0xA, chain 1):           On-chain state:
    ========================           ================
    next_nonce = 42                    pending count = 42

    1. Claim tx, assign nonce 42       sends tx with nonce 42
    2. Increment cursor to 43          pending count = 43 (tx in mempool)
    3. Claim tx, assign nonce 43       sends tx with nonce 43
    4. Increment cursor to 44          pending count = 44
    ...
```

### Initialization

On startup, for each `(sender, chain_id)`:
1. Read `next_nonce` from the database.
2. Query `eth_getTransactionCount(sender, "pending")` from the chain.
3. Query `eth_getTransactionCount(sender, "latest")` from the chain.
4. Take `max(db_nonce, pending_count)` as the starting nonce.
5. If `pending_count > latest_count`, there are in-flight transactions. Scan our DB for
   `SUBMITTED` transactions in the range `[latest_count, pending_count)` and ensure they are
   tracked.
6. If `db_nonce > pending_count`, transactions may have been evicted from the mempool. Check
   each nonce in the gap and re-submit or fill.

This handles:
- Clean restarts (DB and chain agree)
- Restarts after crash (DB may be behind chain if tx was sent but DB update failed)
- Restarts after mempool eviction (chain is behind DB)

### Nonce Assignment Within the Pipeline

```go
func (p *Pipeline) assignNonce(ctx context.Context) (uint64, error) {
    // Single-writer: only this goroutine touches this cursor.
    // Use a DB transaction to atomically read-increment.
    nonce, err := p.store.IncrementNonceCursor(ctx, p.sender, p.chainID)
    if err != nil {
        return 0, fmt.Errorf("nonce assignment failed: %w", err)
    }
    return nonce, nil
}
```

```sql
-- IncrementNonceCursor (atomic read-and-increment)
UPDATE nonce_cursors
SET next_nonce = next_nonce + 1, updated_at = NOW()
WHERE sender = $1 AND chain_id = $2
RETURNING next_nonce - 1 AS assigned_nonce;
```

Because only one goroutine per `(sender, chain_id)` calls this, there is no contention. The
database transaction ensures durability -- if the process crashes after incrementing but before
sending, the nonce reconciler will detect and fill the gap.

### Handling Nonce Gaps

A nonce gap occurs when nonce N was assigned but the transaction was never mined (mempool
eviction, RPC failure during broadcast, etc.). All transactions with nonce > N are now stuck.

**Detection:**
- The receipt poller notices that transactions with nonces > N are confirmed but nonce N is not.
- The nonce reconciler detects `on_chain_pending_nonce < our_cursor`.
- The stuck transaction detector sees a transaction pending beyond the threshold.

**Resolution:**
1. Find the original transaction for the gap nonce in our DB.
2. If it exists and has a tx_hash, re-broadcast it (it may have been evicted from the mempool
   but is still valid).
3. If re-broadcast fails (e.g., nonce already used somehow), submit a zero-value self-transfer
   at that nonce to fill the gap.
4. If the original transaction was never signed (failure before broadcast), re-process it from
   the pipeline.

### Handling Replacement Transactions (Gas Bumping)

When bumping gas on a stuck transaction:
- Use the **same nonce** as the original.
- Increase gas by at least 10% (the minimum most nodes require to accept a replacement).
- Store both the original and replacement as separate `attempts` linked to the same logical
  transaction.
- Only ONE attempt per nonce can be mined. The receipt poller handles this: when any attempt is
  confirmed, mark the transaction as confirmed and other attempts as `REPLACED`.

### Why Not Just Use `eth_getTransactionCount("pending")` Each Time?

Because it is a consensus-breaking single point of failure:
1. **Stale data:** Different RPC nodes may return different pending counts due to mempool
   propagation delays.
2. **Race conditions:** Two concurrent calls might both get nonce N, leading to a collision.
3. **Unreliable "pending" state:** Some nodes do not implement the pending filter correctly.
   Some return "latest" for "pending". Behavior varies by client (Geth, Erigon, Besu).
4. **Latency:** An RPC call per transaction adds latency. Our local cursor is a DB read.

We use `eth_getTransactionCount` ONLY for:
- Initialization on startup
- Periodic reconciliation (safety net)
- Investigating anomalies

---

## 8. Gas Strategy

### EIP-1559 (Type 2) by Default

All supported chains should use EIP-1559 transactions where available. For chains that do not
support EIP-1559, fall back to legacy (Type 0) transactions. The system auto-detects support
by checking if the chain returns `baseFeePerGas` in block headers.

### Fee Estimation

```
                    +-----------------------+
                    |   Gas Strategy Engine  |
                    +-----------------------+
                    |                       |
                    |  1. Get latest block   |
                    |     header (baseFee)   |
                    |                       |
                    |  2. Get fee history    |
                    |     (eth_feeHistory)   |
                    |                       |
                    |  3. Calculate target   |
                    |     fees by priority   |
                    |                       |
                    |  4. Apply chain-       |
                    |     specific bounds    |
                    +-----------------------+
```

#### EIP-1559 Fee Calculation

```go
type GasParams struct {
    GasLimit             uint64
    MaxFeePerGas         *big.Int  // cap on total fee per gas
    MaxPriorityFeePerGas *big.Int  // tip to validator
    // Legacy fields (for non-1559 chains)
    GasPrice             *big.Int
    Type                 int       // 0 = legacy, 2 = EIP-1559
}

func (e *GasEngine) Estimate(ctx context.Context, chain ChainConfig, tx TxRequest, priority Priority) (GasParams, error) {
    // 1. Gas limit: use provided value, or eth_estimateGas + buffer
    gasLimit := tx.GasLimit
    if gasLimit == 0 {
        estimated, err := e.rpc.EstimateGas(ctx, tx.ToCallMsg())
        if err != nil {
            return GasParams{}, fmt.Errorf("gas estimation failed: %w", err)
        }
        gasLimit = uint64(float64(estimated) * chain.GasLimitMultiplier) // e.g., 1.2x buffer
    }

    // 2. Base fee: from latest block
    baseFee := e.rpc.LatestBaseFee(ctx)

    // 3. Priority fee: from fee history, percentile based on priority
    priorityFee := e.calculatePriorityFee(ctx, priority)

    // 4. Max fee: baseFee * multiplier + priorityFee
    //    The multiplier accounts for base fee increase over the next few blocks.
    //    EIP-1559 base fee can increase at most 12.5% per block.
    //    For 6 blocks of headroom: baseFee * 1.125^6 ~ baseFee * 2.0
    maxFee := new(big.Int).Mul(baseFee, big.NewInt(2))
    maxFee.Add(maxFee, priorityFee)

    // 5. Apply bounds
    maxFee = clamp(maxFee, chain.MinMaxFee, chain.MaxMaxFee)
    priorityFee = clamp(priorityFee, chain.MinPriorityFee, chain.MaxPriorityFee)

    return GasParams{
        GasLimit:             gasLimit,
        MaxFeePerGas:         maxFee,
        MaxPriorityFeePerGas: priorityFee,
        Type:                 2,
    }, nil
}
```

#### Priority Tiers

| Priority | Priority Fee Percentile | Max Fee Multiplier | Use Case              |
|----------|------------------------|--------------------|-----------------------|
| low      | 25th percentile        | 1.5x baseFee      | Non-urgent, save gas  |
| normal   | 50th percentile        | 2.0x baseFee      | Default               |
| high     | 75th percentile        | 2.5x baseFee      | Time-sensitive solves  |
| urgent   | 95th percentile        | 3.0x baseFee      | Must include next block|

#### Gas Limit Estimation

- Call `eth_estimateGas` with the exact calldata.
- Apply a safety multiplier (default 1.2x, configurable per chain).
- The multiplier accounts for state changes between estimation and mining that could increase gas
  usage.
- Cap at a chain-specific maximum (e.g., block gas limit / 2).
- For known simple operations (ETH transfers = 21000), use constants instead of estimation.

#### Gas Bumping Strategy

When a transaction is stuck (see section 10):

```
Attempt 1: priorityFee = P,     maxFee = M
Attempt 2: priorityFee = P*1.15, maxFee = M*1.15   (15% bump)
Attempt 3: priorityFee = P*1.30, maxFee = M*1.30   (another 15% bump from original)
Attempt 4: priorityFee = P*1.50, maxFee = M*1.50
Attempt 5: priorityFee = P*2.00, maxFee = M*2.00   (double original)
```

After 5 bumps, alert the operator and stop automatic bumping. The cost of continuing to bump
may exceed the value of the transaction.

**Cost cap:** Each transaction has a maximum total gas cost (configurable, default: gas_limit *
original_max_fee * 3). If a bump would exceed this cap, do not bump and alert instead.

#### Caching and Fee Freshness

- Cache `eth_feeHistory` results for 1 block (~12s on Ethereum, ~2s on Polygon).
- Cache `baseFeePerGas` from the latest block header.
- Subscribe to new block headers via WebSocket where available to keep fee data fresh.
- On cache miss, fall back to a synchronous RPC call.

---

## 9. Transaction Lifecycle & State Machine

```
                                +--------+
                                | QUEUED |  (request accepted, waiting for pipeline)
                                +---+----+
                                    |
                        pipeline claims tx
                                    |
                                +---v----+
                                | PENDING|  (claimed by pipeline, nonce being assigned)
                                +---+----+
                                    |
                          nonce assigned, tx signed & broadcast
                                    |
                                +---v------+
                                | SUBMITTED|  (broadcast to network, waiting for receipt)
                                +---+------+
                                    |
                    +---------------+------------------+
                    |               |                  |
                +---v-----+   +----v----+       +-----v------+
                |CONFIRMED|   |REVERTED |       | FAILED     |
                |(on-chain|   |(on-chain|       | (never     |
                | success)|   | revert) |       |  mined or  |
                +---------+   +---------+       |  broadcast |
                                                |  failure)  |
                                                +------------+

    Additional sub-states tracked via the `attempts` table:
    - Each attempt can be: PENDING_SEND, BROADCAST, REPLACED, CONFIRMED, DROPPED
```

### Status Definitions

| Status      | Meaning                                                    | Terminal? |
|-------------|------------------------------------------------------------|-----------|
| `QUEUED`    | Request accepted, waiting in queue for pipeline pickup.     | No        |
| `PENDING`   | Pipeline has claimed this tx. Nonce assignment in progress. | No        |
| `SUBMITTED` | Signed tx broadcast to network. Awaiting receipt.          | No        |
| `CONFIRMED` | Transaction mined successfully (`receipt.status == 1`).     | Yes       |
| `REVERTED`  | Transaction mined but reverted (`receipt.status == 0`).     | Yes       |
| `FAILED`    | Permanently failed (unrecoverable error, max retries).      | Yes       |

### State Transition Rules

1. `QUEUED -> PENDING`: Only by the owning pipeline goroutine.
2. `PENDING -> SUBMITTED`: After successful broadcast of at least one attempt.
3. `PENDING -> QUEUED`: If pipeline fails before broadcast and releases the tx (retry).
4. `PENDING -> FAILED`: If nonce assignment or signing fails irrecoverably.
5. `SUBMITTED -> CONFIRMED`: Receipt received with `status == 1`.
6. `SUBMITTED -> REVERTED`: Receipt received with `status == 0`.
7. `SUBMITTED -> FAILED`: Max gas bumps exceeded, operator-initiated cancellation, or
   unrecoverable broadcast failure.
8. `CONFIRMED -> SUBMITTED`: Reorg detected, transaction no longer in canonical chain.

All state transitions are logged with timestamps, actor (pipeline, receipt-poller, etc.), and
reason. Backward transitions (CONFIRMED -> SUBMITTED) are critical events that trigger alerts.

---

## 10. Retry & Recovery

### Failure Taxonomy and Retry Policy

| Failure                           | Retryable? | Strategy                                |
|-----------------------------------|------------|-----------------------------------------|
| RPC connection timeout            | Yes        | Retry with next RPC endpoint            |
| RPC HTTP 429 (rate limited)       | Yes        | Exponential backoff, then next endpoint |
| RPC returns error: nonce too low  | Depends    | Re-sync nonce, retry if nonce was stale |
| RPC returns error: nonce too high | No         | Bug in our nonce management. Alert.     |
| RPC returns error: insufficient funds | No     | Mark FAILED. Alert operator.            |
| RPC returns error: gas too low    | Yes        | Re-estimate gas, retry                  |
| RPC returns error: replacement underpriced | Yes | Bump gas by 15%, retry              |
| eth_estimateGas reverts           | No         | Mark FAILED. The calldata itself fails. |
| Signing error                     | No         | Mark FAILED. Key issue. Alert.          |
| DB connection failure             | Yes        | Retry with backoff. Pipeline pauses.    |
| Transaction in mempool too long   | Yes        | Gas bump (replacement tx)               |
| Transaction evicted from mempool  | Yes        | Re-broadcast or re-send with same nonce |
| Chain reorg reverses confirmation | Yes        | Revert status, wait for re-confirmation |

### Retry Configuration

```go
type RetryConfig struct {
    MaxBroadcastAttempts int           // max times to retry initial broadcast (default: 5)
    MaxGasBumps          int           // max replacement transactions (default: 5)
    InitialBackoff       time.Duration // first retry delay (default: 1s)
    MaxBackoff           time.Duration // backoff cap (default: 30s)
    BackoffMultiplier    float64       // exponential factor (default: 2.0)
    StuckThreshold       time.Duration // when to consider tx stuck (chain-specific)
    GasBumpInterval      time.Duration // min time between gas bumps (default: 30s)
}
```

### Idempotency

Idempotency is enforced at two levels:

1. **API level:** The `idempotency_key` in the request is stored as a unique index. Duplicate
   submissions return the existing transaction state. The key is scoped globally (not per-sender
   or per-chain) because the client is responsible for generating meaningful keys.

2. **Pipeline level:** The pipeline claims transactions atomically with a `SELECT ... FOR UPDATE
   SKIP LOCKED` query. This ensures exactly one pipeline processes each transaction, even if
   there were somehow multiple pipeline instances (which should not happen, but defense in
   depth).

**Idempotency key conflict detection:** If a request arrives with a previously-seen
`idempotency_key` but different parameters (different `to`, `value`, etc.), the API returns
409 Conflict. This prevents silent parameter mutation bugs in the caller.

### Recovery After Crash

On process restart:

1. **Transactions in PENDING status:** These were claimed by a pipeline but not yet submitted.
   Reset them to `QUEUED` so the new pipeline instance picks them up. This is safe because no
   nonce was consumed on-chain yet (the DB increment happened but the tx was never broadcast).
   However, the nonce cursor WAS incremented. The nonce reconciler will detect this gap and
   either the re-processed transaction will fill it (likely, since it gets the next nonce), or
   the reconciler will fill the gap.

   Actually -- we should be more careful. If the cursor was incremented but the transaction was
   never broadcast, we have a nonce gap. The correct recovery is:

   a. Reset PENDING transactions to QUEUED.
   b. On startup nonce reconciliation, compare our cursor to on-chain pending count.
   c. If cursor > on-chain count, we have un-broadcast nonces. Roll back the cursor to match
      on-chain. The QUEUED transactions will be assigned fresh nonces.

2. **Transactions in SUBMITTED status:** These were broadcast. Check on-chain for their
   receipts. They might be confirmed, reverted, or still pending. The receipt poller handles
   this naturally.

3. **Transactions in QUEUED status:** No action needed. Pipelines will pick them up.

---

## 11. Persistence & Schema

### Database Schema

```sql
-- Tracks the next nonce to assign for each (sender, chain) pair.
-- Only written by the pipeline goroutine for that pair.
CREATE TABLE nonce_cursors (
    sender      CHAR(42) NOT NULL,        -- 0x-prefixed, lowercase
    chain_id    BIGINT NOT NULL,
    next_nonce  BIGINT NOT NULL DEFAULT 0,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (sender, chain_id)
);

-- Logical transactions. One row per client request.
CREATE TABLE transactions (
    id               TEXT PRIMARY KEY,          -- ULID for time-sortability
    idempotency_key  TEXT NOT NULL UNIQUE,
    chain_id         BIGINT NOT NULL,
    sender           CHAR(42) NOT NULL,         -- 0x-prefixed, lowercase
    to_address       CHAR(42) NOT NULL,         -- 0x-prefixed, lowercase
    value            NUMERIC(78, 0) NOT NULL DEFAULT 0,  -- wei, uint256 max is 78 digits
    data             BYTEA,                     -- calldata
    gas_limit        BIGINT,                    -- may be NULL until estimated
    nonce            BIGINT,                    -- assigned nonce, NULL until assigned
    priority         TEXT NOT NULL DEFAULT 'normal',

    -- Status tracking
    status           TEXT NOT NULL DEFAULT 'QUEUED',
    error_reason     TEXT,
    error_code       TEXT,

    -- Result data (filled on confirmation/revert)
    final_tx_hash    CHAR(66),                  -- hash of the mined attempt
    block_number     BIGINT,
    block_hash       CHAR(66),
    gas_used         BIGINT,
    effective_gas_price NUMERIC(78, 0),
    receipt_status   SMALLINT,                  -- 1 = success, 0 = revert
    receipt_data     JSONB,                     -- full receipt for debugging
    confirmations    INTEGER DEFAULT 0,

    -- Transfer metadata (always populated since all transactions are transfers)
    transfer_token   TEXT NOT NULL,              -- token symbol: "ETH", "USDC", etc.
    transfer_amount  TEXT NOT NULL,               -- original human-readable amount: "1500.50"
    transfer_recipient CHAR(42) NOT NULL,        -- original recipient (for ERC20, distinct from to_address)
    token_contract   CHAR(42),                   -- ERC20 contract address (NULL for native transfers)
    token_decimals   SMALLINT NOT NULL,          -- token decimals at time of transfer (for audit)

    -- Client metadata (opaque)
    metadata         JSONB,

    -- Timestamps
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    submitted_at     TIMESTAMPTZ,               -- first broadcast time
    confirmed_at     TIMESTAMPTZ,               -- receipt received time

    -- Pipeline ownership
    claimed_by       TEXT,                       -- pipeline ID that claimed this tx
    claimed_at       TIMESTAMPTZ
);

-- Indexes for common query patterns
CREATE INDEX idx_transactions_status_chain_sender
    ON transactions (status, chain_id, sender)
    WHERE status IN ('QUEUED', 'PENDING', 'SUBMITTED');

CREATE INDEX idx_transactions_chain_sender_nonce
    ON transactions (chain_id, sender, nonce)
    WHERE nonce IS NOT NULL;

CREATE INDEX idx_transactions_submitted_at
    ON transactions (submitted_at)
    WHERE status = 'SUBMITTED';

-- Index for transfer queries (filter by token, useful for operational dashboards)
CREATE INDEX idx_transactions_transfer_token
    ON transactions (chain_id, transfer_token);

-- Individual broadcast attempts (gas bumps create new attempts, same nonce).
CREATE TABLE tx_attempts (
    id               TEXT PRIMARY KEY,          -- ULID
    transaction_id   TEXT NOT NULL REFERENCES transactions(id),
    attempt_number   INTEGER NOT NULL,
    tx_hash          CHAR(66) NOT NULL,

    -- Gas parameters for this attempt
    gas_limit        BIGINT NOT NULL,
    max_fee_per_gas         NUMERIC(78, 0),     -- EIP-1559
    max_priority_fee_per_gas NUMERIC(78, 0),    -- EIP-1559
    gas_price               NUMERIC(78, 0),     -- Legacy
    tx_type          SMALLINT NOT NULL,          -- 0 = legacy, 2 = EIP-1559

    -- Signed raw transaction (for re-broadcast)
    raw_tx           BYTEA NOT NULL,

    -- Status of this specific attempt
    status           TEXT NOT NULL DEFAULT 'BROADCAST',  -- BROADCAST, CONFIRMED, REPLACED, DROPPED
    error_reason     TEXT,

    -- Timestamps
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    confirmed_at     TIMESTAMPTZ,

    UNIQUE (transaction_id, attempt_number)
);

CREATE INDEX idx_tx_attempts_transaction_id ON tx_attempts (transaction_id);
CREATE INDEX idx_tx_attempts_tx_hash ON tx_attempts (tx_hash);

-- Audit log for state transitions. Append-only.
CREATE TABLE tx_state_log (
    id               BIGSERIAL PRIMARY KEY,
    transaction_id   TEXT NOT NULL,
    from_status      TEXT,
    to_status        TEXT NOT NULL,
    actor            TEXT NOT NULL,              -- "pipeline:0xA-1", "receipt-poller", "gas-bumper", etc.
    reason           TEXT,
    metadata         JSONB,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_tx_state_log_transaction_id ON tx_state_log (transaction_id);
```

### Schema Design Decisions

**ULID for transaction IDs:** ULIDs are time-sortable (first 48 bits are millisecond timestamp),
globally unique, and URL-safe. This gives us natural chronological ordering without a serial
sequence, which is better for distributed systems and avoids sequence contention.

**NUMERIC(78,0) for value/gas fields:** EVM uint256 values can be up to 78 decimal digits.
Using `NUMERIC` instead of `BIGINT` (which caps at 19 digits) prevents overflow for large token
amounts. We use scale 0 (no decimals) because all values are in wei (integer).

**BYTEA for calldata and raw_tx:** Binary data should be stored as `BYTEA`, not hex strings.
More compact (half the size of hex) and avoids encoding/decoding bugs.

**Separate `tx_attempts` table:** A single logical transaction (from the client's perspective)
may result in multiple on-chain transactions (gas bumps). The `transactions` table represents the
logical intent; `tx_attempts` represents each physical broadcast. This separation is critical for
gas bump tracking and debugging.

**Append-only `tx_state_log`:** Every state transition is logged for debugging, auditing, and
anomaly detection. In a system that handles real money, you need a complete history of what
happened and why.

**Partial indexes:** The index on `status IN ('QUEUED', 'PENDING', 'SUBMITTED')` covers only
active transactions. Since most transactions eventually reach terminal states, this index stays
small even as the table grows.

**Transfer metadata columns:** Since all transactions are token transfers, the `transfer_*`
columns are always populated (NOT NULL, except `token_contract` which is NULL for native
transfers). The `transfer_recipient` is stored separately from `to_address` because for ERC20
transfers, `to_address` is the ERC20 contract and `transfer_recipient` is the actual recipient.
For native transfers, `to_address` and `transfer_recipient` are the same. The `token_decimals`
is denormalized at write time (snapshot of the whitelist config) so that historical queries remain
correct even if the whitelist config changes.

### Partitioning Strategy (future)

As the table grows, partition `transactions` by `chain_id` and `created_at` (monthly range).
This improves query performance for chain-specific operations and allows efficient data retention
(drop old partitions).

---

## 12. Security

### Private Key Management

**Current approach (MVP):** Private keys provided via environment variables. This is acceptable
for an internal service in a controlled deployment environment (e.g., Kubernetes with sealed
secrets).

**Format:**
```
TX_SENDER_KEYS_0xABC=0x<hex_private_key>
TX_SENDER_KEYS_0xDEF=0x<hex_private_key>
```

**Key handling rules:**
1. Keys are loaded into memory at startup and NEVER logged, serialized, or written to disk.
2. Keys are stored in a `sync.Map` keyed by lowercase address.
3. On startup, derive the address from each key and verify it matches the env var name. This
   catches copy-paste errors.
4. After loading, unset the environment variables from the process (Go: `os.Unsetenv`).
5. The signing function accepts a transaction and a sender address, looks up the key internally.
   The key material never leaves the signing module.

**Production upgrade path:** Replace env-var keys with:
- AWS KMS / GCP Cloud KMS for remote signing (adds latency but removes key material from memory)
- HashiCorp Vault with transit secrets engine
- Dedicated HSM (Hardware Security Module) for high-value signers

The signing interface should be abstracted behind an interface:
```go
type Signer interface {
    Sign(ctx context.Context, sender common.Address, tx *types.Transaction) (*types.Transaction, error)
    Addresses() []common.Address
}
```

This makes the key backend swappable without changing any pipeline logic.

### Input Validation

Every transfer request (`POST /v1/transfers`) must be validated:
- `idempotency_key`: must be non-empty, max 256 characters.
- `chain_id`: must be in the configured set of supported chains.
- `sender`: must be a valid 0x-prefixed address AND must be a registered signer.
- `token`: must be a non-empty string. Checked against the chain's native token symbol and ERC20
  whitelist. Reject with 403 if not found.
- `amount`: must be a valid positive decimal string. Must not have more decimal places than the
  token's configured decimals. Must not exceed the per-token `max_transfer` limit.
- `recipient`: must be a valid 0x-prefixed checksummed address. Must NOT be the zero address
  (`0x0000000000000000000000000000000000000000`). For ERC20 transfers, must also not be the
  token contract address itself (transferring tokens to the token contract is almost always a
  mistake and results in permanently locked tokens).
- `priority`: if provided, must be one of `"low"`, `"normal"`, `"high"`, `"urgent"`.

**No raw calldata accepted.** The API does not accept arbitrary `to`, `value`, or `data` fields.
All transaction parameters are derived from the transfer request after whitelist validation. This
eliminates an entire class of attacks where a compromised caller submits malicious calldata.

### Token Whitelist Security

The token whitelist is a critical security boundary. It constrains which ERC20 contracts the
transfer endpoint can interact with.

**Threat: Arbitrary contract interaction via transfer endpoint.**
Without the whitelist, a malicious or compromised caller could use the transfer endpoint to call
any contract's `transfer(address,uint256)` function, which might have unintended side effects on
non-ERC20 contracts that happen to have a function with the same 4-byte selector.

**Mitigation:** The whitelist is loaded from configuration at startup. Only contracts explicitly
listed can be targeted. The whitelist is immutable at runtime (no API to add/remove tokens while
running). Changes require a configuration update and service restart (or graceful reload, if
implemented).

**Threat: Whitelisted contract that is not actually an ERC20.**
A misconfigured whitelist entry could point to a contract that is not an ERC20, or to an EOA
address.

**Mitigation:** Startup validation checks `eth_getCode` for each whitelisted address. If it has
no code (is an EOA), the entry is rejected. Additionally, we could (optionally) call `decimals()`
and `symbol()` on each whitelisted contract at startup and verify they match the configured
values. This is not mandatory (some tokens do not implement these optional view functions), but
recommended where possible.

**Threat: Proxy contract upgrades changing ERC20 behavior.**
A whitelisted ERC20 that uses a proxy pattern could have its implementation changed after
whitelisting.

**Mitigation:** This is an inherent risk of interacting with upgradeable contracts and cannot be
fully mitigated at the transaction sender level. Operational procedures should include monitoring
token contract events and periodic review of whitelisted contracts. Use the `enabled` flag to
quickly disable a token if suspicious behavior is detected.

**Threat: Amount manipulation through decimal confusion.**
A caller might submit `amount: "1000"` intending 1000 USDC ($1000), but if the system used wrong
decimals (e.g., 18 instead of 6), it would send 0.000000001 USDC instead.

**Mitigation:** Decimals are configured in the whitelist and validated at startup. The
`token_decimals` value is snapshotted into the transaction record for audit purposes. All
transactions go through the transfer endpoint, which is the single code path that converts
human-readable amounts to smallest units. There is no alternative path that bypasses decimal
validation.

### Rate Limiting

Even without external auth, rate limit the API to prevent abuse from misconfigured clients:
- Per-sender rate limit: prevent one caller from monopolizing an EOA's pipeline.
- Global rate limit: prevent overloading the database or RPC endpoints.
- Implement as middleware using a token bucket algorithm.

### Gas Spend Limits

Configure maximum gas spend per:
- Transaction (max gas_limit * max_fee_per_gas * bump_factor)
- Sender per hour / per day
- Chain per hour / per day

This prevents a bug in the caller or gas price oracle from draining the sender's ETH balance.

### Token Transfer Limits

In addition to gas spend limits, the token whitelist enforces per-transfer limits:
- **Per-token max transfer:** Each whitelisted token (including native) has a configurable
  maximum amount per single transfer. This is a hard reject at the API level -- the transaction
  is never queued.
- **Purpose:** Catches bugs where the caller passes an amount in the wrong unit (e.g., passing
  wei when they meant ETH), or where a logic error produces an absurdly large transfer amount.
- **Not a replacement for application-level limits:** The calling service should have its own
  business-logic limits. The transfer limits here are a defense-in-depth safety net.
- **Configurable per token per chain:** Different chains may have different limits for the same
  token symbol (e.g., USDC limits might differ between Ethereum and Polygon due to different
  liquidity profiles).

### Network Security

- The service should only be accessible from the internal network (no public internet exposure).
- All RPC connections should use HTTPS/WSS.
- Database connections should use TLS with certificate verification.

---

## 13. Observability

### Structured Logging

All logs are structured JSON (via `zap`). Every log entry includes:
- `timestamp`: RFC3339 with nanoseconds
- `level`: debug, info, warn, error
- `message`: human-readable description
- `chain_id`: when applicable
- `sender`: when applicable
- `tx_id`: when applicable
- `tx_hash`: when applicable
- `nonce`: when applicable
- `component`: which subsystem (api, pipeline, receipt-poller, gas-bumper, etc.)
- `trace_id`: for request tracing across components

**Key log events:**
- Transaction submitted (INFO): `{tx_id, chain_id, sender, to, nonce, gas_params, tx_hash}`
- Transaction confirmed (INFO): `{tx_id, chain_id, tx_hash, block, gas_used, latency_ms}`
- Transaction reverted (WARN): `{tx_id, chain_id, tx_hash, block, revert_reason}`
- Gas bump (INFO): `{tx_id, chain_id, attempt, old_gas, new_gas, reason}`
- Nonce gap detected (ERROR): `{chain_id, sender, expected, actual}`
- RPC failure (WARN): `{chain_id, endpoint, error, latency_ms}`
- Reorg detected (WARN): `{chain_id, reorg_depth, affected_txs}`
- Transfer accepted (INFO): `{tx_id, chain_id, sender, recipient, token, amount, transfer_type}`
- Transfer rejected (WARN): `{chain_id, sender, token, amount, reason, recipient}`

### Prometheus Metrics

```
# Transaction lifecycle
tx_sender_transactions_total{chain_id, sender, status}           counter
tx_sender_transaction_duration_seconds{chain_id, sender, phase}  histogram
    # phase: queued_to_submitted, submitted_to_confirmed, total

# Gas
tx_sender_gas_price_gwei{chain_id, type}                         gauge
    # type: base_fee, priority_fee, max_fee
tx_sender_gas_used_total{chain_id, sender}                       counter
tx_sender_gas_spent_wei_total{chain_id, sender}                  counter

# Nonce
tx_sender_nonce_current{chain_id, sender}                        gauge
tx_sender_nonce_gaps_total{chain_id, sender}                     counter

# RPC
tx_sender_rpc_requests_total{chain_id, endpoint, method, status} counter
tx_sender_rpc_request_duration_seconds{chain_id, method}         histogram
tx_sender_rpc_endpoint_health{chain_id, endpoint}                gauge  (1=healthy, 0=unhealthy)

# Queue
tx_sender_queue_depth{chain_id, sender}                          gauge
tx_sender_pipeline_processing{chain_id, sender}                  gauge  (1=busy, 0=idle)

# Retries
tx_sender_gas_bumps_total{chain_id, sender}                      counter
tx_sender_rebroadcasts_total{chain_id, sender}                   counter

# Token Transfers
tx_sender_transfers_total{chain_id, sender, token, transfer_type}  counter
    # transfer_type: "native" or "erc20"
tx_sender_transfer_amount{chain_id, token}                         histogram
    # amount in human-readable units (for distribution analysis, NOT for accounting)
tx_sender_transfer_rejected_total{chain_id, token, reason}         counter
    # reason: "not_whitelisted", "disabled", "exceeds_limit", "invalid_amount", "zero_address"

# System
tx_sender_db_connections_active                                  gauge
tx_sender_db_query_duration_seconds{query}                       histogram
```

### Alerting Rules (Recommended)

| Alert                              | Condition                                  | Severity |
|------------------------------------|--------------------------------------------|----------|
| NoncGapDetected                    | `nonce_gaps_total` increases                | Critical |
| TransactionStuckTooLong            | SUBMITTED tx older than 10min              | High     |
| RPCAllEndpointsDown                | All endpoints unhealthy for a chain        | Critical |
| GasSpendExceedsLimit               | Hourly gas spend > threshold               | High     |
| QueueDepthTooHigh                  | queue_depth > 100 for any pipeline         | Warning  |
| ReorgDetected                      | Reorg affecting confirmed txs              | High     |
| TransactionRevertRate              | > 10% of txs reverting in 5min window      | Warning  |
| PipelineNotProcessing              | Pipeline idle with non-empty queue > 30s   | High     |
| TransferRejectionSpike             | `transfer_rejected_total` > 50 in 5min     | Warning  |
| UnwhitelistedTokenAttempts         | Rejections with reason `not_whitelisted`    | Warning  |

---

## 14. Configuration Model

Configuration is layered: defaults -> config file -> environment variables (highest priority).

```go
type Config struct {
    // Server
    Server ServerConfig

    // Database
    Database DatabaseConfig

    // Chains: keyed by chain ID
    Chains map[uint64]ChainConfig

    // Global retry config (overridable per chain)
    Retry RetryConfig

    // Global gas config (overridable per chain)
    Gas GasConfig

    // Alerting thresholds
    Alerts AlertConfig
}

type ServerConfig struct {
    Host            string        `env:"TX_SENDER_HOST" default:"0.0.0.0"`
    Port            int           `env:"TX_SENDER_PORT" default:"8080"`
    ReadTimeout     time.Duration `env:"TX_SENDER_READ_TIMEOUT" default:"10s"`
    WriteTimeout    time.Duration `env:"TX_SENDER_WRITE_TIMEOUT" default:"30s"`
    ShutdownTimeout time.Duration `env:"TX_SENDER_SHUTDOWN_TIMEOUT" default:"60s"`
}

type DatabaseConfig struct {
    URL             string `env:"TX_SENDER_DATABASE_URL" required:"true"`
    MaxConns        int    `env:"TX_SENDER_DB_MAX_CONNS" default:"25"`
    MinConns        int    `env:"TX_SENDER_DB_MIN_CONNS" default:"5"`
    MaxConnLifetime time.Duration `env:"TX_SENDER_DB_MAX_CONN_LIFETIME" default:"1h"`
}

type ChainConfig struct {
    ChainID            uint64
    Name               string            // human-readable: "ethereum", "polygon"
    RPCEndpoints       []string          // ordered by priority
    WSEndpoints        []string          // for subscriptions (optional)
    BlockTime          time.Duration     // expected block time
    ConfirmationBlocks int               // blocks to wait before marking CONFIRMED
    MaxReorgDepth      int               // max reorg depth to watch for
    SupportsEIP1559    bool              // auto-detected, but overridable

    // Gas overrides
    GasLimitMultiplier float64           // buffer for gas estimation (default: 1.2)
    MinMaxFee          *big.Int          // floor for maxFeePerGas
    MaxMaxFee          *big.Int          // ceiling for maxFeePerGas
    MinPriorityFee     *big.Int          // floor for priority fee
    MaxPriorityFee     *big.Int          // ceiling for priority fee

    // Timing
    StuckThreshold     time.Duration     // when to consider a tx stuck
    GasBumpInterval    time.Duration     // min time between gas bumps

    // Native token configuration
    NativeTokenSymbol   string            // "ETH", "MATIC", "AVAX", "BNB", etc.
    NativeTokenDecimals uint8             // 18 for all EVM natives, but explicit for safety
    NativeMaxTransfer   *big.Int          // max native transfer per tx in wei (nil = no limit)

    // ERC20 token whitelist -- only whitelisted tokens can be transferred.
    // Keyed by uppercase symbol. See section 6 for details.
    TokenWhitelist map[string]TokenConfig
}

type TokenConfig struct {
    Symbol          string         // "USDC", "USDT", "WETH", etc. (must match map key)
    ContractAddress string         // 0x-prefixed, checksummed
    Decimals        uint8          // token decimals (6 for USDC, 18 for WETH, etc.)
    MaxTransfer     *big.Int       // max per-transfer in smallest unit (nil = no limit)
    Enabled         bool           // false = temporarily suspended, transfers rejected
}
```

### Environment Variable Format for Chains

```bash
# Chain 1 (Ethereum Mainnet)
TX_SENDER_CHAIN_1_RPC_URLS="https://eth-mainnet.g.alchemy.com/v2/KEY,https://mainnet.infura.io/v3/KEY,https://rpc.ankr.com/eth"
TX_SENDER_CHAIN_1_WS_URLS="wss://eth-mainnet.g.alchemy.com/v2/KEY"
TX_SENDER_CHAIN_1_BLOCK_TIME="12s"
TX_SENDER_CHAIN_1_CONFIRMATION_BLOCKS="12"
TX_SENDER_CHAIN_1_STUCK_THRESHOLD="180s"

# Chain 1 native token
TX_SENDER_CHAIN_1_NATIVE_SYMBOL="ETH"
TX_SENDER_CHAIN_1_NATIVE_DECIMALS="18"
TX_SENDER_CHAIN_1_NATIVE_MAX_TRANSFER="100000000000000000000"  # 100 ETH

# Chain 1 whitelisted ERC20 tokens
TX_SENDER_CHAIN_1_TOKEN_USDC_ADDRESS="0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48"
TX_SENDER_CHAIN_1_TOKEN_USDC_DECIMALS="6"
TX_SENDER_CHAIN_1_TOKEN_USDC_MAX_TRANSFER="10000000000"
TX_SENDER_CHAIN_1_TOKEN_USDC_ENABLED="true"

TX_SENDER_CHAIN_1_TOKEN_USDT_ADDRESS="0xdAC17F958D2ee523a2206206994597C13D831ec7"
TX_SENDER_CHAIN_1_TOKEN_USDT_DECIMALS="6"
TX_SENDER_CHAIN_1_TOKEN_USDT_MAX_TRANSFER="10000000000"
TX_SENDER_CHAIN_1_TOKEN_USDT_ENABLED="true"

# Chain 137 (Polygon)
TX_SENDER_CHAIN_137_RPC_URLS="https://polygon-mainnet.g.alchemy.com/v2/KEY,https://rpc.ankr.com/polygon"
TX_SENDER_CHAIN_137_BLOCK_TIME="2s"
TX_SENDER_CHAIN_137_CONFIRMATION_BLOCKS="64"
TX_SENDER_CHAIN_137_STUCK_THRESHOLD="30s"

# Chain 137 native token
TX_SENDER_CHAIN_137_NATIVE_SYMBOL="MATIC"
TX_SENDER_CHAIN_137_NATIVE_DECIMALS="18"
TX_SENDER_CHAIN_137_NATIVE_MAX_TRANSFER="1000000000000000000000"  # 1000 MATIC

# Chain 137 whitelisted ERC20 tokens
TX_SENDER_CHAIN_137_TOKEN_USDC_ADDRESS="0x3c499c542cEF5E3811e1192ce70d8cC03d5c3359"
TX_SENDER_CHAIN_137_TOKEN_USDC_DECIMALS="6"
TX_SENDER_CHAIN_137_TOKEN_USDC_MAX_TRANSFER="10000000000"
TX_SENDER_CHAIN_137_TOKEN_USDC_ENABLED="true"

# Signer keys
TX_SENDER_KEYS_0xABCDEF1234567890ABCDEF1234567890ABCDEF12="0x<private_key>"
TX_SENDER_KEYS_0x1234567890ABCDEF1234567890ABCDEF12345678="0x<private_key>"

# Database
TX_SENDER_DATABASE_URL="postgres://user:pass@localhost:5432/tx_sender?sslmode=require"
```

---

## 15. Error Taxonomy

Errors are classified by recoverability and urgency. Every error in the system maps to one of
these categories:

### Error Categories

```go
type ErrorCategory string

const (
    // Transient: will likely succeed on retry
    ErrTransient ErrorCategory = "TRANSIENT"

    // Permanent: will never succeed, no point retrying
    ErrPermanent ErrorCategory = "PERMANENT"

    // Resource: a resource limit was hit, may succeed after backoff
    ErrResource ErrorCategory = "RESOURCE"

    // Configuration: service is misconfigured
    ErrConfig ErrorCategory = "CONFIGURATION"

    // Chain: blockchain-level issue
    ErrChain ErrorCategory = "CHAIN"
)
```

### Error Codes

| Code                          | Category    | HTTP | Description                           |
|-------------------------------|-------------|------|---------------------------------------|
| `INVALID_ADDRESS`             | PERMANENT   | 400  | Malformed address                     |
| `INVALID_CHAIN`               | PERMANENT   | 400  | Unsupported chain ID                  |
| `INVALID_VALUE`               | PERMANENT   | 400  | Malformed or negative value           |
| `UNKNOWN_SENDER`              | PERMANENT   | 404  | Sender key not loaded                 |
| `IDEMPOTENCY_CONFLICT`        | PERMANENT   | 409  | Same key, different params            |
| `CHAIN_PAUSED`                | RESOURCE    | 422  | Chain temporarily disabled            |
| `ESTIMATION_REVERTED`         | PERMANENT   | 422  | eth_estimateGas failed (calldata reverts) |
| `INSUFFICIENT_FUNDS`          | PERMANENT   | 422  | Sender balance too low                |
| `NONCE_TOO_LOW`               | TRANSIENT   | -    | Internal: re-sync nonce              |
| `NONCE_GAP`                   | CHAIN       | -    | Internal: nonce gap detected         |
| `RPC_UNAVAILABLE`             | TRANSIENT   | -    | All RPC endpoints failing            |
| `RPC_RATE_LIMITED`            | RESOURCE    | -    | RPC provider rate limit hit          |
| `BROADCAST_FAILED`            | TRANSIENT   | -    | sendRawTransaction failed            |
| `REPLACEMENT_UNDERPRICED`     | RESOURCE    | -    | Gas bump too small for replacement   |
| `GAS_LIMIT_EXCEEDED`          | PERMANENT   | -    | Transaction would exceed block gas limit |
| `GAS_SPEND_LIMIT`             | RESOURCE    | -    | Internal spend limit reached         |
| `TX_STUCK`                    | TRANSIENT   | -    | Tx not mined within threshold        |
| `TX_EVICTED`                  | TRANSIENT   | -    | Tx evicted from mempool              |
| `REORG_DETECTED`              | CHAIN       | -    | Chain reorganization                 |
| `MAX_RETRIES_EXCEEDED`        | PERMANENT   | -    | All retry attempts exhausted         |
| `SIGNING_FAILED`              | CONFIG      | -    | Key material issue                   |
| `DB_UNAVAILABLE`              | TRANSIENT   | 503  | Database connection failed           |
| `INTERNAL_ERROR`              | TRANSIENT   | 500  | Unexpected internal error            |

#### Transfer-Specific Error Codes

| Code                          | Category    | HTTP | Description                                             |
|-------------------------------|-------------|------|---------------------------------------------------------|
| `TOKEN_NOT_WHITELISTED`       | PERMANENT   | 403  | Token symbol not in whitelist for this chain             |
| `TOKEN_DISABLED`              | RESOURCE    | 403  | Token exists in whitelist but is disabled                |
| `INVALID_AMOUNT`              | PERMANENT   | 400  | Amount string is not a valid decimal number              |
| `AMOUNT_TOO_MANY_DECIMALS`    | PERMANENT   | 400  | Amount has more decimal places than token supports       |
| `ZERO_AMOUNT`                 | PERMANENT   | 422  | Transfer amount is zero or negative                     |
| `EXCEEDS_TRANSFER_LIMIT`      | PERMANENT   | 422  | Amount exceeds per-token max_transfer                   |
| `AMOUNT_OVERFLOW`             | PERMANENT   | 422  | Amount exceeds uint256 max after conversion              |
| `ZERO_ADDRESS_RECIPIENT`      | PERMANENT   | 400  | Recipient is the zero address                           |
| `SELF_TRANSFER_TOKEN`         | PERMANENT   | 400  | ERC20 recipient is the token contract address itself    |
| `INVALID_RECIPIENT`           | PERMANENT   | 400  | Recipient is not a valid address                        |

---

## 16. Edge Cases & Failure Modes

### Chain Reorgs

**Scenario:** Transaction T is confirmed at block B. A reorg replaces block B with block B'.
Transaction T may or may not be included in B'.

**Detection:** The reorg watcher tracks block hashes for the last `MaxReorgDepth` blocks per
chain. When a new block arrives at a height we have already seen with a different hash, a reorg
is detected.

**Handling:**
1. Identify all transactions confirmed in the reorged block range.
2. Re-check each transaction's receipt against the new canonical chain.
3. If still confirmed: no action, update block hash.
4. If missing from new chain:
   a. Revert status to `SUBMITTED`.
   b. Check if the transaction is in the mempool (it should be, unless the reorg conflicted).
   c. If not in the mempool, re-broadcast the signed raw transaction (we stored it in
      `tx_attempts.raw_tx`).
   d. Wait for re-confirmation.

**Confirmation threshold:** For high-value transactions, do not report `CONFIRMED` until
`ConfirmationBlocks` blocks have passed. For Ethereum mainnet, 12 blocks (~2.4 minutes) is
standard. For Polygon, 64 blocks (~2 minutes). For L2s like Arbitrum, 1 block is usually
sufficient (finality is provided by the L1).

### Mempool Eviction

**Scenario:** A transaction was broadcast but is evicted from all nodes' mempools (due to low
gas price relative to demand, mempool size limits, or node restarts).

**Detection:** The stuck transaction detector finds SUBMITTED transactions with no receipt after
the chain-specific threshold.

**Handling:**
1. Re-broadcast the original signed transaction (from `tx_attempts.raw_tx`).
2. If re-broadcast succeeds, continue waiting.
3. If re-broadcast fails (e.g., "already known"), check for receipt again.
4. If re-broadcast fails (e.g., "nonce too low"), the transaction was mined while we were
   checking. Query the receipt.
5. If the nonce has been used by a different transaction (external interference), mark as FAILED
   and alert.

### Gas Price Spikes

**Scenario:** Gas prices spike 10x during a network congestion event.

**Handling:**
1. The gas engine naturally adapts because it queries `eth_feeHistory` and `baseFeePerGas`
   from recent blocks.
2. The `MaxMaxFee` ceiling prevents spending unlimited gas.
3. If the ceiling is hit, transactions queue up naturally. The pipeline still assigns nonces
   and broadcasts, but transactions wait in the mempool until gas prices drop.
4. If gas prices stay elevated, the gas bumper gradually increases fees up to the cost cap.
5. Beyond the cost cap, human intervention is required. Alert the operator.

### RPC Provider Failures

**Scenario:** Primary RPC provider goes down or returns errors.

**Handling:**
1. The RPC pool automatically fails over to the next healthy endpoint.
2. Health is determined by periodic `eth_blockNumber` polls and error rates on recent requests.
3. If ALL endpoints for a chain are unhealthy:
   a. Log CRITICAL alert.
   b. Pipelines for that chain pause (stop claiming new transactions).
   c. Existing SUBMITTED transactions continue to be monitored (the endpoint may come back).
   d. New transactions for that chain are still accepted (QUEUED) but not processed.

### Concurrent Transactions for the Same Nonce

**Scenario:** Due to gas bumping, we have two transactions in the mempool with the same nonce
(the original and the replacement).

**Handling:** This is expected and correct behavior. Only one can be mined.
1. Both are tracked as separate `tx_attempts`.
2. When either is confirmed, check the `tx_hash` in the receipt against our attempts.
3. Mark the confirmed attempt as CONFIRMED.
4. Mark the other attempt(s) as REPLACED.
5. Update the parent `transactions` row with the confirmed attempt's data.

### Sender Balance Depletion

**Scenario:** An EOA runs out of ETH to pay gas fees.

**Detection:** `eth_estimateGas` or `sendRawTransaction` returns "insufficient funds."

**Handling:**
1. Mark the current transaction as FAILED with `INSUFFICIENT_FUNDS`.
2. Log CRITICAL alert (solver EOA needs funding).
3. Pause the pipeline for this sender/chain (no point trying subsequent transactions).
4. Periodically check the balance. Resume when funds are available.
5. The health endpoint reflects this state.

### Process Crash During Transaction Submission

**Scenario:** Process crashes after signing and broadcasting but before updating the database.

**State on restart:**
- Database shows the transaction as PENDING (claimed, nonce assigned, but no tx_hash recorded).
- On-chain, the transaction may be in the mempool or already mined.

**Recovery:**
1. On startup, find all PENDING transactions.
2. For each, check on-chain: query receipts for the nonce we assigned from our `nonce_cursors`.
3. If a receipt exists at that nonce from this sender, we can match it to our transaction
   (compare `to`, `value`, `data`).
4. If no receipt and nonce < on-chain pending count, the tx is in the mempool. We just lost
   the hash. Mark as SUBMITTED and wait.
5. If nonce >= on-chain pending count, the transaction was never broadcast. Reset to QUEUED
   and re-process (may get a new nonce).

This is why we store `raw_tx` in `tx_attempts` -- it allows us to reconstruct the tx_hash from
the signed bytes and compare against on-chain data.

### External Nonce Interference

**Scenario:** Someone sends a transaction from one of our managed EOAs outside this system
(e.g., via MetaMask or another service).

**Detection:** The nonce reconciler detects that on-chain nonce has advanced beyond our cursor.

**Handling:**
1. Log CRITICAL alert -- this should never happen in a properly secured environment.
2. Advance our local cursor to match on-chain.
3. Any in-flight transactions we had for the skipped nonces are now invalid (nonce too low).
   Mark them as FAILED.
4. Re-queue the logical transactions if they were not completed by the external sender.

### ERC20 Transfer Edge Cases

#### Non-Standard Decimals

**Scenario:** A whitelisted ERC20 has unusual decimals (e.g., 0 decimals for some reward tokens,
2 decimals for some stablecoins, or 8 decimals for WBTC).

**Handling:** The whitelist stores decimals explicitly per token. The `parseAmountToSmallestUnit`
function handles any decimal count from 0 to 18+. For 0-decimal tokens, fractional amounts in
the request are rejected (e.g., `"1.5"` for a 0-decimal token returns `AMOUNT_TOO_MANY_DECIMALS`).
This is validated at the API level before any transaction is constructed.

#### Transfer Amount Overflow (uint256 Exceeded)

**Scenario:** A caller submits an amount that, after decimal conversion, exceeds the maximum
uint256 value (`2^256 - 1`, approximately `1.158 * 10^77`).

**Handling:** After converting to smallest units, the result is checked against `MaxUint256`.
This is practically impossible for standard tokens (even `10^18 * 10^18` fits), but the check
is included for defense-in-depth, especially for custom tokens with 0 decimals and large amounts.
Returns `AMOUNT_OVERFLOW` if exceeded.

```go
var maxUint256, _ = new(big.Int).SetString(
    "115792089237316195423570985008687907853269984665640564039457584007913129639935", 10)

func validateAmountBounds(amount *big.Int) error {
    if amount.Cmp(maxUint256) > 0 {
        return ErrAmountOverflow
    }
    return nil
}
```

#### Zero-Value Transfers

**Scenario:** A caller submits `amount: "0"` or `amount: "0.00"`.

**Handling:** Rejected at the API level with `ZERO_AMOUNT` (HTTP 422). While ERC20 contracts
technically allow zero-value transfers (they succeed on-chain and emit a Transfer event), they
serve no purpose in a solver context and are more likely to indicate a bug in the caller.

#### Transfer to Zero Address

**Scenario:** A caller submits `recipient: "0x0000000000000000000000000000000000000000"`.

**Handling:** Rejected at the API level with `ZERO_ADDRESS_RECIPIENT` (HTTP 400). For ERC20
tokens, transferring to the zero address typically results in permanent loss of tokens (burned).
For native transfers, sending to the zero address burns the ETH. Neither is ever intended in a
solver context. Note: some ERC20 contracts explicitly revert on `transfer(address(0), amount)`,
but not all -- so we enforce this at the API level rather than relying on the contract.

#### Transfer to the Token Contract Address

**Scenario:** A caller submits an ERC20 transfer where `recipient` equals the ERC20 contract
address itself (e.g., sending USDC to the USDC contract).

**Handling:** Rejected at the API level with `SELF_TRANSFER_TOKEN` (HTTP 400). Tokens sent to
the token contract are almost always permanently locked (the contract has no mechanism to
withdraw them). This is a common user mistake in the wild and a plausible bug in calling code.

#### Contract Address That Is Not Actually an ERC20

**Scenario:** The whitelist is misconfigured, and a listed address points to a non-ERC20 contract
(e.g., a multisig wallet, a DEX router, or a deprecated contract).

**Handling:** The startup validation (see section 6) checks `eth_getCode` for each whitelisted
address. Beyond that, the system does not verify ERC20 compliance at runtime. If a non-ERC20
contract receives a `transfer(address,uint256)` call:
- If the contract has no function matching the selector: the transaction reverts (caught as
  `REVERTED` status). No funds are lost because the ERC20 call fails atomically.
- If the contract has a function with the same 4-byte selector but different semantics: this is
  the dangerous case. The whitelist verification process MUST ensure this cannot happen. Use
  `eth_call` simulation during configuration validation if there is any doubt.

**Recommended operational practice:** When adding a token to the whitelist, verify:
1. The address has contract code (`eth_getCode`).
2. Calling `decimals()` returns the expected value.
3. Calling `symbol()` returns the expected value (if the contract supports it).
4. Calling `transfer(test_address, 0)` via `eth_call` does not revert (dry-run validation).

#### ERC20 Transfer Returning `false` Instead of Reverting

**Scenario:** Some non-standard ERC20 tokens (notably USDT on Ethereum) do not return a boolean
from `transfer()`, or return `false` on failure instead of reverting.

**Handling:** For the transfer endpoint, we call the standard `transfer(address,uint256)` which
has a `bool` return type. The go-ethereum ABI encoder/decoder handles the call. However, at the
transaction sender level, we do not inspect ERC20 return values -- we only see whether the
on-chain transaction reverted (`receipt.status == 0`) or succeeded (`receipt.status == 1`). A
`transfer` that returns `false` without reverting would show as `CONFIRMED` (receipt status 1) in
our system, even though the transfer did not actually happen.

**Mitigation for production:** For tokens known to have this behavior, the calling service should
verify the actual token balance change after confirmation, or we could add an optional receipt
log parser that checks for the `Transfer(address,address,uint256)` event log in the receipt. This
is flagged as a future improvement. The initial implementation assumes whitelisted tokens follow
the ERC20 standard (revert on failure). USDT on Ethereum is a notable exception -- its `transfer`
function does not return a `bool`. Using `go-ethereum`'s ABI encoder with the standard
`transfer(address,uint256) returns (bool)` signature against USDT will still work because the ABI
decoder in `go-ethereum` tolerates missing return data for functions that declare a return type.
The transaction itself succeeds; the ABI decoding is only relevant if the caller inspects the
return value, which the pipeline does not.

#### Allowance / Approval Patterns

**Scenario:** Does the sender EOA need to approve the transfer first?

**Handling:** No. The transfer endpoint uses `transfer(address,uint256)`, which moves tokens
that the CALLER (our sender EOA) owns directly. No approval is needed because the sender is
transferring its own tokens. `transferFrom(address,address,uint256)` is the function that
requires prior approval -- it is used when a third party moves tokens on behalf of the owner.
Since our sender EOAs are the direct holders, `transfer` is the correct function.

If the calling service ever needs to use `transferFrom` (e.g., to move tokens from a user wallet
that has approved our EOA), that would require a new dedicated endpoint with its own validation,
security controls, and audit trail. It is intentionally not supported in the current API surface.

#### Insufficient ERC20 Balance

**Scenario:** The sender EOA does not hold enough of the ERC20 token to cover the transfer amount.

**Handling:** Unlike native token balance which can be checked at gas estimation time, ERC20
balance is not automatically checked before submission. The transaction will be submitted, and the
ERC20 `transfer` call will revert on-chain (the contract checks `balanceOf(sender) >= amount`).
The transaction will show as `REVERTED` with a revert reason from the contract.

**Optional enhancement:** Before queuing an ERC20 transfer, the API could call
`balanceOf(sender)` via `eth_call` to check if the balance is sufficient. This is a best-effort
check (the balance could change between the check and the transaction mining), but it catches
obvious cases early and provides a better error message to the caller. This is NOT included in the
initial implementation to keep the transfer endpoint fast (no RPC calls in the hot path), but is
recommended as a future improvement for high-value transfers.

---

## 17. Assumptions

1. **Single instance initially.** The design supports single-process deployment. Multi-instance
   deployment requires adding distributed locking for pipeline ownership (Postgres advisory
   locks are the recommended approach). The schema and API already support this -- only the
   pipeline claim logic needs a lock.

2. **Private keys in env vars.** Acceptable for an internal service in a secure environment.
   The signing interface is abstracted for easy upgrade to KMS/HSM.

3. **Postgres is available and reliable.** The database is the single source of truth. Use
   managed Postgres (RDS, CloudSQL) with automated backups, point-in-time recovery, and
   failover replicas.

4. **RPC endpoints are provided by the operator.** The service does not discover or manage RPC
   endpoints. The operator is responsible for provisioning reliable RPC access (Alchemy, Infura,
   QuickNode, or self-hosted nodes).

5. **Clients handle their own retry logic.** If the service is down, clients retry submission.
   The idempotency key ensures no duplicates.

6. **Token transfers only.** This service handles native and whitelisted ERC20 token transfers
   exclusively. It does not support arbitrary contract calls, contract deployment, or any other
   transaction type. This is a deliberate security decision for solver infrastructure.

7. **UTF-8 / JSON callers.** All API callers speak JSON over HTTP. No binary protocol needed.

8. **Supported chains are known at startup.** Adding a new chain requires a configuration change
   and restart (or graceful reload).

---

## 18. Future Improvements

Listed in rough priority order:

1. **WebSocket/SSE subscription for transaction status.** Clients can subscribe to status updates
   instead of polling. Reduces load and improves responsiveness.

2. **Multi-instance deployment with leader election.** Use Postgres advisory locks to elect one
   instance as the pipeline runner. Others handle API traffic and serve as warm standbys.

3. **KMS/HSM signing integration.** Remove key material from process memory. Essential for
   production at scale.

4. **Priority queue within pipelines.** Currently FIFO within each pipeline. Adding priority
   queuing (urgent > high > normal > low) requires a priority-aware claim query.

5. **Transaction simulation before submission.** Use `eth_call` or trace APIs to simulate the
   transaction before broadcast. Catch reverts before consuming a nonce.

6. **Balance monitoring and auto-funding.** Monitor EOA balances and trigger alerts or automated
   funding from a treasury when balances drop below thresholds.

7. **Chain-specific optimizations.** L2s like Arbitrum and Optimism have different gas models
   (L1 data fee + L2 execution fee). The gas engine should account for this.

8. **Batch transfer submission (Disperse pattern).** The current design sends one transaction per
   transfer. For high-volume workloads on expensive chains (Ethereum L1), batching multiple
   transfers into a single transaction can save significant gas. This section describes the
   target architecture for batching — it is **not part of the initial implementation**.

   **Hybrid two-tier strategy:**

   | Tier | When | Method |
   |------|------|--------|
   | Tier 1 (current) | Urgent/high-priority transfers, *all* L2 transfers | Individual transaction per transfer |
   | Tier 2 (future) | Normal/low-priority transfers on expensive chains (Ethereum L1) | Batched via custom Disperse contract |

   L2 transfers remain individual because the gas savings from batching are negligible (<$0.01
   per transfer) and not worth the added complexity.

   **Custom batch transfer contract:**

   ```solidity
   // Simplified interface — production version would include NatSpec + full error handling
   contract BatchTransfer {
       event TransferExecuted(uint256 indexed batchId, uint256 index, address to, uint256 amount, bool success);

       /// @notice Batch-send native currency (ETH) to multiple recipients.
       /// @dev Partial success: each sub-transfer is independent. Returns bool[] results.
       function batchTransferNative(
           uint256 batchId,
           address[] calldata recipients,
           uint256[] calldata amounts
       ) external payable returns (bool[] memory results);

       /// @notice Batch-send an ERC-20 token to multiple recipients.
       /// @dev Uses SafeERC20 for USDT compatibility (non-standard return value).
       ///      Caller must have approved this contract for sum(amounts).
       function batchTransferERC20(
           uint256 batchId,
           IERC20 token,
           address[] calldata recipients,
           uint256[] calldata amounts
       ) external returns (bool[] memory results);
   }
   ```

   Key design choices:
   - **Partial success, not all-or-nothing.** Each sub-transfer executes independently. A single
     failing recipient (e.g., a contract that reverts on receive) does not block the rest.
     `bool[] results` lets the caller identify and retry only the failures.
   - **Per-sub-transfer events.** `TransferExecuted` fires for each recipient with `success` flag,
     enabling standard observability tooling to track individual transfers within a batch.
   - **SafeERC20 for USDT.** USDT's `transfer()` does not return a bool. Using OpenZeppelin's
     `SafeERC20.safeTransfer` handles this transparently.
   - **Immutable, no admin functions.** No owner, no pause, no upgradability. The contract is
     verified on Etherscan and its behavior is fixed at deploy time.

   **Batch accumulator in the pipeline:**

   The pipeline stage between "claim" and "sign" would gain a batch accumulator that groups
   transfers by `(sender_address, chain_id, token_address)`:

   ```
   Incoming transfers ──► Accumulator ──► Batch full or timer fires ──► Sign & submit batch tx
                          per (sender, chain, token)
   ```

   Configuration knobs:
   - `max_batch_size` — maximum transfers per batch (e.g., 50). Upper-bounded by block gas limit.
   - `max_batch_wait_ms` — maximum time to wait for a full batch (e.g., 2000ms). Prevents
     low-volume periods from adding unbounded latency.
   - `min_batch_size` — threshold below which the accumulator falls back to individual
     transactions (e.g., 3). If only 1-2 transfers arrive before the timer fires, batching
     overhead outweighs savings.

   **Token approval management:**

   EOAs must `approve()` the batch contract for each whitelisted ERC-20 token. This requires:
   - A startup check that verifies sufficient allowance for each `(eoa, token)` pair.
   - Periodic monitoring to detect allowance exhaustion (each `transferFrom` decrements it).
   - A management endpoint or startup routine to issue `approve(batchContract, type(uint256).max)`
     transactions when needed.

   **Gas savings estimates:**

   | Scenario | Individual gas | Batched gas | Savings | USD saved (10 Gwei, $2500 ETH) |
   |----------|---------------|-------------|---------|-------------------------------|
   | 25 ERC-20 transfers | ~1,300,000 | ~796,000 | ~504,000 gas | ~$1.26 |
   | 100 ERC-20 transfers | ~5,200,000 | ~3,380,000 | ~1,820,000 gas (~35%) | ~$4.55 |
   | 25 native (ETH) transfers | ~525,000 | ~325,000 | ~200,000 gas | ~$0.50 |

   On L2s (Arbitrum, Optimism, Base) the per-transfer cost is already <$0.01, so the absolute
   savings are negligible. Batching should only be enabled for Ethereum L1 (and potentially other
   expensive L1s in the future).

   **Flashbots Protect RPC:**

   For Ethereum L1 submissions, using `https://rpc.flashbots.net` as the RPC endpoint provides
   MEV protection with zero code changes:
   - Transactions are sent to Flashbots builders, not the public mempool.
   - Prevents frontrunning and sandwich attacks on token transfers.
   - Complementary to batching — use both together for optimal L1 cost and safety.
   - Trade-off: slightly slower inclusion (builder auction latency) vs. MEV protection.

   **EIP-7702 delegation (longer-term):**

   EIP-7702 allows an EOA to delegate execution to a contract for a single transaction, meaning
   the EOA itself is `msg.sender` while executing batch logic. This eliminates the need for
   separate token approvals and simplifies the trust model. Once EIP-7702 support matures across
   target chains, it becomes the preferred approach over the standalone Disperse contract. Monitor
   adoption before investing in implementation.

   **Approaches explicitly rejected:**
   - **ERC-4337 (Account Abstraction).** Designed for smart-contract wallets with UserOps,
     bundlers, paymasters, and entry points. Overkill for a backend service that controls its own
     EOAs. Adds significant complexity (alt mempool, bundler dependency) with no benefit over a
     simple Disperse contract.
   - **CREATE2 + SELFDESTRUCT batching.** A legacy pattern where a contract is deployed via
     CREATE2, executes transfers in the constructor, then self-destructs for a gas refund.
     Post-EIP-6780 (Dencun, March 2024), `SELFDESTRUCT` no longer clears storage or refunds gas
     in most cases, making this pattern non-viable.

9. **Transaction cancellation API.** Allow clients to cancel a QUEUED or SUBMITTED transaction.
   For QUEUED: remove from queue. For SUBMITTED: send a zero-value self-transfer at the same
   nonce with higher gas.

10. **Metrics dashboard and alerting integration.** Pre-built Grafana dashboards and PagerDuty/
    Slack alerting rules.

11. **Data retention and archival.** Move old terminal transactions to cold storage. Keep the
    active table small for performance.

12. **Dry-run mode.** Accept and validate transactions without actually broadcasting them.
    Useful for testing caller integration.
