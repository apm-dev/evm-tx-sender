package ethereum

import (
	"context"
	"crypto/ecdsa"
	"fmt"
	"math/big"
	"sync"

	"github.com/apm-dev/evm-tx-sender/internal/domain"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

type LocalSigner struct {
	mu   sync.RWMutex
	keys map[common.Address]*ecdsa.PrivateKey
}

func NewLocalSigner(keys map[common.Address]*ecdsa.PrivateKey) *LocalSigner {
	return &LocalSigner{keys: keys}
}

func (s *LocalSigner) Sign(_ context.Context, sender common.Address, tx *types.Transaction, chainID *big.Int) (*types.Transaction, error) {
	s.mu.RLock()
	key, ok := s.keys[sender]
	s.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("no key for address %s", sender.Hex())
	}

	signer := types.LatestSignerForChainID(chainID)
	signed, err := types.SignTx(tx, signer, key)
	if err != nil {
		return nil, fmt.Errorf("signing failed: %w", err)
	}
	return signed, nil
}

func (s *LocalSigner) HasAddress(addr common.Address) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.keys[addr]
	return ok
}

func (s *LocalSigner) Addresses() []common.Address {
	s.mu.RLock()
	defer s.mu.RUnlock()
	addrs := make([]common.Address, 0, len(s.keys))
	for addr := range s.keys {
		addrs = append(addrs, addr)
	}
	return addrs
}

// DeriveAddress derives the Ethereum address from a private key for validation.
func DeriveAddress(key *ecdsa.PrivateKey) common.Address {
	return crypto.PubkeyToAddress(key.PublicKey)
}

var _ domain.Signer = (*LocalSigner)(nil)
