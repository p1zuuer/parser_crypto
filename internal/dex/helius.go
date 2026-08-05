// Package dex adapts real-time on-chain data feeds into the detector engine.
// The current adapter (Helius) consumes Helius' "Enhanced Transactions"
// webhook format for Solana SWAP events.
package dex

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"

	"smart-cluster-bot/internal/detector"
)

// HeliusHandler is an http.Handler that receives Helius Enhanced Transaction
// webhook payloads, extracts SWAP events, and forwards them to the cluster
// detection engine as SwapEvents.
type HeliusHandler struct {
	engine       *detector.ClusterEngine
	sharedSecret string // optional — checked against the Authorization header
}

// NewHeliusHandler wires a Helius webhook receiver to engine. If sharedSecret
// is non-empty, incoming requests must present it via the Authorization
// header (as configured in the Helius dashboard's "Auth Header" field);
// requests without a matching header are rejected with 401.
func NewHeliusHandler(engine *detector.ClusterEngine, sharedSecret string) *HeliusHandler {
	return &HeliusHandler{engine: engine, sharedSecret: sharedSecret}
}

// heliusTransaction is the subset of Helius' Enhanced Transaction schema we
// care about. Helius sends an array of these per webhook POST.
type heliusTransaction struct {
	Type        string `json:"type"` // "SWAP" for swap events
	Timestamp   int64  `json:"timestamp"`
	Signature   string `json:"signature"`
	FeePayer    string `json:"feePayer"`
	Source      string `json:"source"` // DEX program label, e.g. "JUPITER", "RAYDIUM"
	Description string `json:"description"`

	Events struct {
		Swap *heliusSwapEvent `json:"swap"`
	} `json:"events"`
}

// heliusSwapEvent describes the token legs of a swap. Helius reports the
// "native" (SOL) and "token" transfers separately; we treat whichever leg is
// the non-SOL token as the traded asset, and derive a USD estimate from any
// attached token-amount + price info when present.
type heliusSwapEvent struct {
	NativeInput  *heliusNativeTransfer `json:"nativeInput"`
	NativeOutput *heliusNativeTransfer `json:"nativeOutput"`
	TokenInputs  []heliusTokenTransfer `json:"tokenInputs"`
	TokenOutputs []heliusTokenTransfer `json:"tokenOutputs"`
}

type heliusNativeTransfer struct {
	Account string `json:"account"`
	Amount  int64  `json:"amount"` // lamports
}

type heliusTokenTransfer struct {
	UserAccount    string `json:"userAccount"`
	TokenAccount   string `json:"tokenAccount"`
	Mint           string `json:"mint"`
	RawTokenAmount struct {
		TokenAmount string `json:"tokenAmount"`
		Decimals    int    `json:"decimals"`
	} `json:"rawTokenAmount"`
	// TokenAmountUSD is not part of the raw Helius payload — Helius does not
	// price legs itself. We accept it here only in case an upstream relay
	// (e.g. your own enrichment layer) has annotated the payload before
	// forwarding it to us; it is optional and defaults to 0 otherwise.
	TokenAmountUSD float64 `json:"tokenAmountUsd,omitempty"`
}

// solLamportsToUSDFallback is used when a webhook payload carries no USD
// annotation. Wire a real price oracle (Pyth/Birdeye) for production
// accuracy — this exists so the pipeline never silently drops an event for
// lack of a price.
const solLamportsToUSDFallback = 150.0 / 1_000_000_000.0 // $/lamport at ~$150/SOL

func (h *HeliusHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	if h.sharedSecret != "" {
		auth := r.Header.Get("Authorization")
		if auth != h.sharedSecret {
			log.Printf("[HELIUS] rejected webhook: bad Authorization header")
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
	}

	var txs []heliusTransaction
	if err := json.NewDecoder(r.Body).Decode(&txs); err != nil {
		log.Printf("[HELIUS] decode error: %v", err)
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}

	// Acknowledge immediately; Helius retries on non-2xx and slow responses
	// can cause it to back off delivery.
	w.WriteHeader(http.StatusOK)

	go h.processBatch(txs)
}

// processBatch is run asynchronously so the HTTP handler can return fast.
// Wrapped in panic recovery so a single malformed transaction never crashes
// the webhook receiver.
func (h *HeliusHandler) processBatch(txs []heliusTransaction) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[PANIC RECOVER] helius processBatch recovered: %v", r)
		}
	}()

	for _, tx := range txs {
		h.processOne(tx)
	}
}

func (h *HeliusHandler) processOne(tx heliusTransaction) {
	if !strings.EqualFold(tx.Type, "SWAP") || tx.Events.Swap == nil {
		return
	}
	swap := tx.Events.Swap

	tokenAddress, tokenAmountUSD, walletAddress := extractSwapLeg(swap, tx.FeePayer)
	if tokenAddress == "" || walletAddress == "" {
		return
	}
	if tokenAmountUSD <= 0 {
		// No priced leg found — fall back to valuing the SOL leg if present.
		tokenAmountUSD = estimateUSDFromNativeLegs(swap)
	}
	if tokenAmountUSD <= 0 {
		log.Printf("[HELIUS] skipping swap %s: could not determine USD value", tx.Signature)
		return
	}

	h.engine.ProcessSwap(detector.SwapEvent{
		TokenAddress:  tokenAddress,
		TokenSymbol:   "", // Helius doesn't include symbol; resolved downstream if needed
		WalletAddress: walletAddress,
		AmountUSD:     tokenAmountUSD,
		TxHash:        tx.Signature,
		Chain:         "Solana",
		Timestamp:     timestampOrNow(tx.Timestamp),
	})
}

// extractSwapLeg picks the non-SOL token leg out of a swap event, returning
// its mint address, best-effort USD value (if the payload carried one), and
// the wallet that initiated the swap.
func extractSwapLeg(swap *heliusSwapEvent, feePayer string) (tokenAddress string, usdValue float64, wallet string) {
	wallet = feePayer

	for _, out := range swap.TokenOutputs {
		if out.Mint == "" {
			continue
		}
		if out.UserAccount != "" {
			wallet = out.UserAccount
		}
		return out.Mint, out.TokenAmountUSD, wallet
	}
	for _, in := range swap.TokenInputs {
		if in.Mint == "" {
			continue
		}
		if in.UserAccount != "" {
			wallet = in.UserAccount
		}
		return in.Mint, in.TokenAmountUSD, wallet
	}
	return "", 0, wallet
}

// estimateUSDFromNativeLegs derives an approximate USD trade size from the
// SOL leg of the swap when no explicit USD annotation is present.
func estimateUSDFromNativeLegs(swap *heliusSwapEvent) float64 {
	var lamports int64
	if swap.NativeInput != nil {
		lamports = swap.NativeInput.Amount
	} else if swap.NativeOutput != nil {
		lamports = swap.NativeOutput.Amount
	}
	if lamports <= 0 {
		return 0
	}
	return float64(lamports) * solLamportsToUSDFallback
}

func timestampOrNow(unixSeconds int64) time.Time {
	if unixSeconds <= 0 {
		return time.Now().UTC()
	}
	return time.Unix(unixSeconds, 0).UTC()
}
