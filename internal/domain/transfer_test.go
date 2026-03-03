package domain

import (
	"math/big"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseAmountToSmallestUnit(t *testing.T) {
	tests := []struct {
		name     string
		amount   string
		decimals uint8
		want     string
		wantErr  bool
	}{
		{
			name:     "whole ETH 18 decimals",
			amount:   "1",
			decimals: 18,
			want:     "1000000000000000000",
		},
		{
			name:     "fractional ETH 18 decimals",
			amount:   "1.5",
			decimals: 18,
			want:     "1500000000000000000",
		},
		{
			name:     "USDC 6 decimals",
			amount:   "1500.50",
			decimals: 6,
			want:     "1500500000",
		},
		{
			name:     "zero decimals token",
			amount:   "42",
			decimals: 0,
			want:     "42",
		},
		{
			name:     "zero decimals rejects fractional",
			amount:   "42.5",
			decimals: 0,
			wantErr:  true,
		},
		{
			name:     "fewer decimal places than token supports",
			amount:   "1.5",
			decimals: 6,
			want:     "1500000",
		},
		{
			name:     "exact decimal places",
			amount:   "1.123456",
			decimals: 6,
			want:     "1123456",
		},
		{
			name:     "too many decimal places",
			amount:   "1.1234567",
			decimals: 6,
			wantErr:  true,
		},
		{
			name:     "zero amount",
			amount:   "0",
			decimals: 18,
			want:     "0",
		},
		{
			name:     "zero with decimals",
			amount:   "0.0",
			decimals: 18,
			want:     "0",
		},
		{
			name:     "empty string",
			amount:   "",
			decimals: 18,
			wantErr:  true,
		},
		{
			name:     "negative amount",
			amount:   "-1.5",
			decimals: 18,
			wantErr:  true,
		},
		{
			name:     "large whole number",
			amount:   "999999999999999999",
			decimals: 18,
			want:     "999999999999999999000000000000000000",
		},
		{
			name:     "only fractional part",
			amount:   ".5",
			decimals: 6,
			want:     "500000",
		},
		{
			name:     "whitespace trimming",
			amount:   "  1.5  ",
			decimals: 6,
			want:     "1500000",
		},
		{
			name:     "all zeros fractional",
			amount:   "1.000000",
			decimals: 6,
			want:     "1000000",
		},
		{
			name:     "non-numeric input",
			amount:   "abc",
			decimals: 18,
			wantErr:  true,
		},
		{
			name:     "single digit sub-unit",
			amount:   "0.000001",
			decimals: 6,
			want:     "1",
		},
		{
			name:     "18 decimal precision full",
			amount:   "0.000000000000000001",
			decimals: 18,
			want:     "1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := ParseAmountToSmallestUnit(tt.amount, tt.decimals)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			expected, ok := new(big.Int).SetString(tt.want, 10)
			require.True(t, ok, "invalid expected value: %s", tt.want)
			assert.Equal(t, 0, result.Cmp(expected), "got %s, want %s", result.String(), expected.String())
		})
	}
}

func TestTxStatus_IsTerminal(t *testing.T) {
	tests := []struct {
		status   TxStatus
		terminal bool
	}{
		{TxStatusQueued, false},
		{TxStatusPending, false},
		{TxStatusSubmitted, false},
		{TxStatusIncluded, false},
		{TxStatusConfirmed, true},
		{TxStatusReverted, true},
		{TxStatusFailed, true},
	}

	for _, tt := range tests {
		t.Run(string(tt.status), func(t *testing.T) {
			assert.Equal(t, tt.terminal, tt.status.IsTerminal())
		})
	}
}

func TestParsePriority(t *testing.T) {
	tests := []struct {
		input string
		want  Priority
		ok    bool
	}{
		{"low", PriorityLow, true},
		{"normal", PriorityNormal, true},
		{"high", PriorityHigh, true},
		{"urgent", PriorityUrgent, true},
		{"NORMAL", "", false},
		{"", "", false},
		{"invalid", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, ok := ParsePriority(tt.input)
			assert.Equal(t, tt.ok, ok)
			if ok {
				assert.Equal(t, tt.want, got)
			}
		})
	}
}
