package ethereum

import (
	"context"
	"fmt"
	"log/slog"
	"math/big"
	"sync"
	"sync/atomic"
	"time"

	"github.com/apm-dev/evm-tx-sender/internal/domain"
	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
)

type endpoint struct {
	url     string
	client  *ethclient.Client
	healthy atomic.Bool
}

// Client implements domain.EthClient with RPC pool and failover.
type Client struct {
	chainID   uint64
	endpoints []*endpoint
	mu        sync.RWMutex
	log       *slog.Logger
}

func NewClient(chainID uint64, rpcURLs []string, log *slog.Logger) (*Client, error) {
	if len(rpcURLs) == 0 {
		return nil, fmt.Errorf("no RPC URLs for chain %d", chainID)
	}

	c := &Client{
		chainID: chainID,
		log:     log.With("component", "eth-client", "chain_id", chainID),
	}

	for _, url := range rpcURLs {
		ec, err := ethclient.Dial(url)
		if err != nil {
			c.log.Warn("failed to connect to RPC", "url", url, "error", err)
			continue
		}
		ep := &endpoint{url: url, client: ec}
		ep.healthy.Store(true)
		c.endpoints = append(c.endpoints, ep)
	}

	if len(c.endpoints) == 0 {
		return nil, fmt.Errorf("no reachable RPC endpoints for chain %d", chainID)
	}

	return c, nil
}

func (c *Client) ChainID() uint64 { return c.chainID }

func (c *Client) Healthy() bool {
	for _, ep := range c.endpoints {
		if ep.healthy.Load() {
			return true
		}
	}
	return false
}

// getClient returns the first healthy endpoint client.
func (c *Client) getClient() (*ethclient.Client, error) {
	for _, ep := range c.endpoints {
		if ep.healthy.Load() {
			return ep.client, nil
		}
	}
	return nil, fmt.Errorf("no healthy RPC endpoints for chain %d", c.chainID)
}

func (c *Client) markUnhealthy(client *ethclient.Client, err error) {
	for _, ep := range c.endpoints {
		if ep.client == client {
			ep.healthy.Store(false)
			c.log.Warn("marking endpoint unhealthy", "url", ep.url, "error", err)
			// Start recovery goroutine
			go c.recoverEndpoint(ep)
			return
		}
	}
}

func (c *Client) recoverEndpoint(ep *endpoint) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, err := ep.client.BlockNumber(ctx)
		cancel()
		if err == nil {
			ep.healthy.Store(true)
			c.log.Info("endpoint recovered", "url", ep.url)
			return
		}
	}
}

func (c *Client) BlockNumber(ctx context.Context) (uint64, error) {
	client, err := c.getClient()
	if err != nil {
		return 0, err
	}
	n, err := client.BlockNumber(ctx)
	if err != nil {
		c.markUnhealthy(client, err)
		// Try next
		client2, err2 := c.getClient()
		if err2 != nil {
			return 0, err
		}
		return client2.BlockNumber(ctx)
	}
	return n, nil
}

func (c *Client) PendingNonceAt(ctx context.Context, addr common.Address) (uint64, error) {
	client, err := c.getClient()
	if err != nil {
		return 0, err
	}
	n, err := client.PendingNonceAt(ctx, addr)
	if err != nil {
		c.markUnhealthy(client, err)
		return 0, err
	}
	return n, nil
}

func (c *Client) NonceAt(ctx context.Context, addr common.Address) (uint64, error) {
	client, err := c.getClient()
	if err != nil {
		return 0, err
	}
	n, err := client.NonceAt(ctx, addr, nil)
	if err != nil {
		c.markUnhealthy(client, err)
		return 0, err
	}
	return n, nil
}

func (c *Client) SuggestGasPrice(ctx context.Context) (*big.Int, error) {
	client, err := c.getClient()
	if err != nil {
		return nil, err
	}
	p, err := client.SuggestGasPrice(ctx)
	if err != nil {
		c.markUnhealthy(client, err)
		return nil, err
	}
	return p, nil
}

func (c *Client) SuggestGasTipCap(ctx context.Context) (*big.Int, error) {
	client, err := c.getClient()
	if err != nil {
		return nil, err
	}
	tip, err := client.SuggestGasTipCap(ctx)
	if err != nil {
		// Some chains do not support this, fall back
		return nil, err
	}
	return tip, nil
}

func (c *Client) EstimateGas(ctx context.Context, from common.Address, to common.Address, data []byte, value *big.Int) (uint64, error) {
	client, err := c.getClient()
	if err != nil {
		return 0, err
	}
	msg := ethereum.CallMsg{
		From:  from,
		To:    &to,
		Data:  data,
		Value: value,
	}
	gas, err := client.EstimateGas(ctx, msg)
	if err != nil {
		return 0, err
	}
	return gas, nil
}

func (c *Client) LatestBaseFee(ctx context.Context) (*big.Int, error) {
	client, err := c.getClient()
	if err != nil {
		return nil, err
	}
	header, err := client.HeaderByNumber(ctx, nil)
	if err != nil {
		c.markUnhealthy(client, err)
		return nil, err
	}
	if header.BaseFee == nil {
		return nil, fmt.Errorf("chain %d does not support EIP-1559", c.chainID)
	}
	return header.BaseFee, nil
}

func (c *Client) SendTransaction(ctx context.Context, tx *types.Transaction) error {
	// Broadcast to ALL healthy endpoints simultaneously
	var wg sync.WaitGroup
	errs := make([]error, len(c.endpoints))

	for i, ep := range c.endpoints {
		if !ep.healthy.Load() {
			continue
		}
		wg.Add(1)
		go func(idx int, ep *endpoint) {
			defer wg.Done()
			errs[idx] = ep.client.SendTransaction(ctx, tx)
		}(i, ep)
	}
	wg.Wait()

	// Return success if ANY succeeded
	for _, err := range errs {
		if err == nil {
			return nil
		}
	}
	// Return the first error
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return fmt.Errorf("no healthy endpoints to broadcast")
}

func (c *Client) TransactionReceipt(ctx context.Context, txHash common.Hash) (*types.Receipt, error) {
	client, err := c.getClient()
	if err != nil {
		return nil, err
	}
	receipt, err := client.TransactionReceipt(ctx, txHash)
	if err != nil {
		return nil, err
	}
	return receipt, nil
}

func (c *Client) BatchTransactionReceipts(ctx context.Context, txHashes []common.Hash) (map[common.Hash]*types.Receipt, error) {
	if len(txHashes) == 0 {
		return make(map[common.Hash]*types.Receipt), nil
	}

	ec, err := c.getClient()
	if err != nil {
		return nil, err
	}

	// Access underlying rpc.Client via ethclient's Client() method
	rpcClient := ec.Client()

	elems := make([]rpc.BatchElem, len(txHashes))
	results := make([]*types.Receipt, len(txHashes))
	for i, hash := range txHashes {
		results[i] = new(types.Receipt)
		elems[i] = rpc.BatchElem{
			Method: "eth_getTransactionReceipt",
			Args:   []interface{}{hash},
			Result: results[i],
		}
	}

	if err := rpcClient.BatchCallContext(ctx, elems); err != nil {
		c.markUnhealthy(ec, err)
		return nil, err
	}

	receipts := make(map[common.Hash]*types.Receipt, len(txHashes))
	for i, elem := range elems {
		if elem.Error != nil {
			continue // individual receipt error, skip
		}
		// Check receipt is non-empty (null response means no receipt yet)
		if results[i] != nil && results[i].BlockNumber != nil {
			receipts[txHashes[i]] = results[i]
		}
	}
	return receipts, nil
}

func (c *Client) CodeAt(ctx context.Context, addr common.Address) ([]byte, error) {
	client, err := c.getClient()
	if err != nil {
		return nil, err
	}
	code, err := client.CodeAt(ctx, addr, nil)
	if err != nil {
		c.markUnhealthy(client, err)
		return nil, err
	}
	return code, nil
}

// StartHealthCheck runs periodic health checks on all endpoints.
func (c *Client) StartHealthCheck(ctx context.Context, interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				for _, ep := range c.endpoints {
					checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
					_, err := ep.client.BlockNumber(checkCtx)
					cancel()
					wasHealthy := ep.healthy.Load()
					ep.healthy.Store(err == nil)
					if wasHealthy && err != nil {
						c.log.Warn("endpoint became unhealthy", "url", ep.url, "error", err)
					} else if !wasHealthy && err == nil {
						c.log.Info("endpoint recovered", "url", ep.url)
					}
				}
			}
		}
	}()
}

// Ensure Client implements domain.EthClient.
var _ domain.EthClient = (*Client)(nil)
