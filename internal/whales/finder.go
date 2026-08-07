// Package whales implements the Shadow Whale discovery pipeline.
//
// IMPORTANT — read this before trusting the output:
// The filters below are a statistical screen based on the last 7 days of
// on-chain activity. They are designed to reject the most obvious traps
// (bots, one-hit-wonders, unsustainable win rates) — they are NOT a
// prediction of future performance. Past PnL does not guarantee future
// profit. Nothing in this file can "guarantee" a wallet will keep winning.
// Always review a candidate yourself before copying it.
//
// Pipeline:
//  1. DexScreener top gainers → candidate tokens
//  2. GMGN early buyers per token → candidate wallets
//  3. GMGN 7-day wallet stats per candidate, evaluated through a strict
//     multi-layer profitability filter (see evaluateWallet)
//
// All network calls go through a shared rate limiter and a small worker
// pool so this never hammers GMGN into rate-limiting us (HTTP 429).
package whales

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	dexScreenerTopGainersURL = "https://api.dexscreener.com/token-boosts/top/v1"
	gmgnWalletStatsURL       = "https://gmgn.ai/defi/quotation/v1/wallet_stats/sol/%s?period=7d"

	httpTimeout = 8 * time.Second

	// ── Rate limiting / concurrency ──────────────────────────────────────
	// Keep this conservative. GMGN's free endpoints rate-limit aggressively;
	// 2-3 concurrent workers with a per-request floor delay keeps us well
	// under their threshold in practice.
	maxWorkers             = 3
	minRequestInterval     = 180 * time.Millisecond // floor delay between any two requests
	maxRetries             = 3
	retryBaseDelay         = 1 * time.Second // 1s, 2s, 4s
	maxCandidatesPerRun    = 10
	maxTokensScannedPerRun = 20

	// ── Smart Money Validation thresholds (all must pass) ────────────────
	minClosedTrades7d  = 5      // statistical significance floor
	minWinRate7d       = 0.55   // 55%
	minRealizedPnL7d   = 1500.0 // USD, net of fees
	minProfitFactor    = 1.2    // avg win / avg loss
	maxTotalTrades7d   = 100    // anti-MEV/bot ceiling
	minAvgHoldMinutes  = 2.0    // anti-bot floor
	maxHoursSinceTrade = 48.0   // must be actively trading
)

// Candidate is a wallet that passed every layer of the profitability filter.
type Candidate struct {
	WalletAddress   string
	WinRate7d       float64
	ClosedTrades7d  int
	TotalTrades7d   int
	RealizedPnL7d   float64
	ProfitFactor    float64
	AvgHoldMinutes  float64
	HoursSinceTrade float64
	Note            string
}

// ── Rate limiter ────────────────────────────────────────────────────────────

// rateLimiter enforces a minimum gap between successive outbound requests
// across all workers, so the aggregate request rate never exceeds
// 1 / minRequestInterval, regardless of how many goroutines are running.
type rateLimiter struct {
	mu       sync.Mutex
	lastCall time.Time
}

func (r *rateLimiter) wait(ctx context.Context) error {
	r.mu.Lock()
	next := r.lastCall.Add(minRequestInterval)
	now := time.Now()
	var sleep time.Duration
	if next.After(now) {
		sleep = next.Sub(now)
	}
	r.lastCall = now.Add(sleep)
	r.mu.Unlock()

	if sleep <= 0 {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(sleep):
		return nil
	}
}

// ── Finder ───────────────────────────────────────────────────────────────────

// Finder orchestrates the whale discovery pipeline.
type Finder struct {
	httpClient *http.Client
	notify     func(msg string)
	limiter    *rateLimiter
}

// NewFinder constructs a Finder. notify is called with the discovery report
// text so the caller can route it to the admin's Telegram chat.
func NewFinder(notify func(msg string)) *Finder {
	return &Finder{
		httpClient: &http.Client{Timeout: httpTimeout},
		notify:     notify,
		limiter:    &rateLimiter{},
	}
}

// Run executes the full discovery pipeline once and sends the result to the
// admin. Safe to call from a goroutine — panics are recovered internally.
func (f *Finder) Run(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[WHALE FINDER] panic recovered: %v", r)
		}
	}()

	log.Printf("[WHALE FINDER] starting discovery run")

	tokens, err := f.fetchTopGainers(ctx)
	if err != nil {
		log.Printf("[WHALE FINDER] fetchTopGainers: %v", err)
		f.notify("Whale Finder: could not fetch top gainers — " + err.Error())
		return
	}
	if len(tokens) == 0 {
		f.notify("Whale Finder: DexScreener returned no top gainers right now.")
		return
	}

	log.Printf("[WHALE FINDER] analysing %d top-gainer tokens", len(tokens))

	// Gather candidate wallet addresses (deduplicated) from early buyers of
	// each token, respecting the rate limiter for every request.
	seen := make(map[string]struct{})
	var walletQueue []string

	for _, tokenAddr := range tokens {
		if ctx.Err() != nil {
			break
		}
		buyers, err := f.fetchEarlyBuyers(ctx, tokenAddr)
		if err != nil {
			log.Printf("[WHALE FINDER] fetchEarlyBuyers %s: %v", tokenAddr, err)
			continue
		}
		for _, wallet := range buyers {
			if _, dup := seen[wallet]; dup {
				continue
			}
			seen[wallet] = struct{}{}
			walletQueue = append(walletQueue, wallet)
		}
	}

	if len(walletQueue) == 0 {
		f.notify("Whale Finder: no candidate wallets found from today's top gainers.")
		return
	}

	log.Printf("[WHALE FINDER] evaluating %d candidate wallets with %d workers", len(walletQueue), maxWorkers)
	candidates := f.evaluateWalletsConcurrently(ctx, walletQueue)

	f.notify(f.formatReport(candidates))
}

// evaluateWalletsConcurrently runs evaluateWallet across a bounded worker
// pool (maxWorkers goroutines) so we never fan out unbounded concurrent
// requests at GMGN. Every request additionally passes through the shared
// rate limiter inside doRequestWithRetry, so total throughput is capped
// even if all workers are busy at once.
func (f *Finder) evaluateWalletsConcurrently(ctx context.Context, wallets []string) []Candidate {
	sem := make(chan struct{}, maxWorkers)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var candidates []Candidate

	for _, wallet := range wallets {
		if ctx.Err() != nil {
			break
		}
		if len(candidates) >= maxCandidatesPerRun {
			break
		}

		sem <- struct{}{}
		wg.Add(1)
		go func(w string) {
			defer wg.Done()
			defer func() { <-sem }()
			defer func() {
				if r := recover(); r != nil {
					log.Printf("[WHALE FINDER] worker panic recovered for %s: %v", w, r)
				}
			}()

			c, ok, err := f.evaluateWallet(ctx, w)
			if err != nil {
				log.Printf("[WHALE FINDER] evaluateWallet %s: %v", w, err)
				return
			}
			if !ok {
				return
			}
			mu.Lock()
			candidates = append(candidates, c)
			mu.Unlock()
		}(wallet)
	}

	wg.Wait()
	return candidates
}

// ── HTTP with retry + backoff ──────────────────────────────────────────────

// doRequestWithRetry performs req through the shared rate limiter, retrying
// on HTTP 429 with exponential backoff (1s, 2s, 4s) plus jitter. Returns the
// response body bytes on success. On persistent failure (including after
// exhausting retries) it returns an error and the caller MUST discard the
// candidate — no fallback data is ever synthesized.
func (f *Finder) doRequestWithRetry(ctx context.Context, req *http.Request) ([]byte, error) {
	var lastErr error

	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			backoff := retryBaseDelay * time.Duration(1<<(attempt-1)) // 1s, 2s, 4s
			jitter := time.Duration(rand.Int63n(int64(200 * time.Millisecond)))
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff + jitter):
			}
		}

		if err := f.limiter.wait(ctx); err != nil {
			return nil, err
		}

		resp, err := f.httpClient.Do(req.Clone(ctx))
		if err != nil {
			lastErr = err
			continue
		}

		if resp.StatusCode == http.StatusTooManyRequests {
			resp.Body.Close()
			lastErr = fmt.Errorf("HTTP 429 rate limited (attempt %d/%d)", attempt+1, maxRetries+1)
			log.Printf("[WHALE FINDER] %v — backing off", lastErr)
			continue
		}

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(body), 200))
		}

		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("read body: %w", err)
		}
		return body, nil
	}

	return nil, fmt.Errorf("exhausted %d retries: %w", maxRetries, lastErr)
}

// ── DexScreener ───────────────────────────────────────────────────────────────

type dexBoostItem struct {
	TokenAddress string `json:"tokenAddress"`
	ChainID      string `json:"chainId"`
}

func (f *Finder) fetchTopGainers(ctx context.Context) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, dexScreenerTopGainersURL, nil)
	if err != nil {
		return nil, err
	}

	body, err := f.doRequestWithRetry(ctx, req)
	if err != nil {
		return nil, err
	}

	var items []dexBoostItem
	if err := json.Unmarshal(body, &items); err != nil {
		return nil, fmt.Errorf("decode top gainers: %w", err)
	}

	var addrs []string
	seen := make(map[string]struct{})
	for _, item := range items {
		if !strings.EqualFold(item.ChainID, "solana") || item.TokenAddress == "" {
			continue
		}
		if _, dup := seen[item.TokenAddress]; dup {
			continue
		}
		seen[item.TokenAddress] = struct{}{}
		addrs = append(addrs, item.TokenAddress)
		if len(addrs) >= maxTokensScannedPerRun {
			break
		}
	}
	return addrs, nil
}

// fetchEarlyBuyers returns wallet addresses that bought tokenAddress early.
func (f *Finder) fetchEarlyBuyers(ctx context.Context, tokenAddress string) ([]string, error) {
	url := fmt.Sprintf("https://gmgn.ai/defi/quotation/v1/tokens/early_buyers/sol/%s", tokenAddress)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")

	body, err := f.doRequestWithRetry(ctx, req)
	if err != nil {
		return nil, err
	}

	var result struct {
		Code int `json:"code"`
		Data []struct {
			Address         string `json:"address"`
			BoughtTimestamp int64  `json:"bought_timestamp"`
			LaunchTimestamp int64  `json:"launch_timestamp"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("decode early buyers: %w", err)
	}

	var wallets []string
	for _, entry := range result.Data {
		// Skip sniper bots: bought within 2 seconds of launch.
		secondsFromLaunch := entry.BoughtTimestamp - entry.LaunchTimestamp
		if entry.LaunchTimestamp > 0 && secondsFromLaunch < 2 {
			continue
		}
		if entry.Address != "" {
			wallets = append(wallets, entry.Address)
		}
		if len(wallets) >= 15 {
			break
		}
	}
	return wallets, nil
}

// ── GMGN 7-day wallet evaluation ──────────────────────────────────────────────

// gmgnWalletStats7d is the subset of GMGN's 7-day wallet stats response we
// need for the profitability math. Field names follow GMGN's public API
// naming; if GMGN changes their schema, missing fields decode to zero and
// evaluateWallet's "cannot verify → discard" rule protects us from acting
// on incomplete data.
type gmgnWalletStats7d struct {
	Code int `json:"code"`
	Data struct {
		// Trade counts
		WinTradeCount   int `json:"win_trade_count"`   // winning closed trades
		LossTradeCount  int `json:"loss_trade_count"`  // losing closed trades
		TotalTradeCount int `json:"total_trade_count"` // all trades incl. unclosed

		// Realized PnL, net of fees, in USD
		RealizedProfitUSD float64 `json:"realized_profit_usd"`

		// Average win/loss size in USD — used for profit factor
		AvgWinUSD  float64 `json:"avg_win_usd"`
		AvgLossUSD float64 `json:"avg_loss_usd"`

		// Average hold time in seconds
		AvgHoldTimeSeconds float64 `json:"avg_holding_period_seconds"`

		// Unix seconds of the wallet's most recent trade
		LastTradeTimestamp int64 `json:"last_trade_timestamp"`

		Tags             []string `json:"tags"`
		CopyTradingCount int      `json:"copy_trading_count"`
	} `json:"data"`
}

// evaluateWallet fetches 7-day stats for wallet and runs it through every
// layer of the profitability filter. Returns (candidate, passed, error).
//
// Fail-safe rule: if ANY required metric is missing, zero in a way that
// can't be distinguished from "no data", or the request fails after
// retries, the candidate is discarded (ok=false) — never included on
// incomplete information, and never backed by mock/placeholder data.
func (f *Finder) evaluateWallet(ctx context.Context, wallet string) (Candidate, bool, error) {
	url := fmt.Sprintf(gmgnWalletStatsURL, wallet)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Candidate{}, false, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")

	body, err := f.doRequestWithRetry(ctx, req)
	if err != nil {
		// Could not verify — discard, per fail-safe rule.
		return Candidate{}, false, err
	}

	var stats gmgnWalletStats7d
	if err := json.Unmarshal(body, &stats); err != nil {
		return Candidate{}, false, fmt.Errorf("decode wallet stats: %w", err)
	}
	d := stats.Data

	// ── Exclude known bots/devs/influencers via GMGN tags ──
	for _, tag := range d.Tags {
		t := strings.ToLower(tag)
		if strings.Contains(t, "bot") || strings.Contains(t, "dev") ||
			strings.Contains(t, "mev") || strings.Contains(t, "influencer") ||
			strings.Contains(t, "sniper") {
			return Candidate{}, false, nil
		}
	}
	if d.CopyTradingCount > 500 {
		return Candidate{}, false, nil // already crowded, edge likely priced in
	}

	// ── Layer 1: Statistical significance ──
	closedTrades := d.WinTradeCount + d.LossTradeCount
	if closedTrades < minClosedTrades7d {
		return Candidate{}, false, nil
	}

	// ── Layer 2: Win rate & net realized PnL ──
	winRate := float64(d.WinTradeCount) / float64(closedTrades)
	if winRate < minWinRate7d {
		return Candidate{}, false, nil
	}
	if d.RealizedProfitUSD <= minRealizedPnL7d {
		return Candidate{}, false, nil
	}

	// ── Layer 3: Profit factor (avg win / avg loss) ──
	// If avg loss is zero/missing we cannot compute a real ratio — discard
	// rather than assume an infinite/undefined profit factor.
	if d.AvgLossUSD <= 0 || d.AvgWinUSD <= 0 {
		return Candidate{}, false, nil
	}
	profitFactor := d.AvgWinUSD / d.AvgLossUSD
	if profitFactor < minProfitFactor {
		return Candidate{}, false, nil
	}

	// ── Layer 4: Anti-MEV / anti-bot ──
	if d.TotalTradeCount <= 0 || d.TotalTradeCount > maxTotalTrades7d {
		return Candidate{}, false, nil
	}
	if d.AvgHoldTimeSeconds <= 0 {
		return Candidate{}, false, nil // can't verify — discard
	}
	avgHoldMinutes := d.AvgHoldTimeSeconds / 60.0
	if avgHoldMinutes <= minAvgHoldMinutes {
		return Candidate{}, false, nil
	}

	// ── Layer 5: Recency — must be actively trading ──
	if d.LastTradeTimestamp <= 0 {
		return Candidate{}, false, nil // can't verify — discard
	}
	hoursSinceTrade := time.Since(time.Unix(d.LastTradeTimestamp, 0)).Hours()
	if hoursSinceTrade < 0 || hoursSinceTrade > maxHoursSinceTrade {
		return Candidate{}, false, nil
	}

	return Candidate{
		WalletAddress:   wallet,
		WinRate7d:       winRate,
		ClosedTrades7d:  closedTrades,
		TotalTrades7d:   d.TotalTradeCount,
		RealizedPnL7d:   d.RealizedProfitUSD,
		ProfitFactor:    profitFactor,
		AvgHoldMinutes:  avgHoldMinutes,
		HoursSinceTrade: hoursSinceTrade,
		Note:            "Passed 7D Smart Money filter — verify before copying",
	}, true, nil
}

// ── Report formatting ─────────────────────────────────────────────────────────

func (f *Finder) formatReport(candidates []Candidate) string {
	if len(candidates) == 0 {
		return "Whale Finder: no wallets passed the 7-day profitability filter this run.\n\n" +
			fmt.Sprintf(
				"Criteria: ≥%d closed trades, ≥%.0f%% win rate, >$%.0f net PnL, "+
					"≥%.1f profit factor, ≤%d total trades, >%.0fmin avg hold, "+
					"active within %.0fh.\n\n"+
					"A strict filter finding nothing is normal on some runs — "+
					"it means no wallet in today's sample cleared the bar, not "+
					"that the pipeline is broken.",
				minClosedTrades7d, minWinRate7d*100, minRealizedPnL7d,
				minProfitFactor, maxTotalTrades7d, minAvgHoldMinutes, maxHoursSinceTrade,
			)
	}

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].RealizedPnL7d > candidates[j].RealizedPnL7d
	})
	if len(candidates) > 5 {
		candidates = candidates[:5]
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Shadow Whale Finder — %s UTC\n\n", time.Now().UTC().Format("02 Jan 15:04")))
	sb.WriteString(fmt.Sprintf("%d wallet(s) passed the full 7-day profitability filter:\n\n", len(candidates)))

	for i, c := range candidates {
		sb.WriteString(fmt.Sprintf(
			"%d. %s\n"+
				"   Win rate: %.0f%% (%d closed trades)\n"+
				"   Net PnL (7d): $%.0f\n"+
				"   Profit factor: %.2f\n"+
				"   Avg hold: %.0f min\n"+
				"   Last trade: %.0fh ago\n\n",
			i+1, c.WalletAddress,
			c.WinRate7d*100, c.ClosedTrades7d,
			c.RealizedPnL7d,
			c.ProfitFactor,
			c.AvgHoldMinutes,
			c.HoursSinceTrade,
		))
	}

	sb.WriteString("This is a statistical screen of the last 7 days, not a guarantee of future performance.\n")
	sb.WriteString("Use /addwhale <address> to track any of these. Review each one yourself before copying.")
	return sb.String()
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// StartScheduled launches the finder on an hourly schedule.
// Also runs once immediately at startup so you get a result right away.
// Manual on-demand runs (via Telegram button) are independent and always
// allowed — they don't reset or conflict with the hourly timer.
func StartScheduled(ctx context.Context, notify func(string)) {
	f := NewFinder(notify)

	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[WHALE FINDER] scheduler goroutine recovered: %v", r)
			}
		}()

		f.Run(ctx)

		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				log.Printf("[WHALE FINDER] hourly run starting")
				f.Run(ctx)
			}
		}
	}()
}
