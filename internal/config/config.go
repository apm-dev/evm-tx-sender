package config

import (
	"crypto/ecdsa"
	"fmt"
	"math/big"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/apm-dev/evm-tx-sender/internal/domain"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

type Config struct {
	Server   ServerConfig
	Database DatabaseConfig
	Chains   map[uint64]*domain.ChainConfig
	Keys     map[common.Address]*ecdsa.PrivateKey
}

type ServerConfig struct {
	Host            string
	Port            int
	ReadTimeout     time.Duration
	WriteTimeout    time.Duration
	ShutdownTimeout time.Duration
}

type DatabaseConfig struct {
	URL             string
	MaxConns        int32
	MinConns        int32
	MaxConnLifetime time.Duration
}

func Load() (*Config, error) {
	cfg := &Config{
		Server: ServerConfig{
			Host:            envOrDefault("TX_SENDER_HOST", "0.0.0.0"),
			Port:            envIntOrDefault("TX_SENDER_PORT", 8080),
			ReadTimeout:     envDurationOrDefault("TX_SENDER_READ_TIMEOUT", 10*time.Second),
			WriteTimeout:    envDurationOrDefault("TX_SENDER_WRITE_TIMEOUT", 30*time.Second),
			ShutdownTimeout: envDurationOrDefault("TX_SENDER_SHUTDOWN_TIMEOUT", 60*time.Second),
		},
		Database: DatabaseConfig{
			URL:             os.Getenv("TX_SENDER_DATABASE_URL"),
			MaxConns:        int32(envIntOrDefault("TX_SENDER_DB_MAX_CONNS", 25)),
			MinConns:        int32(envIntOrDefault("TX_SENDER_DB_MIN_CONNS", 5)),
			MaxConnLifetime: envDurationOrDefault("TX_SENDER_DB_MAX_CONN_LIFETIME", time.Hour),
		},
		Chains: make(map[uint64]*domain.ChainConfig),
	}

	if cfg.Database.URL == "" {
		return nil, fmt.Errorf("TX_SENDER_DATABASE_URL is required")
	}

	if err := cfg.loadChains(); err != nil {
		return nil, fmt.Errorf("loading chains: %w", err)
	}

	if len(cfg.Chains) == 0 {
		return nil, fmt.Errorf("no chains configured")
	}

	keys, err := loadKeys()
	if err != nil {
		return nil, fmt.Errorf("loading keys: %w", err)
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("no signer keys configured")
	}
	cfg.Keys = keys

	return cfg, nil
}

func (c *Config) loadChains() error {
	// Discover chains from TX_SENDER_CHAIN_<ID>_RPC_URLS
	for _, env := range os.Environ() {
		parts := strings.SplitN(env, "=", 2)
		key := parts[0]
		if !strings.HasPrefix(key, "TX_SENDER_CHAIN_") || !strings.HasSuffix(key, "_RPC_URLS") {
			continue
		}

		chainStr := strings.TrimPrefix(key, "TX_SENDER_CHAIN_")
		chainStr = strings.TrimSuffix(chainStr, "_RPC_URLS")
		chainID, err := strconv.ParseUint(chainStr, 10, 64)
		if err != nil {
			continue
		}

		chain, err := loadChainConfig(chainID)
		if err != nil {
			return fmt.Errorf("chain %d: %w", chainID, err)
		}
		c.Chains[chainID] = chain
	}
	return nil
}

func loadChainConfig(chainID uint64) (*domain.ChainConfig, error) {
	prefix := fmt.Sprintf("TX_SENDER_CHAIN_%d_", chainID)

	rpcURLs := os.Getenv(prefix + "RPC_URLS")
	if rpcURLs == "" {
		return nil, fmt.Errorf("RPC_URLS required")
	}

	chain := &domain.ChainConfig{
		ChainID:             chainID,
		Name:                envOrDefault(prefix+"NAME", fmt.Sprintf("chain-%d", chainID)),
		RPCEndpoints:        splitCSV(rpcURLs),
		BlockTime:           envDurationOrDefault(prefix+"BLOCK_TIME", 12*time.Second),
		ConfirmationBlocks:  envIntOrDefault(prefix+"CONFIRMATION_BLOCKS", 12),
		SupportsEIP1559:     true,
		GasLimitMultiplier:  envFloatOrDefault(prefix+"GAS_LIMIT_MULTIPLIER", 1.2),
		StuckThreshold:      envDurationOrDefault(prefix+"STUCK_THRESHOLD", 180*time.Second),
		GasBumpInterval:     envDurationOrDefault(prefix+"GAS_BUMP_INTERVAL", 30*time.Second),
		NativeTokenSymbol:   envOrDefault(prefix+"NATIVE_SYMBOL", "ETH"),
		NativeTokenDecimals: uint8(envIntOrDefault(prefix+"NATIVE_DECIMALS", 18)),
		TokenWhitelist:      make(map[string]domain.TokenConfig),
	}

	// Min/max gas bounds
	if v := os.Getenv(prefix + "MIN_MAX_FEE"); v != "" {
		chain.MinMaxFee, _ = new(big.Int).SetString(v, 10)
	}
	if v := os.Getenv(prefix + "MAX_MAX_FEE"); v != "" {
		chain.MaxMaxFee, _ = new(big.Int).SetString(v, 10)
	}
	if v := os.Getenv(prefix + "MIN_PRIORITY_FEE"); v != "" {
		chain.MinPriorityFee, _ = new(big.Int).SetString(v, 10)
	}
	if v := os.Getenv(prefix + "MAX_PRIORITY_FEE"); v != "" {
		chain.MaxPriorityFee, _ = new(big.Int).SetString(v, 10)
	}
	if v := os.Getenv(prefix + "NATIVE_MAX_TRANSFER"); v != "" {
		chain.NativeMaxTransfer, _ = new(big.Int).SetString(v, 10)
	}

	// Load token whitelist: TX_SENDER_CHAIN_<ID>_TOKEN_<SYMBOL>_ADDRESS
	if err := loadTokenWhitelist(chain, prefix); err != nil {
		return chain, err
	}

	return chain, nil
}

func loadTokenWhitelist(chain *domain.ChainConfig, prefix string) error {
	// Scan for TX_SENDER_CHAIN_<ID>_TOKEN_<SYMBOL>_ADDRESS
	tokenPrefix := prefix + "TOKEN_"
	seen := make(map[string]bool)

	for _, env := range os.Environ() {
		parts := strings.SplitN(env, "=", 2)
		key := parts[0]
		if !strings.HasPrefix(key, tokenPrefix) || !strings.HasSuffix(key, "_ADDRESS") {
			continue
		}

		// Extract symbol
		rest := strings.TrimPrefix(key, tokenPrefix)
		symbol := strings.TrimSuffix(rest, "_ADDRESS")
		symbol = strings.ToUpper(symbol)

		if seen[symbol] {
			continue
		}
		seen[symbol] = true

		symPrefix := tokenPrefix + symbol + "_"
		addrStr := os.Getenv(symPrefix + "ADDRESS")
		if addrStr == "" || !common.IsHexAddress(addrStr) {
			return fmt.Errorf("token %s: invalid address %q", symbol, addrStr)
		}

		decimals := envIntOrDefault(symPrefix+"DECIMALS", 18)
		enabled := envOrDefault(symPrefix+"ENABLED", "true") == "true"

		tc := domain.TokenConfig{
			Symbol:          symbol,
			ContractAddress: common.HexToAddress(addrStr),
			Decimals:        uint8(decimals),
			Enabled:         enabled,
		}

		if v := os.Getenv(symPrefix + "MAX_TRANSFER"); v != "" {
			tc.MaxTransfer, _ = new(big.Int).SetString(v, 10)
		}

		// Validate: symbol must not collide with native token
		if symbol == strings.ToUpper(chain.NativeTokenSymbol) {
			return fmt.Errorf("token %s: symbol conflicts with native token", symbol)
		}

		chain.TokenWhitelist[symbol] = tc
	}
	return nil
}

func loadKeys() (map[common.Address]*ecdsa.PrivateKey, error) {
	keys := make(map[common.Address]*ecdsa.PrivateKey)

	for _, env := range os.Environ() {
		parts := strings.SplitN(env, "=", 2)
		key := parts[0]
		if !strings.HasPrefix(key, "TX_SENDER_KEYS_") {
			continue
		}

		addrStr := strings.TrimPrefix(key, "TX_SENDER_KEYS_")
		if !common.IsHexAddress(addrStr) {
			return nil, fmt.Errorf("invalid address in key env var: %s", addrStr)
		}

		hexKey := parts[1]
		hexKey = strings.TrimPrefix(hexKey, "0x")

		privateKey, err := crypto.HexToECDSA(hexKey)
		if err != nil {
			return nil, fmt.Errorf("invalid private key for %s: %w", addrStr, err)
		}

		// Derive address and verify it matches the env var name
		derived := crypto.PubkeyToAddress(privateKey.PublicKey)
		expected := common.HexToAddress(addrStr)
		if derived != expected {
			return nil, fmt.Errorf("key address mismatch: env says %s, derived %s", expected.Hex(), derived.Hex())
		}

		keys[derived] = privateKey

		// Clear the env var after loading
		os.Unsetenv(key)
	}

	return keys, nil
}

// Helpers

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envIntOrDefault(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			return i
		}
	}
	return def
}

func envDurationOrDefault(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

func envFloatOrDefault(key string, def float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}

func splitCSV(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
