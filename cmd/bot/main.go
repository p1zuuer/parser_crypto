// Command bot is the entrypoint for the solo trading sniper station.
//
// Start-up sequence:
//  1. Load config from environment variables (BOT_TOKEN, ADMIN_CHAT_ID required).
//  2. Load i18n locale files.
//  3. Open SQLite database (WAL mode) and run schema migrations.
//  4. Seed smart wallets (whales) on first boot.
//  5. Connect Telegram client + register webhook.
//  6. Start cluster detection engine (thresholds loaded from DB) and mock feed.
//  7. Start alert broadcaster (engine → admin) and daily digest.
//  8. Start the background data pruner (24h retention).
//  9. Register HTTP routes and serve.
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

// dataRetention bounds how long cluster history is kept before pruning.
const dataRetention = 24 * time.Hour

// prunerInterval controls how often the background pruner runs after the
// initial startup pass.
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
	// Thresholds are loaded from the persisted Sniper Settings so a restart
	// doesn't silently reset tuning done via the Telegram UI.
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

	// Start mock DEX feed (replace with a real feed adapter in production).
	detector.StartMockFeed(ctx, engine, 90*time.Second)

	// ── 7. Alert broadcaster + daily digest ────────────────────────────────────
	telegram.StartAlertBroadcaster(ctx, tgClient, db, cfg, engine.AlertsChan)
	telegram.StartDailyDigest(ctx, tgClient, db, cfg)

	// ── 8. Background pruner ────────────────────────────────────────────────────
	startPruner(ctx, db)

	// ── 9. HTTP routes ─────────────────────────────────────────────────────────
	webhookHandler := telegram.NewWebhookHandler(tgClient, db, cfg, bundle, engine)

	mux := http.NewServeMux()
	mux.Handle("/webhook", webhookHandler)
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
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("FATAL: server: %v", err)
	}
}

// resolveWebAppURL picks the Mini App URL to register as the chat menu
// button, preferring an explicit override.
func resolveWebAppURL(cfg *config.Config) string {
	if cfg.WebAppURL != "" {
		return cfg.WebAppURL
	}
	if cfg.RenderURL != "" {
		return cfg.RenderURL + "/app"
	}
	return ""
}

// startPruner runs an immediate prune pass, then repeats on prunerInterval.
// Wrapped in panic recovery so a database hiccup never crashes the process.
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

	// Immediate pass at startup so a long-stopped bot doesn't carry stale data.
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
