// Package detector implements the cluster detection engine that watches swap
// events and fires alerts when multiple wallets accumulate the same token
// within a rolling time window.
package detector

import (
	"sync"
	"time"
)

// SwapEvent represents a single on-chain token swap captured from a DEX feed.
type SwapEvent struct {
	TokenAddress  string
	TokenSymbol   string
	WalletAddress string
	AmountUSD     float64
	TxHash        string
	Chain         string
	Timestamp     time.Time
}

// ClusterAlert is emitted when the engine detects a meaningful accumulation
// pattern that should be broadcast to subscribers.
type ClusterAlert struct {
	TokenAddress      string
	TokenSymbol       string
	Chain             string
	BuyCount          int
	TotalVolumeUSD    float64
	TimeWindowSeconds int
	// LeadWallet is the wallet with the highest individual buy in this cluster.
	LeadWallet string
}

// tokenBucket collects swap events for a single token within the time window.
type tokenBucket struct {
	events   []SwapEvent
	cooldown time.Time // no alerts before this time
}

// ClusterEngine processes swap events and emits ClusterAlerts when thresholds
// are exceeded. It is safe for concurrent use.
type ClusterEngine struct {
	mu         sync.Mutex
	buckets    map[string]*tokenBucket // keyed by "chain:tokenAddress"
	minWallets int
	minVolume  float64
	window     time.Duration
	cooldown   time.Duration
	AlertsChan chan ClusterAlert
}

// NewClusterEngine constructs a ClusterEngine.
//
//   - minWallets  – minimum distinct wallets to trigger an alert
//   - minVolume   – minimum aggregated USD volume to trigger an alert
//   - window      – rolling time window for accumulation
//   - cooldown    – minimum gap between repeated alerts for the same token
func NewClusterEngine(minWallets int, minVolume float64, window, cooldown time.Duration) *ClusterEngine {
	return &ClusterEngine{
		buckets:    make(map[string]*tokenBucket),
		minWallets: minWallets,
		minVolume:  minVolume,
		window:     window,
		cooldown:   cooldown,
		AlertsChan: make(chan ClusterAlert, 64),
	}
}

// ProcessSwap ingests a single swap event and, if it causes a cluster to cross
// the detection thresholds, emits a ClusterAlert on AlertsChan.
func (e *ClusterEngine) ProcessSwap(ev SwapEvent) {
	key := ev.Chain + ":" + ev.TokenAddress

	e.mu.Lock()
	defer e.mu.Unlock()

	b, ok := e.buckets[key]
	if !ok {
		b = &tokenBucket{}
		e.buckets[key] = b
	}

	// Append and immediately prune events outside the window.
	b.events = append(b.events, ev)
	cutoff := time.Now().UTC().Add(-e.window)
	fresh := b.events[:0]
	for _, fe := range b.events {
		if fe.Timestamp.After(cutoff) {
			fresh = append(fresh, fe)
		}
	}
	b.events = fresh

	// Count distinct wallets and total volume.
	walletSet := make(map[string]struct{}, len(b.events))
	var totalVolume float64
	var leadWallet string
	var leadAmt float64

	for _, fe := range b.events {
		walletSet[fe.WalletAddress] = struct{}{}
		totalVolume += fe.AmountUSD
		if fe.AmountUSD > leadAmt {
			leadAmt = fe.AmountUSD
			leadWallet = fe.WalletAddress
		}
	}

	// Check thresholds and cooldown.
	if len(walletSet) < e.minWallets || totalVolume < e.minVolume {
		return
	}
	if time.Now().UTC().Before(b.cooldown) {
		return
	}
	b.cooldown = time.Now().UTC().Add(e.cooldown)

	alert := ClusterAlert{
		TokenAddress:      ev.TokenAddress,
		TokenSymbol:       ev.TokenSymbol,
		Chain:             ev.Chain,
		BuyCount:          len(b.events),
		TotalVolumeUSD:    totalVolume,
		TimeWindowSeconds: int(e.window.Seconds()),
		LeadWallet:        leadWallet,
	}

	// Non-blocking send: drop if channel is full to avoid stalling the caller.
	select {
	case e.AlertsChan <- alert:
	default:
	}
}
