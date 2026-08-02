package telegram

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"smart-cluster-bot/internal/detector"
	"smart-cluster-bot/internal/storage"
)

// explorerURL returns a block-explorer link for the given chain and address.
func explorerURL(chain, addr string) string {
	switch strings.ToLower(chain) {
	case "ethereum", "eth":
		return "https://etherscan.io/token/" + addr
	case "solana", "sol":
		return "https://solscan.io/token/" + addr
	case "base":
		return "https://basescan.org/token/" + addr
	case "bsc", "bnb":
		return "https://bscscan.com/token/" + addr
	default:
		return "https://etherscan.io/token/" + addr
	}
}

// dexScreenerURL builds the DEXScreener search URL for a token symbol.
func dexScreenerURL(symbol string) string {
	return "https://dexscreener.com/search?q=" + symbol
}

// trojanURL builds a Trojan Bot deep-link for quick buying.
func trojanURL(tokenAddr string) string {
	return "https://t.me/heymaestro_bot?start=buy_" + tokenAddr
}

// contractCheckURL returns a Rugcheck / DeFi-guard URL.
func contractCheckURL(chain, addr string) string {
	switch strings.ToLower(chain) {
	case "solana", "sol":
		return "https://rugcheck.xyz/tokens/" + addr
	default:
		return "https://app.staysafu.org/scan?ca=" + addr
	}
}

// chainNetworkMatch returns true when the alert's chain matches a user's
// enabled-network preferences.
func chainNetworkMatch(chain string, u storage.User) bool {
	c := strings.ToLower(chain)
	switch {
	case strings.Contains(c, "eth") || c == "ethereum":
		return u.EthEnabled
	case strings.Contains(c, "sol") || c == "solana":
		return u.SolEnabled
	case c == "base":
		return u.BaseEnabled
	case strings.Contains(c, "bsc") || c == "bnb":
		return u.BscEnabled
	default:
		return true // unknown chain — let it through rather than silently drop
	}
}

// formatAlertMessage builds the rich Markdown alert body sent to users.
func formatAlertMessage(alert detector.ClusterAlert) string {
	chainEmoji := chainEmoji(alert.Chain)
	volStr := fmtFloat(alert.TotalVolumeUSD)
	windowMin := alert.TimeWindowSeconds / 60

	return fmt.Sprintf(
		"🚨 *КЛАСТЕР ОБНАРУЖЕН\\!*\n\n"+
			"%s *%s* \\| %s\n\n"+
			"💰 Объём: *$%s*\n"+
			"👛 Кошельков: *%d*\n"+
			"⏱ Окно: *%d мин*\n"+
			"📍 Ведущий кошелёк: `%s`\n\n"+
			"🔗 Контракт: `%s`",
		chainEmoji,
		escMD(alert.TokenSymbol),
		escMD(alert.Chain),
		escMD(volStr),
		alert.BuyCount,
		windowMin,
		escMD(maskAddr(alert.LeadWallet)),
		escMD(maskAddr(alert.TokenAddress)),
	)
}

// alertKeyboard builds the interactive inline keyboard attached to every alert.
func alertKeyboard(alert detector.ClusterAlert) *InlineKeyboardMarkup {
	return &InlineKeyboardMarkup{
		InlineKeyboard: [][]InlineKeyboardButton{
			{
				{Text: "📈 DEXScreener", URL: dexScreenerURL(alert.TokenSymbol)},
				{Text: "⚡ Quick Buy", URL: trojanURL(alert.TokenAddress)},
			},
			{
				{Text: "🛡 Contract Check", URL: contractCheckURL(alert.Chain, alert.TokenAddress)},
				{Text: "🔍 Explorer", URL: explorerURL(alert.Chain, alert.TokenAddress)},
			},
			{
				{Text: "⭐ Track Wallet", CallbackData: "cb:watchlist"},
			},
		},
	}
}

// chainEmoji maps a chain name to a display emoji.
func chainEmoji(chain string) string {
	switch strings.ToLower(chain) {
	case "ethereum", "eth":
		return "⬡"
	case "solana", "sol":
		return "◎"
	case "base":
		return "🔵"
	case "bsc", "bnb":
		return "🟡"
	default:
		return "🌐"
	}
}

// ── StartAlertBroadcaster ──────────────────────────────────────────────────────

// StartAlertBroadcaster runs as a goroutine that consumes alerts from
// alertsChan and fans them out to every qualifying user in the database.
//
// Per-user filtering rules:
//   - alert.TotalVolumeUSD must be >= user.MinVolume
//   - alert.Chain must match one of the user's enabled networks
//
// Additionally, if the alert's LeadWallet appears in any user's watchlist,
// that user receives a personalised "🎯 Watchlist Hit" ping regardless of
// their volume/network filters.
func StartAlertBroadcaster(
	ctx context.Context,
	client *Client,
	store *storage.Storage,
	alertsChan <-chan detector.ClusterAlert,
) {
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case alert, ok := <-alertsChan:
				if !ok {
					return
				}
				broadcastAlert(client, store, alert)
			}
		}
	}()
}

func broadcastAlert(client *Client, store *storage.Storage, alert detector.ClusterAlert) {
	// Persist the cluster to the database first.
	if err := store.SaveCluster(
		alert.TokenAddress, alert.TokenSymbol, alert.Chain,
		alert.BuyCount, alert.TotalVolumeUSD, alert.TimeWindowSeconds,
		alert.LeadWallet,
	); err != nil {
		log.Printf("[BROADCASTER] SaveCluster: %v", err)
	}

	users, err := store.GetAllUsers()
	if err != nil {
		log.Printf("[BROADCASTER] GetAllUsers: %v", err)
		return
	}

	msg := formatAlertMessage(alert)
	kb := alertKeyboard(alert)

	// Set of users already notified (avoid double-ping from watchlist logic).
	notified := make(map[int64]struct{}, len(users))

	for _, u := range users {
		if float64(u.MinVolume) > alert.TotalVolumeUSD {
			continue
		}
		if !chainNetworkMatch(alert.Chain, u) {
			continue
		}
		if err := client.SendMessageWithKeyboard(u.UserID, msg, kb); err != nil {
			log.Printf("[BROADCASTER] send to %d: %v", u.UserID, err)
		}
		notified[u.UserID] = struct{}{}

		// Throttle: 20 ms between sends to avoid hitting Telegram's 30 msg/s limit.
		time.Sleep(20 * time.Millisecond)
	}

	// Watchlist personalised pings ──────────────────────────────────────────
	// If the lead wallet is in someone's watchlist, ping them even if they
	// didn't qualify by volume/network filters above.
	if alert.LeadWallet == "" {
		return
	}
	watchUsers, err := store.GetWatchlistUsersByWallet(alert.LeadWallet)
	if err != nil {
		log.Printf("[BROADCASTER] GetWatchlistUsersByWallet: %v", err)
		return
	}
	for _, uid := range watchUsers {
		if _, already := notified[uid]; already {
			continue // they already got the standard alert
		}
		watchMsg := fmt.Sprintf(
			"🎯 *Watchlist Hit\\!*\n\n"+
				"Отслеживаемый кошелёк `%s` только что купил *%s* на *%s*\\!\n\n"+
				"💰 Объём сделки: *$%s*",
			escMD(maskAddr(alert.LeadWallet)),
			escMD(alert.TokenSymbol),
			escMD(alert.Chain),
			escMD(fmtFloat(alert.TotalVolumeUSD)),
		)
		if err := client.SendMessageWithKeyboard(uid, watchMsg, kb); err != nil {
			log.Printf("[BROADCASTER] watchlist ping %d: %v", uid, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// ── StartDailyDigest (Bonus Feature #2) ───────────────────────────────────────

// StartDailyDigest schedules a daily summary message sent to every user at
// 09:00 UTC. The digest includes the top 3 clusters from the previous 24 h,
// total volume, and the hottest wallet addresses — providing passive value
// even to users who missed real-time alerts.
func StartDailyDigest(ctx context.Context, client *Client, store *storage.Storage) {
	go func() {
		for {
			next := nextDailyDigestTime()
			log.Printf("[DIGEST] Next daily digest scheduled at %s", next.Format(time.RFC3339))

			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Until(next)):
				sendDailyDigest(client, store)
			}
		}
	}()
}

// nextDailyDigestTime returns the next 09:00 UTC moment.
func nextDailyDigestTime() time.Time {
	now := time.Now().UTC()
	next := time.Date(now.Year(), now.Month(), now.Day(), 9, 0, 0, 0, time.UTC)
	if !next.After(now) {
		next = next.Add(24 * time.Hour)
	}
	return next
}

func sendDailyDigest(client *Client, store *storage.Storage) {
	log.Printf("[DIGEST] Sending daily digest to all users")

	stats, err := store.GetStats24h()
	if err != nil {
		log.Printf("[DIGEST] GetStats24h: %v", err)
		return
	}

	clusters, err := store.GetRecentClusters(3)
	if err != nil {
		log.Printf("[DIGEST] GetRecentClusters: %v", err)
		return
	}

	hotWallets, err := store.GetTopWallets(24, 3)
	if err != nil {
		log.Printf("[DIGEST] GetTopWallets: %v", err)
		return
	}

	// ── Build digest message ─────────────────────────────────────────────────
	var sb strings.Builder
	sb.WriteString("📰 *Ежедневный дайджест — Smart Cluster Terminal*\n")
	sb.WriteString(fmt.Sprintf("🗓 %s UTC\n\n", time.Now().UTC().Format("02 Jan 2006")))

	// Stats
	sb.WriteString("📊 *Сводка за 24 часа:*\n")
	if stats != nil {
		sb.WriteString(fmt.Sprintf(
			"• Кластеров: *%d*\n• Объём: *$%s*\n• Топ токен: *%s*\n• Топ сеть: *%s*\n\n",
			stats.TotalClusters,
			escMD(fmtFloat(stats.TotalVolumeUSD)),
			escMD(or(stats.TopToken, "—")),
			escMD(or(stats.TopChain, "—")),
		))
	} else {
		sb.WriteString("_Нет данных_\n\n")
	}

	// Top clusters
	if len(clusters) > 0 {
		sb.WriteString("🔥 *Топ кластеры:*\n")
		for i, c := range clusters {
			sb.WriteString(fmt.Sprintf(
				"%d\\. *%s* \\(%s\\) — $%s · %d кошельков\n",
				i+1,
				escMD(c.TokenSymbol),
				escMD(c.Chain),
				escMD(fmtFloat(c.TotalVolumeUSD)),
				c.BuyCount,
			))
		}
		sb.WriteString("\n")
	}

	// Hot wallets
	if len(hotWallets) > 0 {
		sb.WriteString("🔥 *Горячие кошельки:*\n")
		for _, w := range hotWallets {
			sb.WriteString(fmt.Sprintf(
				"• `%s` — %d кластеров · $%s\n",
				escMD(maskAddr(w.WalletAddress)),
				w.ClusterCount,
				escMD(fmtFloat(w.TotalVolumeUSD)),
			))
		}
		sb.WriteString("\n")
	}

	sb.WriteString("_Откройте терминал для полной картины\\._")
	digestMsg := sb.String()

	digestKB := &InlineKeyboardMarkup{
		InlineKeyboard: [][]InlineKeyboardButton{
			{{Text: "📊 Открыть Terminal", CallbackData: "cb:clusters"}},
			{{Text: "📈 Полная статистика", CallbackData: "cb:stats"}},
		},
	}

	// Fan out to all users.
	users, err := store.GetAllUsers()
	if err != nil {
		log.Printf("[DIGEST] GetAllUsers: %v", err)
		return
	}
	for _, u := range users {
		if err := client.SendMessageWithKeyboard(u.UserID, digestMsg, digestKB); err != nil {
			log.Printf("[DIGEST] send to %d: %v", u.UserID, err)
		}
		time.Sleep(30 * time.Millisecond) // stay well under Telegram's rate limit
	}
	log.Printf("[DIGEST] Done — sent to %d users", len(users))
}
