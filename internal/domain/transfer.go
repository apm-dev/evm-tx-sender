package domain

import (
	"fmt"
	"math/big"
	"strings"
)

var MaxUint256, _ = new(big.Int).SetString(
	"115792089237316195423570985008687907853269984665640564039457584007913129639935", 10,
)

// ParseAmountToSmallestUnit converts a decimal amount string (e.g. "1500.50")
// to the smallest unit for a given number of decimals (e.g. 1500500000 for 6 decimals).
// Uses string arithmetic -- never float64.
func ParseAmountToSmallestUnit(amount string, decimals uint8) (*big.Int, error) {
	if amount == "" {
		return nil, fmt.Errorf("empty amount")
	}

	// Remove leading/trailing whitespace
	amount = strings.TrimSpace(amount)

	// Check for negative
	if strings.HasPrefix(amount, "-") {
		return nil, fmt.Errorf("negative amount")
	}

	parts := strings.SplitN(amount, ".", 2)
	intPart := parts[0]
	fracPart := ""
	if len(parts) == 2 {
		fracPart = parts[1]
	}

	if intPart == "" {
		intPart = "0"
	}

	// Reject if fractional part has more digits than token decimals
	if len(fracPart) > int(decimals) {
		return nil, fmt.Errorf("amount has %d decimal places, token supports %d", len(fracPart), decimals)
	}

	// Pad fractional part to exactly `decimals` digits
	fracPart = fracPart + strings.Repeat("0", int(decimals)-len(fracPart))

	combined := intPart + fracPart

	// Strip leading zeros to avoid issues, but keep at least "0"
	combined = strings.TrimLeft(combined, "0")
	if combined == "" {
		combined = "0"
	}

	result, ok := new(big.Int).SetString(combined, 10)
	if !ok {
		return nil, fmt.Errorf("invalid numeric amount: %s", amount)
	}

	return result, nil
}
