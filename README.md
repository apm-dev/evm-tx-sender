# tx-sender

An EVM transaction sender microservice for solver infrastructure. It handles the hard parts of sending on-chain token transfers -- nonce management, gas estimation, retries, receipt tracking -- so callers just say "send 100 USDC to 0x... on Arbitrum" and poll for the result. Targets hundreds of TPS across multiple chains with multiple transactions per block.

## How to Run and Use

### Prerequisites

- Go 1.24+
- PostgreSQL 15+
- At least one EVM RPC endpoint

### Docker Compose (quickest start)

```bash
# Create a .env and config.yml file with your chain config and signer keys
# see .env.example and config.example.yml as references
docker compose up
```

The service starts on port 8080. PostgreSQL runs alongside it with automatic migrations.

### Running Locally

```bash
# Start just Postgres
docker compose up postgres -d

# Set required env vars (.env) and config.yml
make run
```

### Configuration

Configuration can be provided via a `config.yml` file, environment variables, or both. Env vars take precedence over YAML values. The config file is loaded from `./config.yml` by default (override with `CONFIG_PATH` env var). Private keys are env-var only for security reasons.

A sample `config.yml`:

```yaml
server:
  host: "0.0.0.0"
  port: 8080

database:
  url: "postgres://txsender:txsender@localhost:5432/txsender?sslmode=disable"

chains:
  "42161":
    name: "Arbitrum"
    rpc-urls:
      - "https://arb1.arbitrum.io/rpc"
      - "https://arbitrum-one-rpc.publicnode.com"
    block-time: "250ms"
    confirmation-blocks: 0
    native-symbol: "ETH"
    stuck-threshold: "60s"
    tokens:
      usdc:
        address: "0xaf88d065e77c8cC2239327C5EDb3A432268e5831"
        decimals: 6
        enabled: true
```

See `config.yml` in the repo root for a full example with all available options.

Environment variable equivalents use underscores and uppercase (e.g., `chains.42161.rpc-urls` becomes `CHAINS_42161_RPC_URLS`). Key prefixes:

| Variable | Description | Default |
|---|---|---|
| `DATABASE_URL` | PostgreSQL connection string | (required) |
| `SERVER_PORT` | HTTP listen port | `8080` |
| `SERVER_HOST` | HTTP listen address | `0.0.0.0` |
| `CHAINS_<ID>_NAME` | Chain display name | `chain-<ID>` |
| `CHAINS_<ID>_RPC_URLS` | Comma-separated RPC endpoints | (required) |
| `CHAINS_<ID>_BLOCK_TIME` | Expected block time | `12s` |
| `CHAINS_<ID>_CONFIRMATION_BLOCKS` | Blocks to wait before CONFIRMED | `12` |
| `CHAINS_<ID>_NATIVE_SYMBOL` | Native token symbol | `ETH` |
| `CHAINS_<ID>_STUCK_THRESHOLD` | Time before a tx is considered stuck | `180s` |
| `CHAINS_<ID>_GAS_BUMP_INTERVAL` | Min time between gas bumps | `30s` |
| `CHAINS_<ID>_TOKENS_<SYM>_ADDRESS` | ERC20 contract address | -- |
| `CHAINS_<ID>_TOKENS_<SYM>_DECIMALS` | Token decimals | `18` |
| `CHAINS_<ID>_TOKENS_<SYM>_MAX_TRANSFER` | Max transfer amount (smallest unit) | -- |
| `TX_SENDER_KEYS_<ADDRESS>` | Private key hex for sender EOA | (required) |

Private keys are cleared from the process environment after loading.

### API Usage

You can use the **Swagger UI** at `http://localhost:8080/swagger/index.html` to explore and test all endpoints interactively from the browser -- no curl needed.

**Submit a transfer:**

```bash
curl -X POST http://localhost:8080/v1/transfers \
  -H "Content-Type: application/json" \
  -d '{
    "idempotency_key": "settlement-batch-42-transfer-7",
    "chain_id": 42161,
    "sender": "0xYourSenderAddress",
    "recipient": "0xRecipientAddress",
    "token": "USDC",
    "amount": "100.50",
    "priority": "high"
  }'
```

Returns `202 Accepted` with a transaction ID immediately. The transfer is processed asynchronously.

**Poll for status:**

```bash
curl http://localhost:8080/v1/transactions/01HWXYZ...
```

**List transactions:**

```bash
curl "http://localhost:8080/v1/transactions?chain_id=42161&status=CONFIRMED&limit=50"
```

**Health check:**

```bash
curl http://localhost:8080/v1/health
```

## High-Level Design

```
                                  +------------------+
    POST /v1/transfers            |   PostgreSQL     |
         |                        |  +------------+  |
         v                        |  |transactions|  |
  +-------------+    write        |  |tx_attempts |  |
  | API Handler |---------------->|  |nonce_cursor|  |
  | (validate,  |    QUEUED       |  |tx_state_log|  |
  |  encode)    |                 +--+------------+--+
  +------+------+                        |    ^
         |                               |    |
    notify pipeline                read  |    | write
         |                               v    |
  +------v---------+            +--------+----+-------+
  | Pipeline       |  claim     | Pipeline goroutine  |  per (sender, chain)
  | Manager        |----------->| 1. Assign nonce     |  sequential processing
  | (one per app)  |            | 2. Estimate gas     |  = no nonce races
  +----------------+            | 3. Sign tx          |
                                | 4. Broadcast        |
                                +----------+----------+
                                           |
                     +---------------------+---------------------+
                     |                     |                     |
              +------v------+      +------v------+      +------v------+
              | RPC Pool    |      | Receipt     |      | Stuck       |
              | broadcast   |      | Poller      |      | Detector    |
              | to ALL      |      | batch poll  |      | gas bump &  |
              | healthy     |      | + reorg     |      | resubmit    |
              | endpoints   |      | detection   |      | same nonce  |
              +------+------+      +------+------+      +------+------+
                     |                     |                     |
                     v                     v                     v
              +------+------+      +------+------+      +------+------+
              | EVM Chain   |      | EVM Chain   |      | EVM Chain   |
              | (mempool)   |      | (receipts)  |      | (replace)   |
              +-------------+      +-------------+      +-------------+
```

### Transaction State Machine

```
QUEUED --> PENDING --> SUBMITTED --> INCLUDED --> CONFIRMED
                                 \            \-> SUBMITTED (reorg)
                                  \-> REVERTED
                   \-> FAILED
```

- **QUEUED** -- stored in DB, waiting for pipeline to claim
- **PENDING** -- nonce assigned, gas estimated, being signed/broadcast
- **SUBMITTED** -- broadcast to network, awaiting receipt
- **INCLUDED** -- receipt found, waiting for confirmation depth (optional, configurable per chain)
- **CONFIRMED** -- receipt with status=1 and sufficient confirmations
- **REVERTED** -- receipt with status=0 (on-chain revert)
- **FAILED** -- couldn't get on-chain (signing error, gas estimation revert, max retries)

### Package Structure

```
cmd/tx-sender/main.go              entrypoint, wiring, graceful shutdown
internal/
  config/                           env var + config file parsing (viper)
  domain/                           entities, interfaces, error codes, transfer parsing
  api/                              HTTP handlers (chi), validation, ERC20 encoding
  pipeline/
    manager.go                      orchestrates pipelines, crash recovery
    pipeline.go                     per-(sender,chain) goroutine
    receipt.go                      batch receipt polling, reorg detection
    stuck.go                        stuck tx detection, gas bumping
  infrastructure/
    postgres/                       repository impl (pgx/v5), embedded migrations
    ethereum/
      client.go                     RPC pool with failover + health checks
      gas.go                        EIP-1559 / legacy gas estimation + bumping
      signer.go                     local ECDSA signer
  mocks/                            generated mocks (go.uber.org/mock)
```

## Design Assumptions

- **Single instance for pipelines.** Only one process runs the pipeline goroutines at a time. The API layer could be scaled horizontally, but pipeline coordination across instances is not implemented. This is acceptable for our scale target.
- **Trusted callers.** No authentication or authorization -- the service runs behind a private network or VPN. Callers are internal solver components.
- **Whitelist-only tokens.** The service refuses to interact with arbitrary contracts. Every supported ERC20 must be explicitly whitelisted per chain.
- **Private keys in env vars.** Good enough for now. HSM or KMS integration is a future improvement.
- **PostgreSQL is the bottleneck we can live with.** ACID guarantees on nonce tracking and transaction state are worth the throughput cost. At our scale (hundreds of TPS), Postgres handles it fine.
- **One sender EOA can only send on chains it's configured for.** There's no automatic fund bridging or cross-chain coordination.

## Tradeoffs Considered

### Sequential per-sender pipeline vs. parallel submission

We chose a dedicated goroutine per (sender, chain) that processes transactions one at a time. This eliminates nonce races entirely -- no distributed locks, no optimistic retry loops, no nonce gaps from concurrent senders. The cost is that throughput per sender per chain is limited to roughly one tx per block. For higher throughput, you add more sender addresses. This is the right tradeoff for a system where correctness matters more than raw throughput per address.

### Local nonce cursor vs. on-chain nonce query

We maintain a nonce counter in PostgreSQL and increment it atomically. On-chain `PendingNonceAt` is only used during startup for reconciliation. Querying on-chain nonce on every send is slow (RPC round trip) and unreliable (pending transactions may not be reflected immediately). The DB cursor is fast and deterministic. The risk is cursor drift after crashes, which we mitigate by reconciling on startup.

### Broadcast to all endpoints vs. single endpoint

For `sendRawTransaction`, we broadcast to ALL healthy RPC endpoints simultaneously. This maximizes mempool propagation and reduces the chance of a transaction being dropped. For reads, we use any single healthy endpoint. The cost is slightly higher RPC usage on sends, which is negligible.

### ACID over performance for financial state

Every nonce assignment, state transition, and attempt record goes through PostgreSQL transactions. We could get higher throughput with Redis or in-memory state, but losing a nonce assignment or state transition in a crash would mean lost funds or stuck pipelines. Not worth it.

### Separate attempts table vs. updating transaction in-place

Each gas bump creates a new `tx_attempts` row linked to the same transaction. This gives us a full history of every broadcast attempt with its gas parameters, which is essential for debugging stuck transactions and accounting for gas costs.

### Confirmation depth as a configurable parameter

Some chains (L2s like Arbitrum) have near-instant finality, so we set `confirmation_blocks=0` and go straight from SUBMITTED to CONFIRMED. For L1 Ethereum or chains with frequent reorgs, you configure a depth (e.g., 12 blocks) and the system uses the INCLUDED intermediate state with reorg detection. This avoids over-engineering for fast chains while supporting slow ones.

## Features

### Concurrency and Nonce Safety

Each (sender address, chain ID) pair gets a dedicated goroutine that claims and processes transactions sequentially. The goroutine IS the lock -- there's no shared nonce counter with mutex contention. Nonces are assigned from a PostgreSQL cursor with atomic `UPDATE ... RETURNING`. On startup, all PENDING transactions are reset to QUEUED and the nonce cursor is reconciled against the on-chain `PendingNonceAt`.

### Retry and Failure Strategies

- **Broadcast retries:** If `sendRawTransaction` fails, the pipeline retries up to 5 times with linear backoff before marking the transaction FAILED.
- **Stuck transaction detection:** A background worker scans for SUBMITTED transactions older than the configured threshold (default 3 minutes). It bumps gas by 15% and resubmits with the same nonce, up to 5 times.
- **Reorg detection:** For chains with `confirmation_blocks > 0`, the receipt poller re-fetches receipts for INCLUDED transactions. If the block hash changes or the receipt disappears, the transaction reverts to SUBMITTED and the raw transaction is re-broadcast.
- **Crash recovery:** On startup, the manager resets all PENDING transactions to QUEUED and reconciles nonce cursors. No transactions are lost.

### Gas Pricing Strategy

EIP-1559 is used by default with automatic fallback to legacy pricing on chains that don't support it (detected at startup by checking for `baseFee` in the latest block header).

Four priority tiers adjust the fee calculation:

| Priority | Tip Multiplier | Fee Multiplier |
|---|---|---|
| low | 0.8x | 1.5x base |
| normal | 1.0x | 2.0x base |
| high | 1.5x | 2.5x base |
| urgent | 2.5x | 3.0x base |

Gas limits include a configurable safety buffer (default 1.2x the estimate). Min/max fee caps are configurable per chain. Gas bumps for stuck transactions increase fees by 15% per bump, capped at 100% (double the original).

### Idempotency

Every transfer request requires a client-provided `idempotency_key`. If the same key is submitted with identical parameters, the original response is returned. If the key is reused with different parameters, the API returns `409 Conflict`. The key has a UNIQUE index in PostgreSQL.

### Persistence

PostgreSQL with four tables:
- `transactions` -- one row per logical transfer, tracks the full lifecycle
- `tx_attempts` -- each broadcast attempt (original + gas bumps) with gas params and raw signed tx
- `nonce_cursors` -- current nonce per (sender, chain)
- `tx_state_log` -- audit trail of every state transition

Schema migrations are embedded in the binary and run automatically on startup. All `uint256` values are stored as `NUMERIC(78,0)` -- never floating point.

### Observability

- **Structured JSON logging** via `log/slog` with component, chain_id, sender, tx_id, and tx_hash fields on every log line.
- **State audit trail** -- every transition (QUEUED->PENDING, PENDING->SUBMITTED, etc.) is recorded in `tx_state_log` with actor and reason. You can reconstruct the complete history of any transaction.
- **Health endpoint** at `/v1/health` reports database connectivity, RPC endpoint health per chain, and pipeline queue depths.
- **Swagger UI** at `/swagger/index.html` for API documentation.

### Security Considerations

- **No arbitrary calldata.** The API accepts transfer parameters (token, amount, recipient) and encodes ERC20 calldata internally. Callers cannot inject arbitrary contract calls.
- **Token whitelist.** Only explicitly configured ERC20s are allowed per chain. Unknown tokens are rejected with 403.
- **Per-token transfer limits.** Each token has a configurable max transfer amount. Exceeding it returns 422.
- **Address validation.** Zero address recipients, self-transfers, and transfers to the token contract itself are all rejected.
- **Key hygiene.** Private keys are loaded from env vars and immediately cleared from the process environment.
- **ERC20 transfer verification.** The receipt poller checks for a matching `Transfer` event log in the receipt to catch non-standard tokens that return `false` instead of reverting.

## Future Improvements

- **MEV protection** -- integrate with Flashbots Protect or similar private mempool services to prevent sandwich attacks on large transfers.
- **HSM / KMS integration** -- move private keys out of env vars into AWS KMS, GCP Cloud HSM, or HashiCorp Vault for production-grade key management.
- **Multi-instance pipeline coordination** -- use distributed locking (e.g., PostgreSQL advisory locks or Redis) to allow multiple instances to run pipelines. Currently single-instance only.
- **WebSocket subscriptions** -- push transaction status updates to clients instead of requiring polling.
- **Authentication and authorization** -- add API key or JWT auth for multi-tenant deployments.
- **Balance tracking** -- monitor sender EOA balances per token, reject transfers that would overdraft, and alert when balances are low.
- **Smart sender selection** -- when no sender is specified, automatically pick one with sufficient balance for the requested transfer.
- **Nonce + pending mark in a single DB transaction** -- currently nonce increment and marking pending are two separate DB calls; wrapping them in one transaction would be safer. (IMPLEMENTED)
- **Metrics export** -- Prometheus metrics for transaction throughput, gas costs, error rates, and queue depths.
- **Rate limiting** -- per-caller rate limits to prevent queue flooding.

## Development with Claude

This service was designed and implemented using Claude (Anthropic's AI assistant) as a pair programming partner. Here's how that worked in practice.

### The Process

We started with a requirements document (`REQUIREMENTS.md`) describing what we needed: a transaction sender that handles nonce management, gas estimation, retries, and receipt tracking across multiple EVM chains. The requirements were written from the perspective of "here's what I want the system to do" without prescribing implementation details.

From there, Claude and I iterated on the architecture. The big decisions -- the per-sender-per-chain pipeline model, the nonce cursor strategy, the gas bumping approach, the state machine -- were hashed out through conversation. I'd describe the problem space, Claude would propose an approach with tradeoffs, I'd push back on parts that didn't feel right, and we'd converge. The result was `ARCHITECTURE.md`, a comprehensive design document that became the blueprint.

Claude then generated the initial codebase structure and implementation, package by package. Each package was reviewed, discussed, and refined before moving to the next. Tests were written alongside the implementation, not after.

### CLAUDE.md as Living Context

`CLAUDE.md` serves as a persistent context document that helps Claude understand the project across sessions. It captures the architecture, package structure, conventions, and key decisions so that Claude doesn't need to re-learn the codebase from scratch every time. It's essentially a shared mental model between the human and the AI. Additionally, Claude maintains its own agent memory directory (`.claude/agent-memory/`) for notes about patterns, bugs fixed, and implementation details discovered during development.

### What Worked Well

- **Boilerplate and scaffolding.** Setting up Go project structure, interface definitions, mock generation, handler patterns, middleware -- Claude does this quickly and consistently.
- **Test coverage from the start.** Getting to 99 passing tests across the codebase was straightforward because Claude naturally generates tests alongside implementation. Table-driven tests, edge cases, mock setup -- it's all idiomatic.
- **Consistent patterns.** Error handling, logging, state transitions, and database operations follow the same patterns throughout the codebase. Claude is good at maintaining consistency once a pattern is established.
- **Comprehensive architecture documentation.** The `ARCHITECTURE.md` document is thorough and well-structured. Having an AI that can hold the entire system design in context and write detailed docs is genuinely useful.
- **Incremental feature addition.** Adding confirmation depth tracking, reorg detection, batch RPC calls, and chunked pagination was done incrementally, and Claude handled the ripple effects (interface changes, test updates, mock regeneration) across the codebase.

### What Needed Human Judgment

- **Deployment topology.** Where does this run? How many instances? What's the blast radius of a crash? These are operational questions that require understanding the broader infrastructure.
- **Security boundaries.** Deciding that the service runs behind a VPN with no auth (vs. implementing JWT or API keys) is a product/ops decision, not a code generation one.
- **Scale and cost tradeoffs.** Whether to use PostgreSQL vs. Redis, how many RPC endpoints to configure, what gas limits to set -- these require understanding the specific chains and economics involved.
- **Bug diagnosis from production behavior.** When `eth_estimateGas` was reverting because the `from` address was missing (zero address has no token balance), it took understanding the EVM execution model to diagnose. Claude fixed the code once the problem was identified, but spotting the root cause from the error message required domain knowledge.
- **Knowing when to stop.** Claude will happily add more features, more abstractions, more error handling. Knowing that "good enough for now" is the right answer for things like auth, metrics, and multi-instance coordination requires product judgment.

## Development

```bash
go build ./...          # build
go vet ./...            # lint
go test ./...           # run all tests (99 tests)
go generate ./...       # regenerate mocks
go mod tidy             # sync dependencies
```
