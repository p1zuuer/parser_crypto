package backtest

import (
	"bufio"
	"encoding/csv"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strconv"
	"time"

	"smart-cluster-bot/internal/detector"
)

// HistoricalSwap is one raw swap event from your historical data source.
// This is intentionally identical in shape to detector.SwapEvent so replay
// feeds the exact same engine code path that runs live.
type HistoricalSwap struct {
	TokenAddress  string
	TokenSymbol   string
	WalletAddress string
	AmountUSD     float64
	Chain         string
	Timestamp     time.Time
}

// PricePoint is one historical price observation for a token, used to
// simulate TP/SL exits against real price movement after a cluster fires.
type PricePoint struct {
	TokenAddress string
	PriceUSD     float64
	Timestamp    time.Time
}

// DataSource abstracts where historical data comes from. Implement this
// against Helius/Bitquery/Dune/Birdeye's historical APIs to backtest against
// real data — CSVDataSource below is a working reference implementation
// that reads local exported files, since no live historical feed is wired
// into this environment.
type DataSource interface {
	// LoadSwaps returns all historical swap events in the source, ideally
	// sorted ascending by Timestamp (the replay engine will sort if not).
	LoadSwaps() ([]HistoricalSwap, error)
	// LoadPrices returns historical price points used for TP/SL simulation.
	LoadPrices() ([]PricePoint, error)
}

// ── CSV Data Source (works today) ──────────────────────────────────────────

// CSVDataSource loads historical swaps and prices from two local CSV files.
//
// swaps.csv columns:   token_address,token_symbol,wallet_address,amount_usd,chain,timestamp_unix
// prices.csv columns:  token_address,price_usd,timestamp_unix
//
// You can produce these files by exporting from Helius' historical parsed
// transactions API, a Bitquery Solana DEX trades query, Dune, or — most
// practically — by letting the bot accumulate its own `clusters` table over
// weeks of live monitoring and exporting that.
type CSVDataSource struct {
	SwapsPath  string
	PricesPath string
}

func (c *CSVDataSource) LoadSwaps() ([]HistoricalSwap, error) {
	f, err := os.Open(c.SwapsPath)
	if err != nil {
		return nil, fmt.Errorf("backtest: open swaps CSV %s: %w", c.SwapsPath, err)
	}
	defer f.Close()

	r := csv.NewReader(bufio.NewReader(f))
	r.FieldsPerRecord = 6

	var swaps []HistoricalSwap
	lineNum := 0
	for {
		record, err := r.Read()
		if err == io.EOF {
			break
		}
		lineNum++
		if err != nil {
			return nil, fmt.Errorf("backtest: parse swaps CSV line %d: %w", lineNum, err)
		}
		if lineNum == 1 && record[0] == "token_address" {
			continue // header row
		}

		amount, err := strconv.ParseFloat(record[3], 64)
		if err != nil {
			return nil, fmt.Errorf("backtest: swaps CSV line %d: bad amount_usd %q: %w", lineNum, record[3], err)
		}
		ts, err := strconv.ParseInt(record[5], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("backtest: swaps CSV line %d: bad timestamp %q: %w", lineNum, record[5], err)
		}

		swaps = append(swaps, HistoricalSwap{
			TokenAddress:  record[0],
			TokenSymbol:   record[1],
			WalletAddress: record[2],
			AmountUSD:     amount,
			Chain:         record[4],
			Timestamp:     time.Unix(ts, 0).UTC(),
		})
	}

	sort.Slice(swaps, func(i, j int) bool { return swaps[i].Timestamp.Before(swaps[j].Timestamp) })
	return swaps, nil
}

func (c *CSVDataSource) LoadPrices() ([]PricePoint, error) {
	if c.PricesPath == "" {
		return nil, nil // price-based TP/SL simulation is optional
	}
	f, err := os.Open(c.PricesPath)
	if err != nil {
		return nil, fmt.Errorf("backtest: open prices CSV %s: %w", c.PricesPath, err)
	}
	defer f.Close()

	r := csv.NewReader(bufio.NewReader(f))
	r.FieldsPerRecord = 3

	var prices []PricePoint
	lineNum := 0
	for {
		record, err := r.Read()
		if err == io.EOF {
			break
		}
		lineNum++
		if err != nil {
			return nil, fmt.Errorf("backtest: parse prices CSV line %d: %w", lineNum, err)
		}
		if lineNum == 1 && record[0] == "token_address" {
			continue
		}

		price, err := strconv.ParseFloat(record[1], 64)
		if err != nil {
			return nil, fmt.Errorf("backtest: prices CSV line %d: bad price_usd %q: %w", lineNum, record[1], err)
		}
		ts, err := strconv.ParseInt(record[2], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("backtest: prices CSV line %d: bad timestamp %q: %w", lineNum, record[2], err)
		}

		prices = append(prices, PricePoint{
			TokenAddress: record[0],
			PriceUSD:     price,
			Timestamp:    time.Unix(ts, 0).UTC(),
		})
	}

	sort.Slice(prices, func(i, j int) bool { return prices[i].Timestamp.Before(prices[j].Timestamp) })
	return prices, nil
}

// ── Replay engine ──────────────────────────────────────────────────────────

// ThresholdConfig is one candidate set of detection + exit parameters to test.
type ThresholdConfig struct {
	MinWallets    int
	MinVolumeUSD  float64
	WindowSeconds int
	CooldownSecs  int
	TakeProfitPct float64
	StopLossPct   float64
	TradeSizeUSD  float64
}

func (t ThresholdConfig) String() string {
	return fmt.Sprintf("wallets=%d vol=$%.0f window=%ds TP=%.0f%% SL=%.0f%%",
		t.MinWallets, t.MinVolumeUSD, t.WindowSeconds, t.TakeProfitPct, t.StopLossPct)
}

// SimulatedTrade is one trade produced by replaying a cluster alert against
// historical price data with a given ThresholdConfig.
type SimulatedTrade struct {
	TokenAddress string
	TokenSymbol  string
	EntryTime    time.Time
	EntryPrice   float64
	ExitTime     time.Time
	ExitPrice    float64
	ExitReason   string // "tp", "sl", "no_price_data", "still_open_at_end"
	PnLUSD       float64
}

// RunResult is the aggregate output of replaying one ThresholdConfig.
type RunResult struct {
	Config       ThresholdConfig
	Trades       []SimulatedTrade
	TotalPnLUSD  float64
	WinRate      float64
	ProfitFactor float64
	TradeCount   int
}

// Replayer runs historical swap data through an isolated detector.ClusterEngine
// (never the live one) for a given ThresholdConfig, then simulates TP/SL exits
// against historical prices.
type Replayer struct {
	source DataSource
}

// NewReplayer constructs a Replayer against the given data source.
func NewReplayer(source DataSource) *Replayer {
	return &Replayer{source: source}
}

// Run replays all historical swaps through a fresh engine configured with
// cfg, capturing every ClusterAlert fired, then simulates the exit for each
// alert using price data. Returns a full RunResult.
//
// IMPORTANT: cluster alerts with no matching price data after their entry
// timestamp are recorded with ExitReason "no_price_data" and excluded from
// PnL — they are never assigned a fabricated outcome.
func (r *Replayer) Run(cfg ThresholdConfig) (*RunResult, error) {
	swaps, err := r.source.LoadSwaps()
	if err != nil {
		return nil, err
	}
	prices, err := r.source.LoadPrices()
	if err != nil {
		return nil, err
	}
	if len(swaps) == 0 {
		return nil, fmt.Errorf("backtest: no historical swaps loaded — nothing to replay")
	}

	// Isolated engine — completely separate from the live bot's engine.
	engine := detector.NewClusterEngine(
		cfg.MinWallets,
		cfg.MinVolumeUSD,
		time.Duration(cfg.WindowSeconds)*time.Second,
		time.Duration(cfg.CooldownSecs)*time.Second,
	)

	// Drain alerts into a slice as they're produced. The engine's AlertsChan
	// is buffered (64), but a long historical replay can exceed that, so we
	// drain concurrently while feeding swaps.
	var alerts []detector.ClusterAlert
	done := make(chan struct{})
	go func() {
		for a := range engine.AlertsChan {
			alerts = append(alerts, a)
		}
		close(done)
	}()

	for _, s := range swaps {
		engine.ProcessSwap(detector.SwapEvent{
			TokenAddress:  s.TokenAddress,
			TokenSymbol:   s.TokenSymbol,
			WalletAddress: s.WalletAddress,
			AmountUSD:     s.AmountUSD,
			Chain:         s.Chain,
			Timestamp:     s.Timestamp,
		})
	}
	close(engine.AlertsChan)
	<-done

	result := &RunResult{Config: cfg}
	var sumWin, sumLoss float64
	var wins int

	for _, alert := range alerts {
		trade := simulateExit(alert, prices, cfg)
		result.Trades = append(result.Trades, trade)

		if trade.ExitReason == "no_price_data" {
			continue // never counted in PnL — no fabricated outcome
		}

		result.TotalPnLUSD += trade.PnLUSD
		result.TradeCount++
		if trade.PnLUSD > 0 {
			wins++
			sumWin += trade.PnLUSD
		} else if trade.PnLUSD < 0 {
			sumLoss += -trade.PnLUSD
		}
	}

	if result.TradeCount > 0 {
		result.WinRate = float64(wins) / float64(result.TradeCount)
	}
	if sumLoss > 0 {
		avgWin := sumWin / math.Max(1, float64(wins))
		avgLoss := sumLoss / math.Max(1, float64(result.TradeCount-wins))
		if avgLoss > 0 {
			result.ProfitFactor = avgWin / avgLoss
		}
	}

	return result, nil
}

// simulateExit walks forward through price points for alert.TokenAddress
// after the alert fired, applying TP/SL logic identical in spirit to
// trading.Seller — sell 50% at +TP%, exit fully at -SL%, or ride to 2x-TP /
// 2x-SL for the back half. If no price data exists after entry, the trade
// is marked "no_price_data" and excluded from aggregate stats.
func simulateExit(alert detector.ClusterAlert, prices []PricePoint, cfg ThresholdConfig) SimulatedTrade {
	trade := SimulatedTrade{
		TokenAddress: alert.TokenAddress,
		TokenSymbol:  alert.TokenSymbol,
	}

	// Find the first price point for this token at or after the cluster's
	// actual fire time — this is the real entry point, not a guess.
	var entryIdx = -1
	for i, p := range prices {
		if p.TokenAddress != alert.TokenAddress {
			continue
		}
		if !p.Timestamp.Before(alert.FiredAt) {
			entryIdx = i
			break
		}
	}
	if entryIdx == -1 {
		trade.ExitReason = "no_price_data"
		return trade
	}

	entry := prices[entryIdx]
	trade.EntryTime = entry.Timestamp
	trade.EntryPrice = entry.PriceUSD

	remainingPct := 100.0
	realizedUSD := 0.0
	positionUSD := cfg.TradeSizeUSD

	for i := entryIdx + 1; i < len(prices); i++ {
		p := prices[i]
		if p.TokenAddress != alert.TokenAddress {
			continue
		}
		pctChange := ((p.PriceUSD - entry.PriceUSD) / entry.PriceUSD) * 100.0

		if remainingPct == 100.0 {
			if pctChange >= cfg.TakeProfitPct {
				sold := positionUSD * 0.5
				realizedUSD += sold * (pctChange / 100.0)
				remainingPct = 50.0
				continue
			}
			if pctChange <= -cfg.StopLossPct {
				realizedUSD += positionUSD * (pctChange / 100.0)
				trade.ExitTime = p.Timestamp
				trade.ExitPrice = p.PriceUSD
				trade.ExitReason = "sl"
				trade.PnLUSD = realizedUSD
				return trade
			}
		} else {
			fullTP := cfg.TakeProfitPct * 2
			remainingSL := cfg.StopLossPct * 2
			if pctChange >= fullTP {
				sold := positionUSD * 0.5
				realizedUSD += sold * (pctChange / 100.0)
				trade.ExitTime = p.Timestamp
				trade.ExitPrice = p.PriceUSD
				trade.ExitReason = "tp"
				trade.PnLUSD = realizedUSD
				return trade
			}
			if pctChange <= -remainingSL {
				sold := positionUSD * 0.5
				realizedUSD += sold * (pctChange / 100.0)
				trade.ExitTime = p.Timestamp
				trade.ExitPrice = p.PriceUSD
				trade.ExitReason = "sl"
				trade.PnLUSD = realizedUSD
				return trade
			}
		}
	}

	// Ran out of price data before hitting an exit condition.
	trade.ExitReason = "still_open_at_end"
	trade.PnLUSD = realizedUSD
	return trade
}
