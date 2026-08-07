// Command backtest is a standalone CLI for analyzing bot performance.
//
// Two modes:
//
//	backtest -mode=analyze -db=./data/bot.db
//	    Analyzes your bot's own accumulated positions (from SIMULATION_MODE
//	    or live trading). Works today, zero external data needed.
//
//	backtest -mode=replay -swaps=swaps.csv -prices=prices.csv
//	    Replays historical swap data through the cluster engine with your
//	    current live thresholds and reports what would have happened.
//
//	backtest -mode=sweep -swaps=swaps.csv -prices=prices.csv
//	    Grid-searches a range of thresholds against the same historical data
//	    to find which configuration would have performed best.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"

	"smart-cluster-bot/internal/backtest"
	"smart-cluster-bot/internal/storage"
)

func main() {
	mode := flag.String("mode", "analyze", "analyze | replay | sweep")
	dbPath := flag.String("db", "./data/bot.db", "path to the bot's SQLite database (analyze mode)")
	limit := flag.Int("limit", 10000, "max closed positions to analyze (analyze mode)")

	swapsPath := flag.String("swaps", "swaps.csv", "path to historical swaps CSV (replay/sweep mode)")
	pricesPath := flag.String("prices", "prices.csv", "path to historical prices CSV (replay/sweep mode)")

	minWallets := flag.Int("min-wallets", 3, "min wallets threshold (replay mode)")
	minVolume := flag.Float64("min-volume", 1500, "min volume USD threshold (replay mode)")
	windowSecs := flag.Int("window", 120, "cluster time window in seconds (replay mode)")
	cooldownSecs := flag.Int("cooldown", 60, "per-token cooldown in seconds")
	tpPct := flag.Float64("tp", 50, "take profit %% (replay mode)")
	slPct := flag.Float64("sl", 15, "stop loss %% (replay mode)")
	tradeSize := flag.Float64("trade-size", 1.5, "simulated trade size in USD")

	topN := flag.Int("top", 10, "how many top sweep results to print")

	flag.Parse()

	switch *mode {
	case "analyze":
		runAnalyze(*dbPath, *limit)
	case "replay":
		runReplay(*swapsPath, *pricesPath, backtest.ThresholdConfig{
			MinWallets:    *minWallets,
			MinVolumeUSD:  *minVolume,
			WindowSeconds: *windowSecs,
			CooldownSecs:  *cooldownSecs,
			TakeProfitPct: *tpPct,
			StopLossPct:   *slPct,
			TradeSizeUSD:  *tradeSize,
		})
	case "sweep":
		runSweep(*swapsPath, *pricesPath, *cooldownSecs, *tradeSize, *topN)
	default:
		fmt.Fprintf(os.Stderr, "unknown -mode %q (want analyze | replay | sweep)\n", *mode)
		os.Exit(1)
	}
}

func runAnalyze(dbPath string, limit int) {
	store, err := storage.InitDB(dbPath)
	if err != nil {
		log.Fatalf("FATAL: open db: %v", err)
	}
	defer store.Close()

	analyzer := backtest.NewAnalyzer(store)
	stats, err := analyzer.AnalyzeClosedPositions(limit)
	if err != nil {
		log.Fatalf("FATAL: analyze: %v", err)
	}

	fmt.Println(stats.Report())
}

func runReplay(swapsPath, pricesPath string, cfg backtest.ThresholdConfig) {
	source := &backtest.CSVDataSource{SwapsPath: swapsPath, PricesPath: pricesPath}
	replayer := backtest.NewReplayer(source)

	result, err := replayer.Run(cfg)
	if err != nil {
		log.Fatalf("FATAL: replay: %v", err)
	}

	fmt.Printf("Replay result — %s\n\n", cfg)
	fmt.Printf("Trades: %d\n", result.TradeCount)
	fmt.Printf("Win rate: %.1f%%\n", result.WinRate*100)
	fmt.Printf("Profit factor: %.2f\n", result.ProfitFactor)
	fmt.Printf("Net PnL: $%.2f\n\n", result.TotalPnLUSD)

	skipped := 0
	for _, t := range result.Trades {
		if t.ExitReason == "no_price_data" {
			skipped++
		}
	}
	if skipped > 0 {
		fmt.Printf("Note: %d cluster alerts had no matching price data and were excluded (not counted as wins or losses).\n", skipped)
	}
}

func runSweep(swapsPath, pricesPath string, cooldownSecs int, tradeSize float64, topN int) {
	source := &backtest.CSVDataSource{SwapsPath: swapsPath, PricesPath: pricesPath}

	sweepCfg := backtest.SweepConfig{
		MinWallets:    []int{2, 3, 4, 5},
		MinVolumeUSD:  []float64{500, 1000, 1500, 3000},
		WindowSeconds: []int{60, 120, 180},
		TakeProfitPct: []float64{30, 50, 100},
		StopLossPct:   []float64{10, 15, 25},
		CooldownSecs:  cooldownSecs,
		TradeSizeUSD:  tradeSize,
	}

	result, err := backtest.RunSweep(source, sweepCfg)
	if err != nil {
		log.Fatalf("FATAL: sweep: %v", err)
	}

	fmt.Println(result.Report(topN))
}
