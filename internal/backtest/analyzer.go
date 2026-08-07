// Package backtest provides two complementary tools:
//
//  1. Analyzer — computes real performance statistics from the bot's own
//     accumulated `positions` table. This works TODAY with zero external
//     data dependencies: every trade your bot has recorded (in simulation
//     or live) is a real, honest data point. This is the tool to use while
//     you run in SIMULATION_MODE per your plan.
//
//  2. Replay engine (see replay.go) — re-runs historical swap data through
//     an isolated detector.ClusterEngine to test whether different
//     thresholds would have produced better results. This requires
//     historical swap data you supply (CSV export from Helius, Bitquery,
//     Dune, or your own long-running simulation journal) — there is no
//     live historical data feed wired in here.
package backtest

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"smart-cluster-bot/internal/storage"
)

// JournalStats holds every metric computed from a set of closed positions.
type JournalStats struct {
	TotalTrades      int
	ClosedTrades     int
	OpenTrades       int
	Wins             int
	Losses           int
	WinRate          float64
	TotalPnLUSD      float64
	AvgPnLUSD        float64
	AvgWinUSD        float64
	AvgLossUSD       float64
	ProfitFactor     float64 // avg win / avg loss; 0 if no losses recorded
	LargestWinUSD    float64
	LargestLossUSD   float64
	MaxDrawdownUSD   float64 // largest peak-to-trough equity dip across the sample
	TPHitCount       int     // closed_tp
	SLHitCount       int     // closed_sl
	ManualCloseCount int     // closed_manual
	AvgHoldMinutes   float64
	FirstTradeUTC    time.Time
	LastTradeUTC     time.Time
	ByToken          map[string]*TokenBreakdown
}

// TokenBreakdown aggregates PnL per token symbol, useful for spotting which
// specific tokens/patterns are driving (or destroying) performance.
type TokenBreakdown struct {
	Symbol      string
	Trades      int
	Wins        int
	TotalPnLUSD float64
}

// Analyzer computes JournalStats from storage.
type Analyzer struct {
	store *storage.Storage
}

// NewAnalyzer constructs an Analyzer bound to the live database.
func NewAnalyzer(store *storage.Storage) *Analyzer {
	return &Analyzer{store: store}
}

// AnalyzeClosedPositions pulls up to `limit` recent closed positions and
// computes full statistics. Pass a large limit (e.g. 10000) to analyze your
// entire simulation history.
func (a *Analyzer) AnalyzeClosedPositions(limit int) (*JournalStats, error) {
	closed, err := a.store.GetRecentClosedPositions(limit)
	if err != nil {
		return nil, fmt.Errorf("backtest: fetch closed positions: %w", err)
	}
	open, err := a.store.GetOpenPositions()
	if err != nil {
		return nil, fmt.Errorf("backtest: fetch open positions: %w", err)
	}

	return computeStats(closed, len(open)), nil
}

// computeStats is pure and side-effect free so it's easy to unit test
// independently of the database.
func computeStats(closed []storage.Position, openCount int) *JournalStats {
	stats := &JournalStats{
		OpenTrades: openCount,
		ByToken:    make(map[string]*TokenBreakdown),
	}
	if len(closed) == 0 {
		stats.TotalTrades = openCount
		return stats
	}

	// Sort oldest-first so drawdown and equity-curve math reads chronologically.
	sort.Slice(closed, func(i, j int) bool {
		return closed[i].EntryTimeUTC.Before(closed[j].EntryTimeUTC)
	})

	var sumWinUSD, sumLossUSD float64
	var equity, peak, maxDrawdown float64
	var sumHoldMinutes float64
	holdSamples := 0

	for _, p := range closed {
		stats.ClosedTrades++
		stats.TotalPnLUSD += p.PnLUSD
		equity += p.PnLUSD
		if equity > peak {
			peak = equity
		}
		if dd := peak - equity; dd > maxDrawdown {
			maxDrawdown = dd
		}

		if p.PnLUSD > 0 {
			stats.Wins++
			sumWinUSD += p.PnLUSD
			if p.PnLUSD > stats.LargestWinUSD {
				stats.LargestWinUSD = p.PnLUSD
			}
		} else if p.PnLUSD < 0 {
			stats.Losses++
			sumLossUSD += -p.PnLUSD
			if -p.PnLUSD > stats.LargestLossUSD {
				stats.LargestLossUSD = -p.PnLUSD
			}
		}

		switch {
		case strings.HasSuffix(p.Status, "closed_tp"):
			stats.TPHitCount++
		case strings.HasSuffix(p.Status, "closed_sl"):
			stats.SLHitCount++
		case strings.HasSuffix(p.Status, "closed_manual"):
			stats.ManualCloseCount++
		}

		if p.ExitTimeUTC != nil {
			holdMinutes := p.ExitTimeUTC.Sub(p.EntryTimeUTC).Minutes()
			if holdMinutes > 0 {
				sumHoldMinutes += holdMinutes
				holdSamples++
			}
		}

		if stats.FirstTradeUTC.IsZero() || p.EntryTimeUTC.Before(stats.FirstTradeUTC) {
			stats.FirstTradeUTC = p.EntryTimeUTC
		}
		if p.ExitTimeUTC != nil && p.ExitTimeUTC.After(stats.LastTradeUTC) {
			stats.LastTradeUTC = *p.ExitTimeUTC
		}

		tb, ok := stats.ByToken[p.TokenSymbol]
		if !ok {
			tb = &TokenBreakdown{Symbol: p.TokenSymbol}
			stats.ByToken[p.TokenSymbol] = tb
		}
		tb.Trades++
		tb.TotalPnLUSD += p.PnLUSD
		if p.PnLUSD > 0 {
			tb.Wins++
		}
	}

	stats.TotalTrades = stats.ClosedTrades + openCount
	stats.MaxDrawdownUSD = maxDrawdown

	if stats.ClosedTrades > 0 {
		stats.WinRate = float64(stats.Wins) / float64(stats.ClosedTrades)
		stats.AvgPnLUSD = stats.TotalPnLUSD / float64(stats.ClosedTrades)
	}
	if stats.Wins > 0 {
		stats.AvgWinUSD = sumWinUSD / float64(stats.Wins)
	}
	if stats.Losses > 0 {
		stats.AvgLossUSD = sumLossUSD / float64(stats.Losses)
	}
	if stats.AvgLossUSD > 0 {
		stats.ProfitFactor = stats.AvgWinUSD / stats.AvgLossUSD
	}
	if holdSamples > 0 {
		stats.AvgHoldMinutes = sumHoldMinutes / float64(holdSamples)
	}

	return stats
}

// Report renders JournalStats as a plain-text report suitable for Telegram
// or terminal output.
func (s *JournalStats) Report() string {
	if s.ClosedTrades == 0 {
		return fmt.Sprintf(
			"Simulation Journal — No closed trades yet\n\n"+
				"Open positions: %d\n\n"+
				"Nothing to analyze until positions close (TP/SL/manual). "+
				"Let the bot run longer in SIMULATION_MODE before drawing conclusions — "+
				"a handful of trades tells you almost nothing statistically.",
			s.OpenTrades,
		)
	}

	var sb strings.Builder
	sb.WriteString("Simulation Journal — Performance Report\n\n")
	fmt.Fprintf(&sb, "Period: %s → %s\n\n",
		s.FirstTradeUTC.Format("02 Jan 2006"),
		lastOr(s.LastTradeUTC, "ongoing"),
	)

	fmt.Fprintf(&sb, "Closed trades: %d (open: %d)\n", s.ClosedTrades, s.OpenTrades)
	fmt.Fprintf(&sb, "Win rate: %.1f%% (%d wins / %d losses)\n", s.WinRate*100, s.Wins, s.Losses)
	fmt.Fprintf(&sb, "Total PnL: $%.2f\n", s.TotalPnLUSD)
	fmt.Fprintf(&sb, "Avg PnL/trade: $%.2f\n", s.AvgPnLUSD)
	fmt.Fprintf(&sb, "Avg win: $%.2f | Avg loss: $%.2f\n", s.AvgWinUSD, s.AvgLossUSD)
	if s.ProfitFactor > 0 {
		fmt.Fprintf(&sb, "Profit factor: %.2f\n", s.ProfitFactor)
	}
	fmt.Fprintf(&sb, "Largest win: $%.2f | Largest loss: $%.2f\n", s.LargestWinUSD, s.LargestLossUSD)
	fmt.Fprintf(&sb, "Max drawdown: $%.2f\n", s.MaxDrawdownUSD)
	if s.AvgHoldMinutes > 0 {
		fmt.Fprintf(&sb, "Avg hold time: %.0f min\n", s.AvgHoldMinutes)
	}
	sb.WriteString("\nExit breakdown:\n")
	fmt.Fprintf(&sb, "  Take-profit: %d\n", s.TPHitCount)
	fmt.Fprintf(&sb, "  Stop-loss: %d\n", s.SLHitCount)
	fmt.Fprintf(&sb, "  Manual: %d\n", s.ManualCloseCount)

	if len(s.ByToken) > 0 {
		type kv struct {
			k string
			v *TokenBreakdown
		}
		var sorted []kv
		for k, v := range s.ByToken {
			sorted = append(sorted, kv{k, v})
		}
		sort.Slice(sorted, func(i, j int) bool { return sorted[i].v.TotalPnLUSD > sorted[j].v.TotalPnLUSD })

		sb.WriteString("\nTop tokens by PnL:\n")
		limit := 5
		if len(sorted) < limit {
			limit = len(sorted)
		}
		for i := 0; i < limit; i++ {
			t := sorted[i].v
			symbol := t.Symbol
			if symbol == "" {
				symbol = "(unknown)"
			}
			fmt.Fprintf(&sb, "  %s: $%.2f (%d trades, %d wins)\n", symbol, t.TotalPnLUSD, t.Trades, t.Wins)
		}
	}

	sb.WriteString(interpretationNote(s))
	return sb.String()
}

// interpretationNote adds honest, non-hype context so a small or skewed
// sample doesn't get over-interpreted as "the bot is profitable."
func interpretationNote(s *JournalStats) string {
	var notes []string

	if s.ClosedTrades < 20 {
		notes = append(notes, fmt.Sprintf(
			"\n⚠ Only %d closed trades — this is too small a sample to draw "+
				"real conclusions. Even a coin flip can look like a 70%% win rate "+
				"over a handful of trades. Keep running.", s.ClosedTrades))
	}
	if s.ProfitFactor > 0 && s.ProfitFactor < 1.0 {
		notes = append(notes, "\n⚠ Profit factor < 1.0 — average losses are "+
			"larger than average wins. Even a good win rate can lose money with this shape.")
	}
	if s.MaxDrawdownUSD > math.Abs(s.TotalPnLUSD)*2 && s.TotalPnLUSD > 0 {
		notes = append(notes, "\n⚠ Max drawdown is large relative to total PnL — "+
			"the equity curve is volatile even though the net result is positive.")
	}
	return strings.Join(notes, "")
}

func lastOr(t time.Time, fallback string) string {
	if t.IsZero() {
		return fallback
	}
	return t.Format("02 Jan 2006")
}
