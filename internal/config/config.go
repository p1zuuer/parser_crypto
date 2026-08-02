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

	return &Config{
		BotToken:     os.Getenv("BOT_TOKEN"),
		Port:         port,
		WebhookURL:   os.Getenv("WEBHOOK_URL"),
		DatabasePath: dbPath,
	}
}
