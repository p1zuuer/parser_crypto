package telegram

import (
	"context"
	"fmt"
	"html"
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

// dexScreenerURL builds the DEXScreener chart URL for a token address.
func dexScreenerURL(chain, addr string) string {
	return fmt.Sprintf("https://dexscreener.com/%s/%s", strings.ToLower(chain), addr)
}

// birdeyeURL builds the Birdeye / GMGN alternative chart URL.
func birdeyeURL(chain, addr string) string {
	if strings.EqualFold(chain, "solana") || strings.EqualFold(chain, "sol") {
		return "https://birdeye.so/token/" + addr + "?chain=solana"
	}
	return "https://gmgn.ai/" + strings.ToLower(chain) + "/token/" + addr
}

// contractCheckURL returns a Rugcheck / Safety check URL.
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
		return true
	}
}

// formatAlertMessage builds the rich HTML alert body sent to users.
func formatAlertMessage(alert detector.ClusterAlert) string {
	chainEmoji := chainEmoji(alert.Chain)
	volStr := fmtFloat(alert.TotalVolumeUSD)
	windowMin := alert.TimeWindowSeconds / 60
	if windowMin < 1 {
		windowMin = 1
	}

	avgEntry := alert.TotalVolumeUSD / float64(alert.BuyCount)
	if alert.BuyCount == 0 {
		avgEntry = alert.TotalVolumeUSD
	}

	badge := "🟢 STAGE: INITIAL ACCUMULATION (SAFE ENTRY)"
	if alert.TimeWindowSeconds > 120 {
		badge = "⚠️ STAGE: LATE ENTRY - HIGH RISK"
	}

	return fmt.Sprintf(
		"<b>🚨 CLUSTER & INTELLIGENCE ALERT!</b>\n\n"+
			"%s <b>%s</b> | <code>%s</code>\n\n"+
			"%s\n\n"+
			"💰 Total Cluster Volume: <b>$%s</b>\n"+
			"👛 Smart Wallets Involved: <b>%d</b>\n"+
			"📊 Average Entry Price: <b>$%s</b>\n"+
			"⏱ Time Window: <b>%d min</b>\n"+
			"📍 Lead Wallet: <code>%s</code>\n\n"+
			"🔗 Contract Address:\n<code>%s</code>",
		chainEmoji,
		html.EscapeString(alert.TokenSymbol),
		html.EscapeString(alert.Chain),
		badge,
		html.EscapeString(volStr),
		alert.BuyCount,
		html.EscapeString(fmtFloat(avgEntry)),
		windowMin,
		html.EscapeString(maskAddr(alert.LeadWallet)),
		alert.TokenAddress,
	)
}

// alertKeyboard builds the interactive inline keyboard attached to every alert with analytical links and 1-click buy buttons.
func alertKeyboard(alert detector.ClusterAlert) *InlineKeyboardMarkup {
	contract := alert.TokenAddress
	return &InlineKeyboardMarkup{
		InlineKeyboard: [][]InlineKeyboardButton{
			{
				{Text: "⚡ Buy on Photon", URL: "https://photon-sol.tinyastro.io/en/r/@clusterbot/" + contract},
				{Text: "🎯 Buy via Trojan", URL: "https://t.me/solana_trojanbot?start=r-clusterbot-" + contract},
			},
			{
				{Text: "📊 DexScreener", URL: "https://dexscreener.com/solana/" + contract},
				{Text: "🛡 RugCheck", URL: "https://rugcheck.xyz/tokens/" + contract},
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

// StartAlertBroadcaster runs as a goroutine that consumes alerts from
// alertsChan and fans them out to every qualifying user in the database.
func StartAlertBroadcaster(
	ctx context.Context,
	client *Client,
	store *storage.Storage,
	alertsChan <-chan detector.ClusterAlert,
) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[PANIC RECOVER] StartAlertBroadcaster recovered: %v", r)
			}
		}()
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

	notified := make(map[int64]struct{}, len(users))

	for _, u := range users {
		if !u.IsVIP {
			// Free tier: check min volume $25,000+ or default, max 5 live alerts per day
			minVol := u.MinVolume
			if minVol < 25000 {
				minVol = 25000
			}
			if float64(minVol) > alert.TotalVolumeUSD {
				continue
			}
			allowed, err := store.CheckAndIncrementFreeAlert(u.UserID, 5)
			if err != nil {
				log.Printf("[BROADCASTER] CheckAndIncrementFreeAlert %d: %v", u.UserID, err)
				continue
			}
			if !allowed {
				continue
			}
		} else {
			if float64(u.MinVolume) > alert.TotalVolumeUSD {
				continue
			}
		}

		if !chainNetworkMatch(alert.Chain, u) {
			continue
		}

		sendMsg := msg
		if !u.IsVIP {
			sendMsg += "\n\n<i>💡 Free Plan: Live alert preview. Upgrade to VIP for unlimited $10k+ alerts & custom watchlist!</i>"
		}

		if err := client.SendMessageWithKeyboard(u.UserID, sendMsg, kb); err != nil {
			log.Printf("[BROADCASTER] send to %d: %v", u.UserID, err)
		}
		notified[u.UserID] = struct{}{}

		time.Sleep(20 * time.Millisecond)
	}

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
			continue
		}
		watchMsg := fmt.Sprintf(
			"<b>🎯 Watchlist Hit!</b>\n\n"+
				"Monitored wallet <code>%s</code> just bought <b>%s</b> on <b>%s</b>!\n\n"+
				"💰 Deal Volume: <b>$%s</b>\n"+
				"🔗 Contract:\n<code>%s</code>",
			html.EscapeString(maskAddr(alert.LeadWallet)),
			html.EscapeString(alert.TokenSymbol),
			html.EscapeString(alert.Chain),
			html.EscapeString(fmtFloat(alert.TotalVolumeUSD)),
			alert.TokenAddress,
		)
		if err := client.SendMessageWithKeyboard(uid, watchMsg, kb); err != nil {
			log.Printf("[BROADCASTER] watchlist ping %d: %v", uid, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// StartDailyDigest schedules a daily summary message sent to every user at 09:00 UTC.
func StartDailyDigest(ctx context.Context, client *Client, store *storage.Storage) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[PANIC RECOVER] StartDailyDigest recovered: %v", r)
			}
		}()
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

	var sb strings.Builder
	sb.WriteString("<b>📰 Daily Digest — Smart Cluster Terminal</b>\n")
	sb.WriteString(fmt.Sprintf("🗓 %s UTC\n\n", time.Now().UTC().Format("02 Jan 2006")))

	sb.WriteString("📊 <b>24h Summary:</b>\n")
	if stats != nil {
		sb.WriteString(fmt.Sprintf(
			"• Clusters: <b>%d</b>\n• Volume: <b>$%s</b>\n• Top Token: <b>%s</b>\n• Top Chain: <b>%s</b>\n\n",
			stats.TotalClusters,
			html.EscapeString(fmtFloat(stats.TotalVolumeUSD)),
			html.EscapeString(or(stats.TopToken, "—")),
			html.EscapeString(or(stats.TopChain, "—")),
		))
	} else {
		sb.WriteString("<i>No data</i>\n\n")
	}

	if len(clusters) > 0 {
		sb.WriteString("🔥 <b>Top Clusters:</b>\n")
		for i, c := range clusters {
			sb.WriteString(fmt.Sprintf(
				"%d. <b>%s</b> (%s) — $%s · %d wallets\n<code>%s</code>\n",
				i+1,
				html.EscapeString(c.TokenSymbol),
				html.EscapeString(c.Chain),
				html.EscapeString(fmtFloat(c.TotalVolumeUSD)),
				c.BuyCount,
				c.TokenAddress,
			))
		}
		sb.WriteString("\n")
	}

	if len(hotWallets) > 0 {
		sb.WriteString("🔥 <b>Hot Wallets:</b>\n")
		for _, w := range hotWallets {
			sb.WriteString(fmt.Sprintf(
				"• <code>%s</code> — %d clusters · $%s\n",
				html.EscapeString(maskAddr(w.WalletAddress)),
				w.ClusterCount,
				html.EscapeString(fmtFloat(w.TotalVolumeUSD)),
			))
		}
		sb.WriteString("\n")
	}

	sb.WriteString("<i>Open the terminal for complete intelligence.</i>")
	digestMsg := sb.String()

	digestKB := &InlineKeyboardMarkup{
		InlineKeyboard: [][]InlineKeyboardButton{
			{{Text: "📊 Open Terminal", CallbackData: "cb:clusters"}},
			{{Text: "📈 Full Stats", CallbackData: "cb:stats"}},
		},
	}

	users, err := store.GetAllUsers()
	if err != nil {
		log.Printf("[DIGEST] GetAllUsers: %v", err)
		return
	}
	for _, u := range users {
		if err := client.SendMessageWithKeyboard(u.UserID, digestMsg, digestKB); err != nil {
			log.Printf("[DIGEST] send to %d: %v", u.UserID, err)
		}
		time.Sleep(30 * time.Millisecond)
	}
	log.Printf("[DIGEST] Done — sent to %d users", len(users))
}
