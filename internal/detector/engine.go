package detector

import (
	"sync"
	"time"
)

// SwapEvent represents a single token swap event from a DEX feed.
type SwapEvent struct {
	TokenAddress  string
	TokenSymbol   string
	WalletAddress string
	AmountUSD     float64
	TxHash        string
	Chain         string
	Timestamp     time.Time
}

// ClusterAlert represents a detected cluster alert for a specific token.
type ClusterAlert struct {
	TokenAddress      string
	TokenSymbol       string
	Chain             string
	BuyCount          int // number of unique wallets
	TotalVolumeUSD    float64
	TimeWindowSeconds int
	TxHashes          []string
}

// ClusterEngine evaluates incoming swap events against a sliding window and emits alerts.
type ClusterEngine struct {
	mu           sync.Mutex
	events       []SwapEvent
	minWallets   int
	minVolumeUSD float64
	timeWindow   time.Duration
	cooldown     time.Duration
	lastAlerts   map[string]time.Time // TokenAddress -> last alert timestamp
	AlertsChan   chan ClusterAlert
}

// NewClusterEngine creates a new ClusterEngine with given parameters.
func NewClusterEngine(minWallets int, minVolumeUSD float64, timeWindow time.Duration, cooldown time.Duration) *ClusterEngine {
	return &ClusterEngine{
		minWallets:   minWallets,
		minVolumeUSD: minVolumeUSD,
		timeWindow:   timeWindow,
		cooldown:     cooldown,
		lastAlerts:   make(map[string]time.Time),
		AlertsChan:   make(chan ClusterAlert, 100),
	}
}

// ProcessSwap adds an event, cleans old events, checks cluster conditions, and emits alerts.
func (e *ClusterEngine) ProcessSwap(event SwapEvent) {
	e.mu.Lock()
	defer e.mu.Unlock()

	now := time.Now()
	if event.Timestamp.IsZero() {
		event.Timestamp = now
	}

	// Append event
	e.events = append(e.events, event)

	// Clean up events outside the time window
	cutoff := now.Add(-e.timeWindow)
	validEvents := make([]SwapEvent, 0, len(e.events))
	for _, ev := range e.events {
		if ev.Timestamp.After(cutoff) {
			validEvents = append(validEvents, ev)
		}
	}
	e.events = validEvents

	// Group/evaluate by TokenAddress & Chain
	// We want to check cluster conditions for the token of the incoming event (or all tokens in window).
	// Let's check specifically for the event's TokenAddress to be efficient.
	targetToken := event.TokenAddress
	targetChain := event.Chain

	uniqueWallets := make(map[string]bool)
	var totalVolume float64
	var txHashes []string
	var tokenSymbol string

	for _, ev := range e.events {
		if ev.TokenAddress == targetToken && ev.Chain == targetChain {
			uniqueWallets[ev.WalletAddress] = true
			totalVolume += ev.AmountUSD
			txHashes = append(txHashes, ev.TxHash)
			tokenSymbol = ev.TokenSymbol
		}
	}

	// Check criteria
	if len(uniqueWallets) >= e.minWallets && totalVolume >= e.minVolumeUSD {
		// Check cooldown
		lastTime, exists := e.lastAlerts[targetToken]
		if !exists || now.Sub(lastTime) > e.cooldown {
			e.lastAlerts[targetToken] = now

			alert := ClusterAlert{
				TokenAddress:      targetToken,
				TokenSymbol:       tokenSymbol,
				Chain:             targetChain,
				BuyCount:          len(uniqueWallets),
				TotalVolumeUSD:    totalVolume,
				TimeWindowSeconds: int(e.timeWindow.Seconds()),
				TxHashes:          txHashes,
			}

			// Non-blocking send or buffered send
			select {
			case e.AlertsChan <- alert:
			default:
				// Channel full, drop or skip
			}
		}
	}
}
