package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"log"
	"net/http"
	"strings"
	"time"

	"smart-cluster-bot/internal/config"
	"smart-cluster-bot/internal/detector"
	"smart-cluster-bot/internal/storage"
)

// rugCheckTimeout bounds every call to the RugCheck API. This is deliberately
// short — a slow safety check must never delay or block alert delivery.
const rugCheckTimeout = 4 * time.Second

// RugCheckReport is our decode target for RugCheck's summary endpoint.
// Field names are best-effort based on RugCheck's public summary schema;
// unknown/absent fields simply decode to zero values rather than erroring,
// so a schema drift degrades gracefully instead of crashing the broadcaster.
type RugCheckReport struct {
	Score           int    `json:"score"`
	MintAuthority   string `json:"mintAuthority"`
	FreezeAuthority string `json:"freezeAuthority"`
	TokenMeta       struct {
		Mutable bool `json:"mutable"`
	} `json:"tokenMeta"`
	Markets []struct {
		LP struct {
			LPLockedPct float64 `json:"lpLockedPct"`
		} `json:"lp"`
	} `json:"markets"`
	Risks []struct {
		Name  string `json:"name"`
		Level string `json:"level"`
	} `json:"risks"`
}

// minSafeLPLockedPct is the minimum acceptable percentage of locked
// liquidity across reported markets before we treat liquidity as "unlocked".
const minSafeLPLockedPct = 50.0

// checkRugCheck queries the RugCheck API for Solana tokens under a strict
// timeout. Returns (safetyBadge, shouldBlock, error).
//
// Hard-blocking conditions (shouldBlock=true):
//  1. Freeze authority present (non-empty, non-null) — token can be frozen.
//  2. Token metadata is mutable — symbol/name/image can change post-launch.
//  3. Liquidity is unlocked or locked below minSafeLPLockedPct.
//  4. Any risk entry reports level "danger" or "critical".
//
// Non-Solana chains and API hiccups (timeout, non-200, malformed JSON, rate
// limiting) never block — they degrade to an "UNVERIFIED" badge so the bot
// keeps functioning through network flakiness rather than freezing alerts.
func checkRugCheck(ctx context.Context, baseURL, chain, address string) (string, bool, error) {
	c := strings.ToLower(chain)
	if c != "solana" && c != "sol" {
		return "🛡 AUTO-RUGCHECK: PASSED (Non-Solana Chain)", false, nil
	}

	reqCtx, cancel := context.WithTimeout(ctx, rugCheckTimeout)
	defer cancel()

	url := fmt.Sprintf("%s/tokens/%s/report/summary", strings.TrimSuffix(baseURL, "/"), address)
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return "🛡 AUTO-RUGCHECK: UNVERIFIED (Request Error)", false, err
	}

	client := &http.Client{Timeout: rugCheckTimeout}
	resp, err := client.Do(req)
	if err != nil {
		// Timeout, DNS failure, connection refused, etc. — never block on this.
		return "🛡 AUTO-RUGCHECK: UNVERIFIED (API Timeout)", false, nil
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusTooManyRequests {
		return "🛡 AUTO-RUGCHECK: UNVERIFIED (Rate Limited 429)", false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Sprintf("🛡 AUTO-RUGCHECK: UNVERIFIED (API Status %d)", resp.StatusCode), false, nil
	}

	var report RugCheckReport
	if err := json.NewDecoder(resp.Body).Decode(&report); err != nil {
		return "🛡 AUTO-RUGCHECK: UNVERIFIED (Parse Error)", false, nil
	}

	// 1. Freeze authority enabled.
	if strings.TrimSpace(report.FreezeAuthority) != "" {
		return "🛑 RUGCHECK FAILED: FREEZE AUTHORITY ENABLED", true, nil
	}

	// 2. Mutable metadata.
	if report.TokenMeta.Mutable {
		return "🛑 RUGCHECK FAILED: MUTABLE METADATA", true, nil
	}

	// 3. Liquidity unlocked / below safe threshold.
	if len(report.Markets) > 0 {
		var maxLocked float64
		for _, m := range report.Markets {
			if m.LP.LPLockedPct > maxLocked {
				maxLocked = m.LP.LPLockedPct
			}
		}
		if maxLocked < minSafeLPLockedPct {
			return fmt.Sprintf("🛑 RUGCHECK FAILED: LIQUIDITY %.0f%% LOCKED (MIN %.0f%%)", maxLocked, minSafeLPLockedPct), true, nil
		}
	}

	// 4. Explicit danger/critical risk flags.
	for _, risk := range report.Risks {
		lvl := strings.ToLower(risk.Level)
		if lvl == "danger" || lvl == "critical" {
			return fmt.Sprintf("🛑 RUGCHECK FAILED: %s", strings.ToUpper(risk.Name)), true, nil
		}
	}

	// Overall high score is an additional soft signal even without a named risk.
	if report.Score > 5000 {
		return fmt.Sprintf("🛑 RUGCHECK FAILED: HIGH RISK SCORE (%d)", report.Score), true, nil
	}

	return "🛡 AUTO-RUGCHECK: PASSED", false, nil
}

// formatAlertMessage builds the rich HTML alert body sent to the admin.
// All dynamic content is HTML-escaped before embedding to prevent Telegram
// parse-mode panics on adversarial token names/symbols.
func formatAlertMessage(alert detector.ClusterAlert, safetyLine string, isWhaleMatch bool) string {
	chainEmo := chainEmoji(alert.Chain)
	volStr := fmtFloat(alert.TotalVolumeUSD)
	windowMin := alert.TimeWindowSeconds / 60
	if windowMin < 1 {
		windowMin = 1
	}

	avgEntry := alert.TotalVolumeUSD
	if alert.BuyCount > 0 {
		avgEntry = alert.TotalVolumeUSD / float64(alert.BuyCount)
	}

	entryBadge := "🟢 SAFE ENTRY (INITIAL ACCUMULATION)"
	if alert.TimeWindowSeconds > 120 {
		entryBadge = "⚠️ LATE ENTRY — HIGH RISK"
	}

	whaleLine := ""
	if isWhaleMatch {
		whaleLine = "\n🐋 <b>SEEDED WHALE MATCH!</b>"
	}

	return fmt.Sprintf(
		"<b>🚨 CLUSTER ALERT</b>%s\n\n"+
			"%s <b>%s</b> | <code>%s</code>\n\n"+
			"%s\n"+
			"%s\n\n"+
			"💰 Volume: <b>$%s</b>\n"+
			"👛 Wallets: <b>%d</b>\n"+
			"📊 Avg Entry: <b>$%s</b>\n"+
			"⏱ Window: <b>%d min</b>\n"+
			"📍 Lead Wallet: <code>%s</code>\n\n"+
			"🔗 Contract:\n<code>%s</code>",
		whaleLine,
		chainEmo,
		html.EscapeString(alert.TokenSymbol),
		html.EscapeString(alert.Chain),
		entryBadge,
		safetyLine,
		volStr,
		alert.BuyCount,
		fmtFloat(avgEntry),
		windowMin,
		html.EscapeString(maskAddr(alert.LeadWallet)),
		alert.TokenAddress,
	)
}

// alertKeyboard builds the interactive inline keyboard attached to every alert.
func alertKeyboard(alert detector.ClusterAlert) *InlineKeyboardMarkup {
	contract := alert.TokenAddress
	rows := [][]InlineKeyboardButton{
		{
			{Text: "⚡ Photon", URL: "https://photon-sol.tinyastro.io/en/r/@sniperbot/" + contract},
			{Text: "🎯 Trojan", URL: "https://t.me/solana_trojanbot?start=r-sniperbot-" + contract},
		},
		{
			{Text: "📊 DexScreener", URL: "https://dexscreener.com/solana/" + contract},
			{Text: "🛡 RugCheck", URL: "https://rugcheck.xyz/tokens/" + contract},
		},
	}
	if alert.LeadWallet != "" {
		rows = append(rows, []InlineKeyboardButton{
			{Text: "🐋 Add Whale", CallbackData: "cb:whale:add:" + alert.LeadWallet},
		})
	}
	return &InlineKeyboardMarkup{InlineKeyboard: rows}
}

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
// alertsChan and delivers them to the single admin chat. Wrapped in panic
// recovery — a bad alert or downstream failure must never kill the process.
func StartAlertBroadcaster(
	ctx context.Context,
	client *Client,
	store *storage.Storage,
	cfg *config.Config,
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
				safeBroadcastAlert(ctx, client, store, cfg, alert)
			}
		}
	}()
}

// safeBroadcastAlert wraps broadcastAlert with its own recover so a panic
// processing one alert doesn't kill the broadcaster goroutine for subsequent
// alerts (the outer recover only protects the loop from a single fatal exit).
func safeBroadcastAlert(ctx context.Context, client *Client, store *storage.Storage, cfg *config.Config, alert detector.ClusterAlert) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[PANIC RECOVER] broadcastAlert recovered: %v", r)
		}
	}()
	broadcastAlert(ctx, client, store, cfg, alert)
}

func broadcastAlert(ctx context.Context, client *Client, store *storage.Storage, cfg *config.Config, alert detector.ClusterAlert) {
	rugCheckBase := "https://api.rugcheck.xyz/v1"
	if cfg != nil && cfg.RugCheckBaseURL != "" {
		rugCheckBase = cfg.RugCheckBaseURL
	}

	safetyLine, shouldBlock, err := checkRugCheck(ctx, rugCheckBase, alert.Chain, alert.TokenAddress)
	if err != nil {
		log.Printf("[RUGCHECK] error checking %s (%s): %v", alert.TokenSymbol, alert.TokenAddress, err)
	}
	if shouldBlock {
		log.Printf("[BROADCASTER] blocked dangerous token %s (%s): %s", alert.TokenSymbol, alert.TokenAddress, safetyLine)
		return
	}

	if err := store.SaveCluster(
		alert.TokenAddress, alert.TokenSymbol, alert.Chain,
		alert.BuyCount, alert.TotalVolumeUSD, alert.TimeWindowSeconds,
		alert.LeadWallet,
	); err != nil {
		log.Printf("[BROADCASTER] SaveCluster: %v", err)
	}

	isWhaleMatch := false
	if alert.LeadWallet != "" {
		if match, err := store.IsSmartWallet(alert.LeadWallet); err == nil {
			isWhaleMatch = match
		}
	}

	if cfg == nil || cfg.AdminChatID == 0 {
		log.Printf("[BROADCASTER] no AdminChatID configured — dropping alert for %s", alert.TokenSymbol)
		return
	}

	msg := formatAlertMessage(alert, safetyLine, isWhaleMatch)
	kb := alertKeyboard(alert)
	if err := client.SendMessageWithKeyboard(cfg.AdminChatID, msg, kb); err != nil {
		log.Printf("[BROADCASTER] send to admin %d: %v", cfg.AdminChatID, err)
	}
}

// StartDailyDigest schedules a daily summary message sent to the admin at 09:00 UTC.
func StartDailyDigest(ctx context.Context, client *Client, store *storage.Storage, cfg *config.Config) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[PANIC RECOVER] StartDailyDigest recovered: %v", r)
			}
		}()
		for {
			next := nextDailyDigestTime()
			log.Printf("[DIGEST] next daily digest at %s", next.Format(time.RFC3339))

			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Until(next)):
				safeSendDailyDigest(client, store, cfg)
			}
		}
	}()
}

func safeSendDailyDigest(client *Client, store *storage.Storage, cfg *config.Config) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[PANIC RECOVER] sendDailyDigest recovered: %v", r)
		}
	}()
	sendDailyDigest(client, store, cfg)
}

func nextDailyDigestTime() time.Time {
	now := time.Now().UTC()
	next := time.Date(now.Year(), now.Month(), now.Day(), 9, 0, 0, 0, time.UTC)
	if !next.After(now) {
		next = next.Add(24 * time.Hour)
	}
	return next
}

func sendDailyDigest(client *Client, store *storage.Storage, cfg *config.Config) {
	if cfg == nil || cfg.AdminChatID == 0 {
		log.Printf("[DIGEST] no AdminChatID configured — skipping")
		return
	}

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
	sb.WriteString("<b>📰 Daily Digest — Solo Sniper Station</b>\n")
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
	}

	kb := &InlineKeyboardMarkup{
		InlineKeyboard: [][]InlineKeyboardButton{
			{{Text: "📡 Active Clusters", CallbackData: "cb:clusters"}},
		},
	}
	if err := client.SendMessageWithKeyboard(cfg.AdminChatID, sb.String(), kb); err != nil {
		log.Printf("[DIGEST] send to admin %d: %v", cfg.AdminChatID, err)
	}
	log.Printf("[DIGEST] sent to admin")
}
