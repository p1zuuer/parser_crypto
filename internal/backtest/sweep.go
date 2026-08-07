package backtest

import (
	"fmt"
	"sort"
	"strings"
)

// SweepConfig defines the grid of parameter values to test. Every
// combination of these values is run once, so keep the grid reasonably
// sized — 4x4x3x2x2 = 192 combinations is already a lot of replay runs.
type SweepConfig struct {
	MinWallets    []int
	MinVolumeUSD  []float64
	WindowSeconds []int
	TakeProfitPct []float64
	StopLossPct   []float64
	CooldownSecs  int     // fixed across the sweep
	TradeSizeUSD  float64 // fixed across the sweep
}

// SweepResult ranks every ThresholdConfig tested by net PnL.
type SweepResult struct {
	Results []*RunResult
}

// RunSweep replays the same historical data against every combination in
// cfg, returning results ranked by total PnL (best first). This is how you
// find out whether your current live thresholds (3 wallets, $1500, 120s)
// are actually good, or just reasonable-sounding guesses.
func RunSweep(source DataSource, cfg SweepConfig) (*SweepResult, error) {
	if len(cfg.MinWallets) == 0 || len(cfg.MinVolumeUSD) == 0 ||
		len(cfg.WindowSeconds) == 0 || len(cfg.TakeProfitPct) == 0 || len(cfg.StopLossPct) == 0 {
		return nil, fmt.Errorf("backtest: sweep config has an empty parameter list")
	}

	replayer := NewReplayer(source)
	var results []*RunResult

	for _, mw := range cfg.MinWallets {
		for _, vol := range cfg.MinVolumeUSD {
			for _, window := range cfg.WindowSeconds {
				for _, tp := range cfg.TakeProfitPct {
					for _, sl := range cfg.StopLossPct {
						tc := ThresholdConfig{
							MinWallets:    mw,
							MinVolumeUSD:  vol,
							WindowSeconds: window,
							CooldownSecs:  cfg.CooldownSecs,
							TakeProfitPct: tp,
							StopLossPct:   sl,
							TradeSizeUSD:  cfg.TradeSizeUSD,
						}
						r, err := replayer.Run(tc)
						if err != nil {
							return nil, fmt.Errorf("backtest: sweep run %s: %w", tc, err)
						}
						results = append(results, r)
					}
				}
			}
		}
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].TotalPnLUSD > results[j].TotalPnLUSD
	})

	return &SweepResult{Results: results}, nil
}

// Report renders the top N sweep results as a readable table.
func (s *SweepResult) Report(topN int) string {
	if len(s.Results) == 0 {
		return "Sweep: no results."
	}
	if topN <= 0 || topN > len(s.Results) {
		topN = len(s.Results)
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "Parameter Sweep — %d configs tested, top %d by net PnL:\n\n", len(s.Results), topN)

	for i := 0; i < topN; i++ {
		r := s.Results[i]
		fmt.Fprintf(&sb,
			"%d. %s\n"+
				"   Trades: %d | Win rate: %.1f%% | Profit factor: %.2f\n"+
				"   Net PnL: $%.2f\n\n",
			i+1, r.Config, r.TradeCount, r.WinRate*100, r.ProfitFactor, r.TotalPnLUSD,
		)
	}

	sb.WriteString(
		"Reminder: this ranks configs against ONE historical sample. A config \n" +
			"that wins here isn't guaranteed to keep winning — markets change. \n" +
			"Prefer configs that are stable across multiple time windows over \n" +
			"the single best-scoring one (that's often overfit to this sample).",
	)
	return sb.String()
}
