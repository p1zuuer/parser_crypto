package config

import (
	"os"
	"strings"
)

// Config holds all configuration values for the application.
type Config struct {
	BotToken     string
	Port         string
	WebhookURL   string
	DatabasePath string
	RenderURL    string
}

// Load loads configuration from environment variables with sensible defaults.
func Load() *Config {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	dbPath := os.Getenv("DATABASE_PATH")
	if dbPath == "" {
		dbPath = "./data/bot.db"
	}

	webhookURL := os.Getenv("WEBHOOK_URL")
	if webhookURL == "" {
		webhookURL = os.Getenv("RENDER_EXTERNAL_URL")
	}
	if webhookURL == "" {
		webhookURL = os.Getenv("RENDER_URL")
	}

	var renderURL string
	if webhookURL != "" {
		renderURL = strings.TrimSuffix(webhookURL, "/")
	} else {
		renderURL = "http://localhost:" + port
	}

	return &Config{
		BotToken:     os.Getenv("BOT_TOKEN"),
		Port:         port,
		WebhookURL:   webhookURL,
		DatabasePath: dbPath,
		RenderURL:    renderURL,
	}
}
