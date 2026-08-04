// Package trading defines the interface future auto-buy execution engines
// (Jupiter aggregator, direct Solana RPC transactions, etc.) will implement.
// It is intentionally decoupled from detector/telegram so a real execution
// backend can be plugged in later without touching alerting or UI code.
package trading

import "context"

// AutoBuyer executes a market buy of tokenAddress for approximately
// amountUSD worth of the quote asset. Implementations are responsible for
// slippage protection, gas/priority-fee handling, and their own timeouts —
// callers should still wrap calls with a context deadline.
type AutoBuyer interface {
	BuyToken(ctx context.Context, tokenAddress string, amountUSD float64) error
}

// NoopBuyer is a placeholder AutoBuyer that performs no on-chain action.
// Wire a real implementation (Jupiter swap, RPC transaction builder, etc.)
// in its place once ready; the rest of the codebase only depends on the
// AutoBuyer interface, so swapping implementations requires no other changes.
type NoopBuyer struct{}

// BuyToken always returns an error indicating no execution backend is wired.
func (NoopBuyer) BuyToken(ctx context.Context, tokenAddress string, amountUSD float64) error {
	return errNotImplemented{tokenAddress: tokenAddress}
}

type errNotImplemented struct {
	tokenAddress string
}

func (e errNotImplemented) Error() string {
	return "trading: auto-buy not implemented — wire a real AutoBuyer for " + e.tokenAddress
}
