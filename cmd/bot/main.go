package main

import (
	"context"
	"encoding/json"
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

	// Set Telegram Chat Menu Button to open WebApp
	go func() {
		time.Sleep(3 * time.Second) // slight delay to ensure bot startup
		webAppURL := cfg.RenderURL + "/app"
		if err := tgClient.SetChatMenuButton(webAppURL); err != nil {
			log.Printf("WARNING: failed to set chat menu button: %v", err)
		} else {
			log.Printf("INFO: successfully set Telegram chat menu button to %s", webAppURL)
		}
	}()

	// 5. Initialize Cluster Detector Engine
	// Parameters: minWallets = 3, minVolumeUSD = 10000, timeWindow = 300s, cooldown = 60s
	clusterEngine := detector.NewClusterEngine(3, 10000.0, 300*time.Second, 60*time.Second)

	// 6. Start DEX Mock Feed Worker with 15 minutes interval
	ctx := context.Background()
	detector.StartMockFeed(ctx, clusterEngine, 15*time.Minute)

	// 7. Start Alert Broadcaster worker
	telegram.StartAlertBroadcaster(ctx, tgClient, db, clusterEngine.AlertsChan)

	// 8. Initialize webhook handler with storage
	webhookHandler := telegram.NewWebhookHandler(tgClient, db, cfg)

	// 9. Register HTTP routes
	mux := http.NewServeMux()
	mux.Handle("/", webhookHandler)
	mux.Handle("/webhook", webhookHandler)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	})

	// Serve static files from ./web on /app
	fileServer := http.FileServer(http.Dir("./web"))
	mux.Handle("/app/", http.StripPrefix("/app", fileServer))
	mux.HandleFunc("/app", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "./web/index.html")
	})

	// API endpoint returning cluster history as JSON
	mux.HandleFunc("/api/clusters", func(w http.ResponseWriter, r *http.Request) {
		clusters, err := db.GetRecentClusters(50)
		if err != nil {
			log.Printf("ERROR: failed to get recent clusters for API: %v", err)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(clusters)
	})

	// 10. Start server
	addr := ":" + cfg.Port
	log.Printf("INFO: Starting smart-cluster-bot server on port %s...", cfg.Port)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("FATAL: Server failed: %v", err)
	}
}
