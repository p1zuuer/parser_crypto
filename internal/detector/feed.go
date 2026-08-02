package detector

import (
	"context"
	"fmt"
	"log"
	"math/rand"
	"time"
)

var sampleTokens = []struct {
	Symbol  string
	Address string
	Chain   string
}{
	{"PEPE", "0x6982508145454Ce325ddBe47a25d4ec3d2311933", "Ethereum"},
	{"WIF", "EKpQGSJtjMFqKZ9KQanSqYXRcF8fBopzLHYxdM65zcjm", "Solana"},
	{"BONK", "DezXAZ8z7PnrnRJjz3wXBoRgixCa6xjnB7YaB1pPB263", "Solana"},
	{"SHIB", "0x95aD61b0a150d79219dcf64E1E6Cc01f0B64C4cE", "Ethereum"},
	{"BRETT", "0x4200000000000000000000000000000000000042", "Base"},
	{"FLOKI", "0xcf0C122c6b73ff809C693DB761e7BaeBe62b6a2E", "BSC"},
}

// sampleWallets simulates a pool of smart-money wallets.
var sampleWallets = []string{
	"0xd8dA6BF26964aF9D7eEd9e03E53415D37aA96045",
	"0xAb5801a7D398351b8bE11C439e05C5B3259aeC9B",
	"0x7a250d5630B4cF539739dF2C5dAcb4c659F2488D",
	"0xBE0eB53F46cd790Cd13851d5EFf43D12404d33E8",
	"0x40B38765696e3d5d8d9d834D8AaD4bB6e418E489",
	"0x3f5CE5FBFe3E9af3971dD833D26bA9b5C936f0bE",
	"CuieVDEDtLo7FypA9SbLM9saXFdb1dsshEkyErMqkRQq",
	"GThUX1Atko4tqhN2NaiTazFAcaPNt7ZQiMWL6gBvnQJK",
}

// StartMockFeed starts a background goroutine that generates synthetic swap
// events into the ClusterEngine at the given interval.
//
// Multiple swaps are injected per tick to increase the likelihood of crossing
// cluster thresholds, making it easy to observe the full alert pipeline
// during development without waiting for real DEX data.
func StartMockFeed(ctx context.Context, engine *ClusterEngine, interval time.Duration) {
	ticker := time.NewTicker(interval)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[PANIC RECOVER] StartMockFeed recovered: %v", r)
			}
		}()
		defer ticker.Stop()
		r := rand.New(rand.NewSource(time.Now().UnixNano()))

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				injectBurst(r, engine)
			}
		}
	}()
}

// injectBurst sends a cluster-sized burst of swaps on a randomly chosen token
// so that the engine fires an alert on most ticks.
func injectBurst(r *rand.Rand, engine *ClusterEngine) {
	tok := sampleTokens[r.Intn(len(sampleTokens))]

	// Shuffle wallets and pick 3–6 of them to simulate distinct buyers.
	wallets := make([]string, len(sampleWallets))
	copy(wallets, sampleWallets)
	r.Shuffle(len(wallets), func(i, j int) { wallets[i], wallets[j] = wallets[j], wallets[i] })
	count := 3 + r.Intn(4) // 3..6

	for i := 0; i < count; i++ {
		amount := 10_000.0 + r.Float64()*90_000.0 // $10k–$100k
		engine.ProcessSwap(SwapEvent{
			TokenAddress:  tok.Address,
			TokenSymbol:   tok.Symbol,
			WalletAddress: wallets[i%len(wallets)],
			AmountUSD:     amount,
			TxHash:        fmt.Sprintf("0xTx%x%x", r.Int63(), r.Int63()),
			Chain:         tok.Chain,
			Timestamp:     time.Now().UTC(),
		})
	}
}
