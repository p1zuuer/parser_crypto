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

// dedupTTL is the anti-duplicate window: once a token has fired an alert, it
// will not fire again for this long, independent of the per-bucket cooldown.
// This is a belt-and-suspenders guard against alert spam on choppy feeds.
const dedupTTL = 15 * time.Minute

// ClusterEngine processes swap events and emits ClusterAlerts when thresholds
// are exceeded. It is safe for concurrent use: bucket state and thresholds
// are guarded by mu, and the dedup cache is guarded by dedupMu independently
// so alert-cache housekeeping never blocks the hot swap-processing path.
type ClusterEngine struct {
	mu         sync.RWMutex
	buckets    map[string]*tokenBucket // keyed by "chain:tokenAddress"
	minWallets int
	minVolume  float64
	window     time.Duration
	cooldown   time.Duration

	dedupMu sync.Mutex
	alerted map[string]time.Time // keyed by "chain:tokenAddress" → expiry

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
		alerted:    make(map[string]time.Time),
		AlertsChan: make(chan ClusterAlert, 64),
	}
}

// UpdateThresholds live-updates detection parameters (e.g. from the Sniper
// Settings menu) without requiring a process restart. Safe for concurrent use.
func (e *ClusterEngine) UpdateThresholds(minWallets int, minVolume float64, window time.Duration) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.minWallets = minWallets
	e.minVolume = minVolume
	e.window = window
}

// Thresholds returns the current detection parameters.
func (e *ClusterEngine) Thresholds() (minWallets int, minVolume float64, window time.Duration) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.minWallets, e.minVolume, e.window
}

// ProcessSwap ingests a single swap event and, if it causes a cluster to cross
// the detection thresholds, emits a ClusterAlert on AlertsChan.
//
// This method is called from the feed goroutine; a panic here (e.g. from a
// malformed event) must never crash the whole process, so callers are
// expected to wrap their feed loop with recover() — see feed.go.
func (e *ClusterEngine) ProcessSwap(ev SwapEvent) {
	key := ev.Chain + ":" + ev.TokenAddress

	e.mu.Lock()
	minWallets := e.minWallets
	minVolume := e.minVolume
	window := e.window
	cooldown := e.cooldown

	b, ok := e.buckets[key]
	if !ok {
		b = &tokenBucket{}
		e.buckets[key] = b
	}

	// Append and immediately prune events outside the window.
	b.events = append(b.events, ev)
	cutoff := time.Now().UTC().Add(-window)
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

	crossesThreshold := len(walletSet) >= minWallets && totalVolume >= minVolume
	withinCooldown := time.Now().UTC().Before(b.cooldown)

	var alert ClusterAlert
	shouldAlert := false

	if crossesThreshold && !withinCooldown {
		b.cooldown = time.Now().UTC().Add(cooldown)
		alert = ClusterAlert{
			TokenAddress:      ev.TokenAddress,
			TokenSymbol:       ev.TokenSymbol,
			Chain:             ev.Chain,
			BuyCount:          len(b.events),
			TotalVolumeUSD:    totalVolume,
			TimeWindowSeconds: int(window.Seconds()),
			LeadWallet:        leadWallet,
		}
		shouldAlert = true
	}
	e.mu.Unlock()

	if !shouldAlert {
		return
	}

	// Independent 15-minute dedup layer: even if the per-token cooldown is
	// short, never re-alert the same token address within dedupTTL.
	if e.recentlyAlerted(key) {
		return
	}
	e.markAlerted(key)

	// Non-blocking send: drop if channel is full to avoid stalling the caller.
	select {
	case e.AlertsChan <- alert:
	default:
	}
}

// recentlyAlerted reports whether key fired an alert within the last dedupTTL,
// opportunistically purging expired entries while it holds the lock.
func (e *ClusterEngine) recentlyAlerted(key string) bool {
	e.dedupMu.Lock()
	defer e.dedupMu.Unlock()

	now := time.Now().UTC()
	if expiry, ok := e.alerted[key]; ok {
		if now.Before(expiry) {
			return true
		}
		delete(e.alerted, key)
	}

	// Light housekeeping: purge any other expired entries so the map doesn't
	// grow unbounded across a long-running process.
	for k, exp := range e.alerted {
		if now.After(exp) {
			delete(e.alerted, k)
		}
	}
	return false
}

func (e *ClusterEngine) markAlerted(key string) {
	e.dedupMu.Lock()
	defer e.dedupMu.Unlock()
	e.alerted[key] = time.Now().UTC().Add(dedupTTL)
}
