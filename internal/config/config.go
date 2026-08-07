// Package config loads runtime configuration from environment variables for
// the solo trading sniper station.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Config holds all environment-derived settings the bot needs to run.
type Config struct {
	// BotToken is the Telegram Bot API token.
	BotToken string
	// AdminChatID is the single authorized user's Telegram chat/user ID.
	// The bot ignores updates from any other chat.
	AdminChatID int64
	// Port is the local TCP port the HTTP server listens on.
	Port string
	// WebhookURL is the public HTTPS URL Telegram POSTs updates to.
	WebhookURL string
	// RenderURL is the public base URL of the service (e.g. on Render.com).
	RenderURL string
	// WebAppURL is an explicit override for the Mini App URL.
	WebAppURL string
	// DatabasePath is the filesystem path for the SQLite database file.
	DatabasePath string
	// HTTPTimeoutSeconds bounds all outbound network calls (Telegram, RugCheck, RPC).
	HTTPTimeoutSeconds int
	// RugCheckBaseURL allows pointing at a mock/staging RugCheck instance.
	RugCheckBaseURL string
	// SolPrivateKey is the base58-encoded Solana wallet private key used to
	// sign auto-buy transactions. Empty disables auto-buy entirely.
	SolPrivateKey string
	// SolanaRPCURL is the RPC endpoint used to broadcast signed transactions.
	SolanaRPCURL string
	// AutoBuyAmountUSD is the approximate USD size of every auto-buy trade.
	AutoBuyAmountUSD float64
	// AutoBuyEnabled gates whether the broadcaster ever attempts an auto-buy,
	// independent of whether a private key is configured (extra safety switch).
	AutoBuyEnabled bool
	// HeliusWebhookSecret, if set, is checked against the "Authorization"
	// header on incoming Helius webhook requests to reject unauthenticated calls.
	HeliusWebhookSecret string
	// SimulationMode, when true, records trades in the DB and tracks TP/SL
	// using real prices but never signs or broadcasts real transactions.
	// Default: true — must be explicitly set to false to go live.
	SimulationMode bool
}

const (
	defaultPort            = "8080"
	defaultDB              = "./data/bot.db"
	defaultHTTPTimeoutSecs = 5
	defaultRugCheckBase    = "https://api.rugcheck.xyz/v1"
)

// Load reads all required and optional env vars and returns a Config.
// BOT_TOKEN and ADMIN_CHAT_ID are required; everything else has a safe default.
func Load() (*Config, error) {
	cfg := &Config{
		BotToken:        os.Getenv("BOT_TOKEN"),
		Port:            os.Getenv("PORT"),
		WebhookURL:      os.Getenv("WEBHOOK_URL"),
		RenderURL:       coalesce("RENDER_EXTERNAL_URL", "RENDER_URL"),
		WebAppURL:       os.Getenv("WEBAPP_URL"),
		DatabasePath:    os.Getenv("DATABASE_PATH"),
		RugCheckBaseURL: os.Getenv("RUGCHECK_BASE_URL"),
	}

	if cfg.BotToken == "" {
		return nil, fmt.Errorf("config: BOT_TOKEN environment variable is required")
	}

	adminStr := os.Getenv("ADMIN_CHAT_ID")
	if adminStr == "" {
		return nil, fmt.Errorf("config: ADMIN_CHAT_ID environment variable is required (solo bot — this is the only chat it will respond to)")
	}
	adminID, err := strconv.ParseInt(strings.TrimSpace(adminStr), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("config: ADMIN_CHAT_ID must be a valid integer: %w", err)
	}
	cfg.AdminChatID = adminID

	if cfg.Port == "" {
		cfg.Port = defaultPort
	}
	if cfg.DatabasePath == "" {
		cfg.DatabasePath = defaultDB
	}
	if cfg.RugCheckBaseURL == "" {
		cfg.RugCheckBaseURL = defaultRugCheckBase
	}

	cfg.HTTPTimeoutSeconds = defaultHTTPTimeoutSecs
	if v := os.Getenv("HTTP_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.HTTPTimeoutSeconds = n
		}
	}

	cfg.SolPrivateKey = os.Getenv("SOL_PRIVATE_KEY")
	cfg.SolanaRPCURL = os.Getenv("SOLANA_RPC_URL")
	if cfg.SolanaRPCURL == "" {
		cfg.SolanaRPCURL = "https://api.mainnet-beta.solana.com"
	}
	cfg.HeliusWebhookSecret = os.Getenv("HELIUS_WEBHOOK_SECRET")

	cfg.AutoBuyAmountUSD = 1.5
	if v := os.Getenv("AUTO_BUY_AMOUNT_USD"); v != "" {
		if n, err := strconv.ParseFloat(v, 64); err == nil && n > 0 {
			cfg.AutoBuyAmountUSD = n
		}
	}
	// Auto-buy requires an explicit opt-in AND a private key — either alone
	// is not enough to start signing real transactions.
	cfg.AutoBuyEnabled = strings.EqualFold(os.Getenv("AUTO_BUY_ENABLED"), "true") && cfg.SolPrivateKey != ""

	// SimulationMode defaults to TRUE for safety — you must explicitly set
	// SIMULATION_MODE=false to execute real on-chain transactions.
	cfg.SimulationMode = !strings.EqualFold(os.Getenv("SIMULATION_MODE"), "false")

	// Normalise RenderURL: strip trailing slash so callers can always do cfg.RenderURL+"/app".
	cfg.RenderURL = strings.TrimSuffix(cfg.RenderURL, "/")

	return cfg, nil
}

// coalesce returns the value of the first non-empty env var in the list.
func coalesce(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}
