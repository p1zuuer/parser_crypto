// Command bot is the entrypoint for the solo trading sniper station.
//
// Start-up sequence:
//  1. Load config from environment variables (BOT_TOKEN, ADMIN_CHAT_ID required).
//  2. Load i18n locale files.
//  3. Open SQLite database (WAL mode) and run schema migrations.
//  4. Seed smart wallets (whales) on first boot.
//  5. Connect Telegram client + register webhook.
//  6. Start the cluster detection engine (thresholds loaded from DB).
//  7. Construct the auto-buy backend (real JupiterBuyer if configured, else Noop).
//  8. Start the alert broadcaster (engine → admin, with auto-buy) and daily digest.
//  9. Start the background data pruner (24h retention).
//  10. Register HTTP routes (Telegram webhook + Helius live-feed webhook) and serve.
package main

import (
	"context"
	"encoding/json"
	"io/fs"
	"log"
	"net/http"
	"time"

	"smart-cluster-bot/internal/config"
	"smart-cluster-bot/internal/detector"
	"smart-cluster-bot/internal/dex"
	"smart-cluster-bot/internal/i18n"
	"smart-cluster-bot/internal/storage"
	"smart-cluster-bot/internal/telegram"
	"smart-cluster-bot/internal/trading"
	"smart-cluster-bot/internal/whales"
	"smart-cluster-bot/web"
)

const dataRetention = 24 * time.Hour
const prunerInterval = 1 * time.Hour

func main() {
	// ── 1. Config ──────────────────────────────────────────────────────────────
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("FATAL: config: %v", err)
	}

	// ── 2. i18n ────────────────────────────────────────────────────────────────
	bundle, err := i18n.Load("locales")
	if err != nil {
		log.Fatalf("FATAL: i18n: %v", err)
	}

	// ── 3. Database ────────────────────────────────────────────────────────────
	db, err := storage.InitDB(cfg.DatabasePath)
	if err != nil {
		log.Fatalf("FATAL: storage: %v", err)
	}
	defer db.Close()

	// ── 4. Seed whales ─────────────────────────────────────────────────────────
	if err := db.SeedWallets(); err != nil {
		log.Printf("WARNING: SeedWallets: %v", err)
	}

	// ── 5. Telegram client ─────────────────────────────────────────────────────
	tgClient := telegram.NewClient(cfg.BotToken)

	if cfg.WebhookURL != "" {
		if err := tgClient.SetWebhook(cfg.WebhookURL); err != nil {
			log.Printf("WARNING: setWebhook: %v", err)
		} else {
			log.Printf("INFO: webhook registered → %s", cfg.WebhookURL)
		}
	}

	if webAppURL := resolveWebAppURL(cfg); webAppURL != "" {
		go func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("[PANIC RECOVER] SetChatMenuButton goroutine: %v", r)
				}
			}()
			time.Sleep(3 * time.Second)
			if err := tgClient.SetChatMenuButton(webAppURL); err != nil {
				log.Printf("WARNING: SetChatMenuButton: %v", err)
			} else {
				log.Printf("INFO: chat menu button → %s", webAppURL)
			}
		}()
	}

	// ── 6. Cluster detection engine ────────────────────────────────────────────
	// Thresholds are loaded from persisted Sniper Settings so a restart
	// doesn't silently reset tuning done via the Telegram UI. Real swap data
	// now arrives exclusively via the Helius webhook — the mock feed has
	// been removed for live trading.
	settings, err := db.GetSniperSettings()
	if err != nil {
		log.Fatalf("FATAL: load sniper settings: %v", err)
	}
	engine := detector.NewClusterEngine(
		settings.MinWallets,
		settings.MinVolumeUSD,
		time.Duration(settings.WindowSeconds)*time.Second,
		60*time.Second, // per-token cooldown
	)

	ctx := context.Background()

	// ── 7. Auto-buy backend + seller ───────────────────────────────────────────
	var buyer trading.AutoBuyer = trading.NoopBuyer{}
	var seller *trading.Seller

	notify := func(msg string) {
		if err := tgClient.SendMessage(cfg.AdminChatID, msg); err != nil {
			log.Printf("[NOTIFY] send to admin: %v", err)
		}
	}

	if cfg.AutoBuyEnabled {
		jupiterBuyer, err := trading.NewJupiterBuyer(cfg.SolPrivateKey, cfg.SolanaRPCURL)
		if err != nil {
			log.Printf("WARNING: auto-buy requested but wallet init failed, falling back to Noop: %v", err)
		} else {
			buyer = jupiterBuyer
			seller = trading.NewSeller(jupiterBuyer, db, notify)
			seller.Start(ctx)
			log.Printf("INFO: auto-buy ENABLED — $%.2f per Solana cluster, TP +%.0f%% / SL -%.0f%%",
				cfg.AutoBuyAmountUSD, trading.DefaultTakeProfitPct, trading.DefaultStopLossPct)
		}
	} else {
		log.Printf("INFO: auto-buy disabled (set AUTO_BUY_ENABLED=true and SOL_PRIVATE_KEY to enable)")
	}

	// ── 8. Alert broadcaster + daily digest ────────────────────────────────────
	telegram.StartAlertBroadcaster(ctx, tgClient, db, cfg, buyer, seller, engine.AlertsChan)
	telegram.StartDailyDigest(ctx, tgClient, db, cfg)

	// ── 9. Shadow Whale Finder (runs every hour + on-demand via Telegram button) ──
	whaleFinder := whales.NewFinder(notify)
	whales.StartScheduled(ctx, notify)
	// on-demand trigger wired into the Telegram handler
	onDemandFinder := func() { whaleFinder.Run(ctx) }

	// ── 9. Background pruner ────────────────────────────────────────────────────
	startPruner(ctx, db)

	// ── 10. HTTP routes ─────────────────────────────────────────────────────────
	webhookHandler := telegram.NewWebhookHandler(tgClient, db, cfg, bundle, engine, onDemandFinder)
	heliusHandler := dex.NewHeliusHandler(engine, cfg.HeliusWebhookSecret)

	mux := http.NewServeMux()
	mux.Handle("/webhook", webhookHandler)
	mux.Handle("/webhook/helius", heliusHandler)
	mux.Handle("/", webhookHandler)

	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	})

	fileServer := http.FileServer(http.FS(web.WebFS))
	mux.Handle("/app/", http.StripPrefix("/app/", fileServer))
	mux.HandleFunc("/app", func(w http.ResponseWriter, r *http.Request) {
		data, err := fs.ReadFile(web.WebFS, "index.html")
		if err != nil {
			http.Error(w, "Not Found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(data)
	})

	mux.HandleFunc("/api/clusters", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}
		clusters, err := db.GetRecentClusters(50)
		if err != nil {
			log.Printf("ERROR: /api/clusters: %v", err)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(clusters)
	})

	mux.HandleFunc("/api/stats", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}
		stats, err := db.GetStats24h()
		if err != nil {
			log.Printf("ERROR: /api/stats: %v", err)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(stats)
	})

	addr := ":" + cfg.Port
	log.Printf("INFO: solo sniper station listening on %s", addr)
	log.Printf("INFO: point your Helius webhook at %s/webhook/helius", cfg.RenderURL)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("FATAL: server: %v", err)
	}
}

func resolveWebAppURL(cfg *config.Config) string {
	if cfg.WebAppURL != "" {
		return cfg.WebAppURL
	}
	if cfg.RenderURL != "" {
		return cfg.RenderURL + "/app"
	}
	return ""
}

func startPruner(ctx context.Context, db *storage.Storage) {
	runPrune := func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[PANIC RECOVER] pruner recovered: %v", r)
			}
		}()
		n, err := db.PruneOldData(dataRetention)
		if err != nil {
			log.Printf("WARNING: prune: %v", err)
			return
		}
		if n > 0 {
			log.Printf("INFO: pruner removed %d cluster record(s) older than %s", n, dataRetention)
		}
	}

	runPrune()

	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[PANIC RECOVER] pruner goroutine: %v", r)
			}
		}()
		ticker := time.NewTicker(prunerInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				runPrune()
			}
		}
	}()
}
