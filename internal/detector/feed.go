package detector

import (
	"context"
	"fmt"
	"math/rand"
	"time"
)

var sampleTokens = []struct {
	Symbol  string
	Address string
	Chain   string
}{
	{"PEPE", "0x6982508145454Ce325ddBe47a25d4ec3d2311933", "Ethereum"},
	{"WIF", "0x3ek982508145454Ce325ddBe47a25d4ec3d2311933", "Solana"},
	{"BONK", "0xDeZXRAZ8z7P8n6P3Vv7o2b3c4d5e6f7g8h9i0j1k2l3m", "Solana"},
	{"SHIB", "0x95aD61b0a150d79219dcf64E1E6Cc01f0B64C4cE", "Ethereum"},
	{"DOGE", "0x4200000000000000000000000000000000000006", "Base"},
}

// StartMockFeed starts a background worker that generates realistic Swap events into the ClusterEngine.
func StartMockFeed(ctx context.Context, engine *ClusterEngine, interval time.Duration) {
	ticker := time.NewTicker(interval)
	go func() {
		defer ticker.Stop()
		r := rand.New(rand.NewSource(time.Now().UnixNano()))

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				// Pick a random token
				tok := sampleTokens[r.Intn(len(sampleTokens))]

				// Generate random wallet and amount
				wallet := fmt.Sprintf("0xWallet%d", r.Intn(10))
				amount := 12000.0 + r.Float64()*38000.0 // $12,000 to $50,000
				txHash := fmt.Sprintf("0xTx%d%d", r.Int63(), r.Int63())

				event := SwapEvent{
					TokenAddress:  tok.Address,
					TokenSymbol:   tok.Symbol,
					WalletAddress: wallet,
					AmountUSD:     amount,
					TxHash:        txHash,
					Chain:         tok.Chain,
					Timestamp:     time.Now().UTC(),
				}

				engine.ProcessSwap(event)
			}
		}
	}()
}
