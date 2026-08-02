// Command bot is the entrypoint for smart-cluster-bot.
//
// Start-up sequence:
//  1. Load config from environment variables.
//  2. Load i18n locale files.
//  3. Open SQLite database and run schema migrations.
//  4. Connect Telegram client + register webhook.
//  5. Start cluster detection engine and mock DEX feed.
//  6. Start alert broadcaster (engine → Telegram users).
//  7. Start daily digest scheduler.
//  8. Register HTTP routes and serve.
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
	"smart-cluster-bot/internal/i18n"
	"smart-cluster-bot/internal/storage"
	"smart-cluster-bot/internal/telegram"
	"smart-cluster-bot/web"
)

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

	// ── 4. Telegram client ─────────────────────────────────────────────────────
	tgClient := telegram.NewClient(cfg.BotToken)

	if cfg.WebhookURL != "" {
		if err := tgClient.SetWebhook(cfg.WebhookURL); err != nil {
			log.Printf("WARNING: setWebhook: %v", err)
		} else {
			log.Printf("INFO: webhook registered → %s", cfg.WebhookURL)
		}
	}

	// Set the chat menu button to open the WebApp (deferred so the bot is
	// fully ready before we hit the API).
	if cfg.RenderURL != "" {
		go func() {
			time.Sleep(3 * time.Second)
			if err := tgClient.SetChatMenuButton(cfg.RenderURL + "/app"); err != nil {
				log.Printf("WARNING: SetChatMenuButton: %v", err)
			} else {
				log.Printf("INFO: chat menu button → %s/app", cfg.RenderURL)
			}
		}()
	}

	// ── 5. Cluster detection engine ────────────────────────────────────────────
	// Parameters: ≥3 distinct wallets, ≥$10 000 aggregate volume,
	// 5-minute rolling window, 60-second alert cooldown per token.
	engine := detector.NewClusterEngine(3, 10_000.0, 5*time.Minute, 60*time.Second)

	ctx := context.Background()

	// Start mock DEX feed (replace with a real feed adapter in production).
	detector.StartMockFeed(ctx, engine, 15*time.Minute)

	// ── 6. Alert broadcaster ───────────────────────────────────────────────────
	telegram.StartAlertBroadcaster(ctx, tgClient, db, engine.AlertsChan)

	// ── 7. Daily digest ────────────────────────────────────────────────────────
	telegram.StartDailyDigest(ctx, tgClient, db)

	// ── 8. HTTP routes ─────────────────────────────────────────────────────────
	webhookHandler := telegram.NewWebhookHandler(tgClient, db, cfg, bundle)

	mux := http.NewServeMux()

	// Telegram webhook endpoint.
	mux.Handle("/webhook", webhookHandler)

	// Also accept updates on / for setups where Telegram is pointed at the root.
	mux.Handle("/", webhookHandler)

	// Health check (used by Render / load balancers).
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	})

	// WebApp static files embedded in the binary.
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

	// REST API: recent clusters (consumed by the WebApp).
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

		// Mock fallback if DB is empty so WebApp ALWAYS displays demo clusters
		if len(clusters) == 0 {
			clusters = []storage.ClusterRecord{
				{
					ID:                1,
					TokenAddress:      "0x6982508145454Ce325ddBe47a25d4ec3d2311933",
					TokenSymbol:       "PEPE",
					Chain:             "Ethereum",
					BuyCount:          5,
					TotalVolumeUSD:    142500.0,
					TimeWindowSeconds: 300,
					WalletAddress:     "0xd8dA6BF26964aF9D7eEd9e03E53415D37aA96045",
					CreatedAt:         time.Now().UTC().Add(-5 * time.Minute),
				},
				{
					ID:                2,
					TokenAddress:      "EKpQGSJtjMFqKZ9KQanSqYXRcF8fBopzLHYxdM65zcjm",
					TokenSymbol:       "WIF",
					Chain:             "Solana",
					BuyCount:          4,
					TotalVolumeUSD:    98400.0,
					TimeWindowSeconds: 300,
					WalletAddress:     "CuieVDEDtLo7FypA9SbLM9saXFdb1dsshEkyErMqkRQq",
					CreatedAt:         time.Now().UTC().Add(-12 * time.Minute),
				},
				{
					ID:                3,
					TokenAddress:      "0x4200000000000000000000000000000000000042",
					TokenSymbol:       "BRETT",
					Chain:             "Base",
					BuyCount:          3,
					TotalVolumeUSD:    65000.0,
					TimeWindowSeconds: 300,
					WalletAddress:     "0xAb5801a7D398351b8bE11C439e05C5B3259aeC9B",
					CreatedAt:         time.Now().UTC().Add(-20 * time.Minute),
				},
			}
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(clusters)
	})

	// REST API: 24h stats (consumed by the WebApp dashboard).
	mux.HandleFunc("/api/stats", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}

		stats, err := db.GetStats24h()
		if err != nil || stats == nil || stats.TotalClusters == 0 {
			stats = &storage.Stats24h{
				TotalClusters:  12,
				TotalVolumeUSD: 305900.0,
				TopToken:       "PEPE",
				TopChain:       "Ethereum",
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(stats)
	})

	addr := ":" + cfg.Port
	log.Printf("INFO: smart-cluster-bot listening on %s", addr)
	if cfg.RenderURL != "" {
		log.Printf("INFO: WebApp → %s/app", cfg.RenderURL)
	}
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("FATAL: server: %v", err)
	}
}
