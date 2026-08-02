// Package config loads runtime configuration from environment variables.
package config

import (
	"fmt"
	"os"
	"strings"
)

// Config holds all environment-derived settings the bot needs to run.
type Config struct {
	// BotToken is the Telegram Bot API token.
	BotToken string
	// Port is the local TCP port the HTTP server listens on.
	Port string
	// WebhookURL is the public HTTPS URL Telegram POSTs updates to.
	WebhookURL string
	// RenderURL is the public base URL of the service (e.g. on Render.com).
	// Used to construct WebApp deep-links sent to users.
	RenderURL string
	// DatabasePath is the filesystem path for the SQLite database file.
	DatabasePath string
}

const defaultPort = "8080"
const defaultDB = "./data/bot.db"

// Load reads all required and optional env vars and returns a Config.
// Only BOT_TOKEN is strictly required; everything else has a safe default.
func Load() (*Config, error) {
	cfg := &Config{
		BotToken:     os.Getenv("BOT_TOKEN"),
		Port:         os.Getenv("PORT"),
		WebhookURL:   os.Getenv("WEBHOOK_URL"),
		RenderURL:    coalesce("RENDER_EXTERNAL_URL", "RENDER_URL"),
		DatabasePath: os.Getenv("DATABASE_PATH"),
	}

	if cfg.BotToken == "" {
		return nil, fmt.Errorf("config: BOT_TOKEN environment variable is required")
	}
	if cfg.Port == "" {
		cfg.Port = defaultPort
	}
	if cfg.DatabasePath == "" {
		cfg.DatabasePath = defaultDB
	}
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
