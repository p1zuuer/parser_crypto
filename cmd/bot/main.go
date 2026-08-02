package main

import (
	"context"
	"log"
	"net/http"
	"time"

	"smart-cluster-bot/internal/config"
	"smart-cluster-bot/internal/detector"
	"smart-cluster-bot/internal/i18n"
	"smart-cluster-bot/internal/storage"
	"smart-cluster-bot/internal/telegram"
)

func main() {
	// 1. Load configuration
	cfg := config.Load()

	// 2. Initialize i18n
	if err := i18n.Init("./locales"); err != nil {
		log.Fatalf("FATAL: Failed to initialize i18n: %v", err)
	}

	// 3. Initialize Database Storage
	db, err := storage.InitDB(cfg.DatabasePath)
	if err != nil {
		log.Fatalf("FATAL: Failed to initialize database: %v", err)
	}

	// 4. Initialize Telegram client
	tgClient := telegram.NewClient(cfg.BotToken)

	// 5. Initialize Cluster Detector Engine
	// Parameters: minWallets = 3, minVolumeUSD = 1000, timeWindow = 300s, cooldown = 60s
	clusterEngine := detector.NewClusterEngine(3, 1000.0, 300*time.Second, 60*time.Second)

	// 6. Start DEX Mock Feed Worker
	ctx := context.Background()
	detector.StartMockFeed(ctx, clusterEngine, 2*time.Second)

	// 7. Start Alert Broadcaster worker
	telegram.StartAlertBroadcaster(ctx, tgClient, db, clusterEngine.AlertsChan)

	// 8. Initialize webhook handler with storage
	webhookHandler := telegram.NewWebhookHandler(tgClient, db)

	// 9. Register HTTP routes
	mux := http.NewServeMux()
	mux.Handle("/webhook", webhookHandler)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	})

	// 10. Start server
	addr := ":" + cfg.Port
	log.Printf("INFO: Starting smart-cluster-bot server on port %s...", cfg.Port)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("FATAL: Server failed: %v", err)
	}
}
