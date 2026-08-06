// Package whales implements the Shadow Whale discovery pipeline.
// It fetches top-gaining Solana tokens from DexScreener, extracts early
// buyers for each, then filters them through GMGN's wallet analytics API
// to find under-the-radar wallets that meet strict shadow-money criteria:
//
//   - Initial buy between $100–$1,000 (not whales that move markets)
//   - 30-day win rate 70–85%
//   - Minimum 20 trades in 30 days (rules out one-hit wonders)
//   - Not a known MEV/sniper bot (sub-2-second entry time filtered out)
//   - Not a developer wallet (no token creation transactions)
//
// Results are delivered directly to the admin's Telegram chat for review
// before any wallet is added to the tracking list.
package whales

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"
)

const (
	dexScreenerTopGainersURL = "https://api.dexscreener.com/token-boosts/top/v1"
	dexScreenerPairsURL      = "https://api.dexscreener.com/latest/dex/tokens/%s"
	gmgnWalletStatsURL       = "https://gmgn.ai/defi/quotation/v1/wallet_stats/sol/%s?period=30d"
	gmgnWalletTradesURL      = "https://gmgn.ai/defi/quotation/v1/wallet_holdings/sol/%s"

	httpTimeout = 8 * time.Second

	// Shadow whale sizing criteria
	minBuyUSD    = 100.0
	maxBuyUSD    = 1_000.0
	minWinRate   = 0.70 // 70%
	maxWinRate   = 0.85 // 85% — above this is likely a bot or inside info
	minTrades30d = 20   // rules out lucky one-hit-wonders
	maxTrades30d = 500  // above this is likely an MEV bot

	// Minimum seconds after token creation before a buy is considered
	// "not a sniper". Buys within 2 seconds of launch = sniper bot.
	minSecondsFromLaunch = 2
)

// Candidate is a wallet that passed all shadow whale filters and is ready
// for the operator to review and optionally add to the tracking list.
type Candidate struct {
	WalletAddress string
	WinRate30d    float64
	Trades30d     int
	AvgBuyUSD     float64
	TotalPnLUSD   float64
	Note          string
}

// Finder orchestrates the whale discovery pipeline.
type Finder struct {
	httpClient *http.Client
	notify     func(msg string) // sends a Telegram message to the admin
}

// NewFinder constructs a Finder. notify is called with the discovery report
// text so the caller can route it to the admin's Telegram chat.
func NewFinder(notify func(msg string)) *Finder {
	return &Finder{
		httpClient: &http.Client{Timeout: httpTimeout},
		notify:     notify,
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

	seen := make(map[string]struct{})
	var candidates []Candidate

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
			if _, already := seen[wallet]; already {
				continue
			}
			seen[wallet] = struct{}{}

			c, ok, err := f.evaluateWallet(ctx, wallet)
			if err != nil {
				log.Printf("[WHALE FINDER] evaluateWallet %s: %v", wallet, err)
				continue
			}
			if ok {
				candidates = append(candidates, c)
			}
		}
		if len(candidates) >= 10 {
			break // enough material — avoid hammering the APIs
		}
		// Be polite to free-tier APIs
		select {
		case <-ctx.Done():
			return
		case <-time.After(300 * time.Millisecond):
		}
	}

	f.notify(f.formatReport(candidates))
}

// ── DexScreener ───────────────────────────────────────────────────────────────

type dexBoostItem struct {
	TokenAddress string `json:"tokenAddress"`
	ChainID      string `json:"chainId"`
}

// fetchTopGainers returns the Solana token addresses from DexScreener's
// current top-boosted / top-gaining list (Solana only).
func (f *Finder) fetchTopGainers(ctx context.Context) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, dexScreenerTopGainersURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := f.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("DexScreener status %d", resp.StatusCode)
	}

	var items []dexBoostItem
	if err := json.NewDecoder(resp.Body).Decode(&items); err != nil {
		return nil, fmt.Errorf("decode top gainers: %w", err)
	}

	var addrs []string
	seen := make(map[string]struct{})
	for _, item := range items {
		if !strings.EqualFold(item.ChainID, "solana") {
			continue
		}
		if item.TokenAddress == "" {
			continue
		}
		if _, dup := seen[item.TokenAddress]; dup {
			continue
		}
		seen[item.TokenAddress] = struct{}{}
		addrs = append(addrs, item.TokenAddress)
		if len(addrs) >= 20 {
			break
		}
	}
	return addrs, nil
}

// dexPairsResponse is the relevant subset of DexScreener's /tokens/{addr} response.
type dexPairsResponse struct {
	Pairs []struct {
		Txns struct {
			H24 struct {
				Buys int `json:"buys"`
			} `json:"h24"`
		} `json:"txns"`
		// DexScreener doesn't expose individual buyer wallets in its free API.
		// We use the token address itself to query GMGN for early traders.
	} `json:"pairs"`
}

// fetchEarlyBuyers returns up to 15 wallet addresses that bought tokenAddress
// early (within the first hour). We query GMGN's token traders endpoint since
// DexScreener's free tier doesn't expose individual buyer wallets.
func (f *Finder) fetchEarlyBuyers(ctx context.Context, tokenAddress string) ([]string, error) {
	// GMGN token early traders endpoint (free, no auth required)
	url := fmt.Sprintf("https://gmgn.ai/defi/quotation/v1/tokens/early_buyers/sol/%s", tokenAddress)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")

	resp, err := f.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GMGN early buyers status %d", resp.StatusCode)
	}

	var result struct {
		Code int `json:"code"`
		Data []struct {
			Address         string  `json:"address"`
			BoughtTimestamp int64   `json:"bought_timestamp"`
			LaunchTimestamp int64   `json:"launch_timestamp"`
			AmountUSD       float64 `json:"amount_usd"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode early buyers: %w", err)
	}

	var wallets []string
	for _, entry := range result.Data {
		// Skip sniper bots: bought within 2 seconds of launch
		secondsFromLaunch := entry.BoughtTimestamp - entry.LaunchTimestamp
		if entry.LaunchTimestamp > 0 && secondsFromLaunch < minSecondsFromLaunch {
			continue
		}
		// Skip entries outside our shadow sizing criteria at the event level
		if entry.AmountUSD > maxBuyUSD*3 { // allow some headroom; we filter more precisely below
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

// ── GMGN Wallet Evaluation ────────────────────────────────────────────────────

// gmgnWalletStats is the relevant subset of GMGN's wallet stats response.
type gmgnWalletStats struct {
	Code int `json:"code"`
	Data struct {
		WinRate      float64 `json:"winrate"`
		TotalTrades  int     `json:"total_profit_trade"` // winning trades
		TotalTrades2 int     `json:"total_trade"`        // all trades
		AvgBuyUSD    float64 `json:"avg_cost"`
		TotalPnLUSD  float64 `json:"total_profit_usd"`
		// Tag field — GMGN labels known bots, devs, influencers
		Tags []string `json:"tags"`
		// Follower-equivalent: how many people are copy-trading this wallet
		CopyTradingCount int `json:"copy_trading_count"`
	} `json:"data"`
}

// evaluateWallet queries GMGN for wallet stats and applies all shadow whale
// filters. Returns (candidate, passed, error).
func (f *Finder) evaluateWallet(ctx context.Context, wallet string) (Candidate, bool, error) {
	url := fmt.Sprintf(gmgnWalletStatsURL, wallet)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Candidate{}, false, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")

	resp, err := f.httpClient.Do(req)
	if err != nil {
		return Candidate{}, false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusTooManyRequests {
		return Candidate{}, false, fmt.Errorf("GMGN rate limited")
	}
	if resp.StatusCode != http.StatusOK {
		return Candidate{}, false, fmt.Errorf("GMGN stats status %d", resp.StatusCode)
	}

	var stats gmgnWalletStats
	if err := json.NewDecoder(resp.Body).Decode(&stats); err != nil {
		return Candidate{}, false, fmt.Errorf("decode wallet stats: %w", err)
	}

	d := stats.Data

	// ── Filter 1: exclude known bots, devs, influencers via GMGN tags ──
	for _, tag := range d.Tags {
		t := strings.ToLower(tag)
		if strings.Contains(t, "bot") ||
			strings.Contains(t, "dev") ||
			strings.Contains(t, "mev") ||
			strings.Contains(t, "influencer") ||
			strings.Contains(t, "sniper") {
			return Candidate{}, false, nil
		}
	}

	// ── Filter 2: not already crowded / publicly copy-traded ──
	if d.CopyTradingCount > 500 {
		return Candidate{}, false, nil
	}

	// ── Filter 3: trade count in range (not a one-hit wonder, not a bot) ──
	totalTrades := d.TotalTrades2
	if totalTrades < minTrades30d || totalTrades > maxTrades30d {
		return Candidate{}, false, nil
	}

	// ── Filter 4: win rate in shadow range ──
	if d.WinRate < minWinRate || d.WinRate > maxWinRate {
		return Candidate{}, false, nil
	}

	// ── Filter 5: average buy size in the shadow sizing range ──
	if d.AvgBuyUSD < minBuyUSD || d.AvgBuyUSD > maxBuyUSD {
		return Candidate{}, false, nil
	}

	// ── Filter 6: must be profitable overall ──
	if d.TotalPnLUSD <= 0 {
		return Candidate{}, false, nil
	}

	return Candidate{
		WalletAddress: wallet,
		WinRate30d:    d.WinRate,
		Trades30d:     totalTrades,
		AvgBuyUSD:     d.AvgBuyUSD,
		TotalPnLUSD:   d.TotalPnLUSD,
		Note:          "70%+ Winrate — shadow whale candidate",
	}, true, nil
}

// ── Report formatting ─────────────────────────────────────────────────────────

func (f *Finder) formatReport(candidates []Candidate) string {
	if len(candidates) == 0 {
		return "Whale Finder: no shadow whale candidates found this run.\n\n" +
			"Criteria: $100–$1k avg buy, 70–85% winrate, 20–500 trades/30d, not a known bot/dev."
	}

	// Sort by win rate descending
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].WinRate30d > candidates[j].WinRate30d
	})

	// Cap report at 5 candidates — more than this creates noise
	if len(candidates) > 5 {
		candidates = candidates[:5]
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Shadow Whale Finder — %s UTC\n\n", time.Now().UTC().Format("02 Jan 15:04")))
	sb.WriteString(fmt.Sprintf("Found %d candidate(s) passing all filters:\n\n", len(candidates)))

	for i, c := range candidates {
		sb.WriteString(fmt.Sprintf(
			"%d. %s\n"+
				"   Win rate: %.0f%%\n"+
				"   Trades (30d): %d\n"+
				"   Avg buy: $%.0f\n"+
				"   Total PnL: $%.0f\n\n",
			i+1,
			c.WalletAddress,
			c.WinRate30d*100,
			c.Trades30d,
			c.AvgBuyUSD,
			c.TotalPnLUSD,
		))
	}

	sb.WriteString("Use /addwhale <address> to add any of these to your tracking list.\n")
	sb.WriteString("Review each on GMGN before adding — these are candidates, not guarantees.")
	return sb.String()
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

		// Immediate first run on startup
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
