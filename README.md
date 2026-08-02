# Smart Cluster Bot

A high-performance, zero-dependency Telegram bot written in Go (`golang`) designed for real-time cluster notifications, monitoring, and interactive management.

## Architecture & Features

- **High-Performance Go Architecture**: Built using idiomatic Go concurrency patterns, standard library networking, and highly optimized processing pipelines.
- **Zero-Dependency Philosophy**: Designed without heavy external frameworks or bloated libraries for lightning-fast startup times and minimal memory footprint.
- **Robust Storage**: Uses embedded `database/sql` with SQLite for reliable, lightweight, zero-config state persistence.
- **Localization Support**: Built-in multi-language support (`en`, `ru`) via clean JSON translation catalogs.
- **Docker Ready**: Multi-stage `Dockerfile` optimized for production deployments, resulting in a tiny container image.

---

## Configuration

Copy `.env.example` to `.env` and fill in your configuration values:

```env
BOT_TOKEN=your_telegram_bot_token_here
PORT=8080
WEBHOOK_URL=https://your-app-name.onrender.com/webhook
DATABASE_PATH=./data/bot.db
```

---

## Local Development & Running

### Prerequisites
- Go 1.21+
- Docker & Docker Compose (optional)

### Running Locally with Go
```bash
# Install dependencies
go mod download

# Run the bot
go run cmd/bot/main.go
```

### Running with Docker Compose
```bash
docker-compose up --build
```

---

## Deployment to Render.com

This repository is fully configured for deployment on [Render.com](https://render.com) using Docker.

1. Create a new **Web Service** on Render.
2. Connect your GitHub repository.
3. Set the Environment to **Docker**.
4. Add a persistent **Disk** mapped to `/app/data` to ensure SQLite database persistence across redeploys.
5. Configure the environment variables in the Render dashboard:
   - `BOT_TOKEN`
   - `PORT` (e.g., `8080`)
   - `WEBHOOK_URL` (your Render service URL with `/webhook`)
   - `DATABASE_PATH` (`./data/bot.db`)
6. Deploy!
