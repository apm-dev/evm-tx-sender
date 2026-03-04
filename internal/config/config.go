package config

import (
	"crypto/ecdsa"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/apm-dev/evm-tx-sender/internal/domain"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/spf13/viper"
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

// --- viper helpers -----------------------------------------------------------

func viperGetOrDefault(key, defaultValue string) string {
	viper.SetDefault(key, defaultValue)
	return viper.GetString(key)
}

func viperGetOrDefaultInt(key string, defaultValue int) int {
	viper.SetDefault(key, defaultValue)
	return viper.GetInt(key)
}

func viperGetOrDefaultFloat(key string, defaultValue float64) float64 {
	viper.SetDefault(key, defaultValue)
	return viper.GetFloat64(key)
}

func viperGetOrDefaultDuration(key string, defaultValue time.Duration) time.Duration {
	viper.SetDefault(key, defaultValue.String())
	s := viper.GetString(key)
	d, err := time.ParseDuration(s)
	if err != nil {
		return defaultValue
	}
	return d
}

// --- parse() methods ---------------------------------------------------------

func (c *ServerConfig) parse() {
	c.Host = viperGetOrDefault("server.host", "0.0.0.0")
	c.Port = viperGetOrDefaultInt("server.port", 8080)
	c.ReadTimeout = viperGetOrDefaultDuration("server.read-timeout", 10*time.Second)
	c.WriteTimeout = viperGetOrDefaultDuration("server.write-timeout", 30*time.Second)
	c.ShutdownTimeout = viperGetOrDefaultDuration("server.shutdown-timeout", 60*time.Second)
}

func (c *DatabaseConfig) parse() {
	c.URL = viperGetOrDefault("database.url", "")
	c.MaxConns = int32(viperGetOrDefaultInt("database.max-conns", 25))
	c.MinConns = int32(viperGetOrDefaultInt("database.min-conns", 5))
	c.MaxConnLifetime = viperGetOrDefaultDuration("database.max-conn-lifetime", 1*time.Hour)
}

// --- Load --------------------------------------------------------------------

// Load reads configuration from an optional config file, environment variables,
// and built-in defaults. Environment variables take precedence over file values.
// The config file path defaults to ./config.yml and can be overridden with the
// CONFIG_PATH env var.
func Load() (*Config, error) {
	cfg := &Config{
		Chains: make(map[uint64]*domain.ChainConfig),
	}

	viper.AutomaticEnv()
	viper.SetEnvKeyReplacer(strings.NewReplacer(".", "_", "-", "_"))

	viper.SetDefault("config-path", "./config.yml")
	configPath := viper.GetString("config-path")
	if _, err := os.Stat(configPath); err == nil {
		ext := filepath.Ext(configPath)
		if len(ext) > 1 {
			viper.SetConfigType(ext[1:])
		}
		viper.SetConfigFile(configPath)
		if err := viper.ReadInConfig(); err != nil {
			return nil, fmt.Errorf("reading config file: %w", err)
		}
	}

	cfg.Server.parse()
	cfg.Database.parse()

	if cfg.Database.URL == "" {
		return nil, fmt.Errorf("database.url is required")
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

// --- chain loading -----------------------------------------------------------

func (c *Config) loadChains() error {
	chainMap := viper.GetStringMap("chains")
	for idStr := range chainMap {
		chainID, err := strconv.ParseUint(idStr, 10, 64)
		if err != nil {
			return fmt.Errorf("invalid chain ID %q in config: %w", idStr, err)
		}
		chain, err := parseChain(chainID)
		if err != nil {
			return fmt.Errorf("chain %d: %w", chainID, err)
		}
		c.Chains[chainID] = chain
	}
	return nil
}

func parseChain(chainID uint64) (*domain.ChainConfig, error) {
	p := fmt.Sprintf("chains.%d", chainID)

	chain := &domain.ChainConfig{
		ChainID:             chainID,
		SupportsEIP1559:     true,
		TokenWhitelist:      make(map[string]domain.TokenConfig),
		Name:                viperGetOrDefault(p+".name", fmt.Sprintf("chain-%d", chainID)),
		BlockTime:           viperGetOrDefaultDuration(p+".block-time", 12*time.Second),
		ConfirmationBlocks:  viperGetOrDefaultInt(p+".confirmation-blocks", 12),
		GasLimitMultiplier:  viperGetOrDefaultFloat(p+".gas-limit-multiplier", 1.2),
		ReceiptChunkSize:    viperGetOrDefaultInt(p+".receipt-chunk-size", 50),
		StuckThreshold:      viperGetOrDefaultDuration(p+".stuck-threshold", 180*time.Second),
		GasBumpInterval:     viperGetOrDefaultDuration(p+".gas-bump-interval", 30*time.Second),
		NativeTokenSymbol:   viperGetOrDefault(p+".native-symbol", "ETH"),
		NativeTokenDecimals: uint8(viperGetOrDefaultInt(p+".native-decimals", 18)),
	}

	rpcURLs := viper.GetStringSlice(p + ".rpc-urls")
	if len(rpcURLs) == 0 {
		return nil, fmt.Errorf("rpc-urls required")
	}
	chain.RPCEndpoints = rpcURLs

	chain.MinMaxFee = bigIntFromViper(p + ".min-max-fee")
	chain.MaxMaxFee = bigIntFromViper(p + ".max-max-fee")
	chain.MinPriorityFee = bigIntFromViper(p + ".min-priority-fee")
	chain.MaxPriorityFee = bigIntFromViper(p + ".max-priority-fee")
	chain.NativeMaxTransfer = bigIntFromViper(p + ".native-max-transfer")

	if err := parseTokens(chain, p+".tokens"); err != nil {
		return nil, err
	}

	return chain, nil
}

func parseTokens(chain *domain.ChainConfig, prefix string) error {
	tokenMap := viper.GetStringMap(prefix)
	for symbol := range tokenMap {
		symbol = strings.ToUpper(symbol)
		tp := fmt.Sprintf("%s.%s", prefix, strings.ToLower(symbol))

		if symbol == strings.ToUpper(chain.NativeTokenSymbol) {
			return fmt.Errorf("token %s: symbol conflicts with native token", symbol)
		}

		addrStr := viper.GetString(tp + ".address")
		if addrStr == "" || !common.IsHexAddress(addrStr) {
			return fmt.Errorf("token %s: invalid address %q", symbol, addrStr)
		}

		tc := domain.TokenConfig{
			Symbol:          symbol,
			ContractAddress: common.HexToAddress(addrStr),
			Decimals:        uint8(viperGetOrDefaultInt(tp+".decimals", 18)),
			Enabled:         viperGetOrDefaultBool(tp+".enabled", true),
			MaxTransfer:     bigIntFromViper(tp + ".max-transfer"),
		}
		chain.TokenWhitelist[symbol] = tc
	}
	return nil
}

// --- big.Int helper ----------------------------------------------------------

func bigIntFromViper(key string) *big.Int {
	s := viper.GetString(key)
	if s == "" {
		return nil
	}
	n, ok := new(big.Int).SetString(s, 10)
	if !ok {
		return nil
	}
	return n
}

// viperGetOrDefaultBool is used only internally; not part of the public helper set.
func viperGetOrDefaultBool(key string, defaultValue bool) bool {
	viper.SetDefault(key, defaultValue)
	return viper.GetBool(key)
}

// --- private key loading -----------------------------------------------------

// loadKeys reads TX_SENDER_KEYS_<ADDRESS>=<hex-private-key> env vars.
// Keys are env-var-only and never loaded from the config file.
// Each env var is cleared from the process environment after loading.
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

		hexKey := strings.TrimPrefix(parts[1], "0x")

		privateKey, err := crypto.HexToECDSA(hexKey)
		if err != nil {
			return nil, fmt.Errorf("invalid private key for %s: %w", addrStr, err)
		}

		// Derive address and verify it matches the env var name.
		derived := crypto.PubkeyToAddress(privateKey.PublicKey)
		expected := common.HexToAddress(addrStr)
		if derived != expected {
			return nil, fmt.Errorf("key address mismatch: env says %s, derived %s", expected.Hex(), derived.Hex())
		}

		keys[derived] = privateKey
		os.Unsetenv(key)
	}

	return keys, nil
}
