package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/apm-dev/evm-tx-sender/internal/api"
	"github.com/apm-dev/evm-tx-sender/internal/config"
	"github.com/apm-dev/evm-tx-sender/internal/domain"
	"github.com/apm-dev/evm-tx-sender/internal/infrastructure/ethereum"
	"github.com/apm-dev/evm-tx-sender/internal/infrastructure/postgres"
	"github.com/apm-dev/evm-tx-sender/internal/pipeline"
	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	if err := run(log); err != nil {
		log.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Load configuration
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	log.Info("configuration loaded",
		"chains", len(cfg.Chains),
		"signers", len(cfg.Keys),
	)

	// Connect to database
	poolCfg, err := pgxpool.ParseConfig(cfg.Database.URL)
	if err != nil {
		return fmt.Errorf("parse database URL: %w", err)
	}
	poolCfg.MaxConns = cfg.Database.MaxConns
	poolCfg.MinConns = cfg.Database.MinConns
	poolCfg.MaxConnLifetime = cfg.Database.MaxConnLifetime

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return fmt.Errorf("database connect: %w", err)
	}
	defer pool.Close()

	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("database ping: %w", err)
	}
	log.Info("database connected")

	// Run migrations
	repo := postgres.NewRepo(pool)
	if err := repo.Migrate(ctx); err != nil {
		return fmt.Errorf("migrations: %w", err)
	}
	log.Info("migrations applied")

	// Initialize Ethereum clients
	clients := make(map[uint64]domain.EthClient)
	for chainID, chain := range cfg.Chains {
		client, err := ethereum.NewClient(chainID, chain.RPCEndpoints, log)
		if err != nil {
			return fmt.Errorf("eth client chain %d: %w", chainID, err)
		}
		client.StartHealthCheck(ctx, 30*time.Second)
		clients[chainID] = client
		log.Info("eth client initialized", "chain_id", chainID, "endpoints", len(chain.RPCEndpoints))
	}

	// Auto-detect EIP-1559 support
	for chainID, client := range clients {
		chain := cfg.Chains[chainID]
		_, err := client.LatestBaseFee(ctx)
		if err != nil {
			chain.SupportsEIP1559 = false
			log.Info("chain does not support EIP-1559, using legacy gas", "chain_id", chainID)
		} else {
			chain.SupportsEIP1559 = true
		}
	}

	// Initialize signer
	signer := ethereum.NewLocalSigner(cfg.Keys)
	log.Info("signer initialized", "addresses", len(signer.Addresses()))

	// Initialize gas engine
	gasEngine := ethereum.NewGasEngine()

	// Initialize pipeline manager
	mgr := pipeline.NewManager(repo, clients, signer, gasEngine, cfg.Chains, log)

	// Initialize nonces (crash recovery)
	if err := mgr.InitNonces(ctx); err != nil {
		return fmt.Errorf("init nonces: %w", err)
	}
	log.Info("nonces initialized")

	// Start pipelines and background workers
	mgr.Start(ctx)
	log.Info("pipelines started")

	// Initialize HTTP router
	router := api.NewRouter(repo, signer, cfg.Chains, clients, mgr, pool, log)

	// Start HTTP server
	addr := fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.Port)
	srv := &http.Server{
		Addr:         addr,
		Handler:      router,
		ReadTimeout:  cfg.Server.ReadTimeout,
		WriteTimeout: cfg.Server.WriteTimeout,
	}

	// Graceful shutdown
	errCh := make(chan error, 1)
	go func() {
		log.Info("HTTP server starting", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return fmt.Errorf("server: %w", err)
	case <-ctx.Done():
		log.Info("shutdown signal received")
	}

	// Graceful shutdown
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout)
	defer shutdownCancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("server shutdown: %w", err)
	}

	log.Info("server stopped gracefully")
	return nil
}
