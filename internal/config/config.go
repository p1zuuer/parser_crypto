package config

import (
	"os"
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

	renderURL := os.Getenv("RENDER_URL")
	if renderURL == "" {
		renderURL = os.Getenv("WEBHOOK_URL")
		if renderURL == "" {
			renderURL = "https://smart-cluster-bot.onrender.com"
		}
	}

	return &Config{
		BotToken:     os.Getenv("BOT_TOKEN"),
		Port:         port,
		WebhookURL:   os.Getenv("WEBHOOK_URL"),
		DatabasePath: dbPath,
		RenderURL:    renderURL,
	}
}
