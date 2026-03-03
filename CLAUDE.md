# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Build & Test Commands

```bash
go build ./...                          # Build all packages
go vet ./...                            # Static analysis
go test ./...                           # Run all tests
go test ./internal/pipeline/ -run TestPipeline_ProcessTx  # Run single test
go test ./internal/api/ -v              # Verbose output for one package
go generate ./internal/domain/          # Regenerate mocks (requires mockgen)
go mod tidy                             # Sync dependencies
```

No Makefile, Dockerfile, or CI config exists yet. The service requires a running PostgreSQL instance and EVM RPC endpoints to run (not needed for tests — all external dependencies are mocked).

## Architecture

**EVM transaction sender microservice** for solver infrastructure. Sends token transfers (native + ERC20) across multiple EVM chains with correct nonce management, gas optimization, and retry logic.

### Core Design: Per-Sender-Per-Chain Serial Pipeline

The hardest problem is nonce management under concurrency. The solution: each `(EOA address, chain ID)` pair gets a dedicated goroutine (`Pipeline`) that processes transactions **sequentially**. This eliminates nonce races by construction — no locks needed.

```
POST /v1/transfers → DB (QUEUED) → Pipeline goroutine → Nonce → Gas → Sign → Broadcast → Receipt
```

### Package Dependency Flow

```
cmd/tx-sender/main.go          ← wires everything, graceful shutdown
  ├─ internal/config            ← env var parsing (TX_SENDER_* prefix)
  ├─ internal/domain            ← entities, interfaces (Repository, EthClient, Signer), error codes
  ├─ internal/infrastructure/
  │   ├─ postgres/repo.go       ← Repository implementation (pgx/v5)
  │   └─ ethereum/              ← RPC pool with failover, GasEngine, LocalSigner
  ├─ internal/pipeline/
  │   ├─ manager.go             ← orchestrates pipelines, crash recovery, nonce init
  │   ├─ pipeline.go            ← per-(sender,chain) goroutine: claim → nonce → gas → sign → broadcast
  │   ├─ receipt.go             ← polls chain for receipts (SUBMITTED → CONFIRMED/REVERTED)
  │   └─ stuck.go               ← detects stuck txs, bumps gas, resubmits (up to 5 bumps)
  └─ internal/api/              ← HTTP handlers (chi router), validation, ERC20 calldata encoding
```

### Transaction State Machine

`QUEUED → PENDING → SUBMITTED → CONFIRMED | REVERTED | FAILED`

- **QUEUED**: created, waiting for pipeline to claim
- **PENDING**: nonce assigned, being processed
- **SUBMITTED**: broadcast to chain, awaiting receipt
- Terminal states: **CONFIRMED** (receipt status=1), **REVERTED** (status=0), **FAILED** (error before/during broadcast)

### Key Interfaces (internal/domain/interfaces.go)

All external dependencies are behind interfaces with `//go:generate mockgen` directives. Mocks live in `internal/mocks/`. Three core interfaces:
- **Repository** — all DB operations (transactions, attempts, nonce cursors, state log)
- **EthClient** — all RPC operations (nonce, gas estimation, send, receipts)
- **Signer** — transaction signing, key management

### Testing Conventions

- Tests use **external test packages** (`package api_test`, `package pipeline_test`, etc.) — black-box testing only
- `internal/pipeline/export_test.go` bridges unexported symbols for external test packages (Go stdlib pattern)
- Mocks generated with `go.uber.org/mock/mockgen`
- Assertions via `testify/assert` and `testify/require`
- Table-driven tests with `t.Run` subtests

### Database

PostgreSQL 15+ with pgx/v5. Schema in `internal/infrastructure/postgres/migrations/001_init.sql`. Four tables: `transactions`, `tx_attempts`, `nonce_cursors`, `tx_state_log`. Migrations are embedded and run at startup.

### Gas Strategy

EIP-1559 by default with auto-detection fallback to legacy. Four priority tiers (low/normal/high/urgent) apply different tip and fee multipliers. Gas bumping for stuck transactions: 15% increments per bump, capped at 100% (double).

### Crash Recovery

On startup, `Manager.InitNonces()` resets all PENDING transactions back to QUEUED and reconciles DB nonce cursors with on-chain `PendingNonceAt`.

## Key Reference

- **ARCHITECTURE.md** — comprehensive 18-section architecture document covering all design decisions
- **REQUIREMENTS.md** — original project specification
- **refactors.md** — tracked future improvements
