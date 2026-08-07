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
	"smart-cluster-bot/internal/trading"
)

// rugCheckTimeout bounds every call to the RugCheck API. This is deliberately
// short — a slow safety check must never delay or block alert delivery.
const rugCheckTimeout = 4 * time.Second

// autoBuyTimeout bounds the entire quote → sign → broadcast pipeline for a
// single auto-buy attempt.
const autoBuyTimeout = 15 * time.Second

// RugCheckReport is our decode target for RugCheck's summary endpoint.
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

const minSafeLPLockedPct = 50.0

// checkRugCheck queries the RugCheck API for Solana tokens under a strict
// timeout. Returns (safetyLine, shouldBlock, error). Non-Solana chains and
// API hiccups never block — they degrade to an "unverified" state so the
// bot keeps functioning through network flakiness.
func checkRugCheck(ctx context.Context, baseURL, chain, address string) (string, bool, error) {
	c := strings.ToLower(chain)
	if c != "solana" && c != "sol" {
		return "RugCheck: passed (non-Solana chain)", false, nil
	}

	reqCtx, cancel := context.WithTimeout(ctx, rugCheckTimeout)
	defer cancel()

	url := fmt.Sprintf("%s/tokens/%s/report/summary", strings.TrimSuffix(baseURL, "/"), address)
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return "RugCheck: unverified (request error)", false, err
	}

	client := &http.Client{Timeout: rugCheckTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return "RugCheck: unverified (API timeout)", false, nil
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusTooManyRequests {
		return "RugCheck: unverified (rate limited)", false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Sprintf("RugCheck: unverified (API status %d)", resp.StatusCode), false, nil
	}

	var report RugCheckReport
	if err := json.NewDecoder(resp.Body).Decode(&report); err != nil {
		return "RugCheck: unverified (parse error)", false, nil
	}

	if strings.TrimSpace(report.MintAuthority) != "" {
		return "RugCheck: FAILED — mint authority enabled (tokens can be minted)", true, nil
	}
	if strings.TrimSpace(report.FreezeAuthority) != "" {
		return "RugCheck: FAILED — freeze authority enabled", true, nil
	}
	if report.TokenMeta.Mutable {
		return "RugCheck: FAILED — mutable metadata", true, nil
	}
	if len(report.Markets) > 0 {
		var maxLocked float64
		for _, m := range report.Markets {
			if m.LP.LPLockedPct > maxLocked {
				maxLocked = m.LP.LPLockedPct
			}
		}
		if maxLocked < minSafeLPLockedPct {
			return fmt.Sprintf("RugCheck: FAILED — only %.0f%% liquidity locked (min %.0f%%)", maxLocked, minSafeLPLockedPct), true, nil
		}
	}
	for _, risk := range report.Risks {
		lvl := strings.ToLower(risk.Level)
		if lvl == "danger" || lvl == "critical" {
			return fmt.Sprintf("RugCheck: FAILED — %s", risk.Name), true, nil
		}
	}
	if report.Score > 5000 {
		return fmt.Sprintf("RugCheck: FAILED — high risk score (%d)", report.Score), true, nil
	}

	return "RugCheck: passed", false, nil
}

// resultBuyer is implemented by trading.JupiterBuyer; declared locally so
// the broadcaster can retrieve a transaction signature without importing
// concrete trading types beyond the AutoBuyer interface.
type resultBuyer interface {
	BuyTokenWithResult(ctx context.Context, tokenAddress string, amountUSD float64) (string, error)
}

// formatAlertMessage builds the alert body sent to the admin. All dynamic
// content is HTML-escaped before embedding to prevent Telegram parse-mode
// panics on adversarial token names/symbols. Addresses are shown in full.
func formatAlertMessage(alert detector.ClusterAlert, safetyLine string, isWhaleMatch bool, autoBuyLine string) string {
	windowMin := alert.TimeWindowSeconds / 60
	if windowMin < 1 {
		windowMin = 1
	}
	avgEntry := alert.TotalVolumeUSD
	if alert.BuyCount > 0 {
		avgEntry = alert.TotalVolumeUSD / float64(alert.BuyCount)
	}

	entryLine := "Entry: Safe (initial accumulation)"
	if alert.TimeWindowSeconds > 120 {
		entryLine = "Entry: Late — higher risk"
	}

	whaleLine := ""
	if isWhaleMatch {
		whaleLine = "\nSeeded whale match!"
	}

	body := fmt.Sprintf(
		"Cluster Alert%s\n\n"+
			"%s (%s)\n\n"+
			"%s\n"+
			"%s\n\n"+
			"Volume: $%s\n"+
			"Wallets: %d\n"+
			"Avg entry: $%s\n"+
			"Time window: %d min\n"+
			"Lead wallet: %s\n\n"+
			"Contract: %s",
		whaleLine,
		html.EscapeString(alert.TokenSymbol),
		html.EscapeString(alert.Chain),
		entryLine,
		safetyLine,
		fmtFloat(alert.TotalVolumeUSD),
		alert.BuyCount,
		fmtFloat(avgEntry),
		windowMin,
		html.EscapeString(alert.LeadWallet),
		alert.TokenAddress,
	)

	if autoBuyLine != "" {
		body += "\n\n" + autoBuyLine
	}
	return body
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

// StartAlertBroadcaster runs as a goroutine that consumes alerts from
// alertsChan and delivers them to the single admin chat, triggering an
// auto-buy for qualifying Solana clusters along the way. Wrapped in panic
// recovery — a bad alert or downstream failure must never kill the process.
func StartAlertBroadcaster(
	ctx context.Context,
	client *Client,
	store *storage.Storage,
	cfg *config.Config,
	buyer trading.AutoBuyer,
	seller *trading.Seller,
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
				safeBroadcastAlert(ctx, client, store, cfg, buyer, seller, alert)
			}
		}
	}()
}

func safeBroadcastAlert(ctx context.Context, client *Client, store *storage.Storage, cfg *config.Config, buyer trading.AutoBuyer, seller *trading.Seller, alert detector.ClusterAlert) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[PANIC RECOVER] broadcastAlert recovered: %v", r)
		}
	}()
	broadcastAlert(ctx, client, store, cfg, buyer, seller, alert)
}

func broadcastAlert(ctx context.Context, client *Client, store *storage.Storage, cfg *config.Config, buyer trading.AutoBuyer, seller *trading.Seller, alert detector.ClusterAlert) {
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
	rugCheckPassed := !shouldBlock

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

	// Auto-buy: only for Solana clusters that passed RugCheck, and only if
	// the operator has actually enabled it with a configured wallet.
	autoBuyLine := ""
	if rugCheckPassed && cfg != nil && cfg.AutoBuyEnabled && strings.EqualFold(alert.Chain, "solana") {
		autoBuyLine = attemptAutoBuy(ctx, buyer, seller, cfg, alert)
	}

	if cfg == nil || cfg.AdminChatID == 0 {
		log.Printf("[BROADCASTER] no AdminChatID configured — dropping alert for %s", alert.TokenSymbol)
		return
	}

	msg := formatAlertMessage(alert, safetyLine, isWhaleMatch, autoBuyLine)
	kb := alertKeyboard(alert)
	if err := client.SendMessageWithKeyboard(cfg.AdminChatID, msg, kb); err != nil {
		log.Printf("[BROADCASTER] send to admin %d: %v", cfg.AdminChatID, err)
	}
}

// attemptAutoBuy executes or simulates a buy depending on cfg.SimulationMode.
// Returns a human-readable result line to append to the Telegram alert.
func attemptAutoBuy(ctx context.Context, buyer trading.AutoBuyer, seller *trading.Seller, cfg *config.Config, alert detector.ClusterAlert) string {
	if buyer == nil {
		return ""
	}

	// Simulation mode: record a paper trade, no real transaction.
	if cfg.SimulationMode {
		if seller != nil {
			seller.RecordSimulatedBuy(ctx, alert.TokenAddress, alert.TokenSymbol, alert.Chain, cfg.AutoBuyAmountUSD)
		}
		log.Printf("[AUTOBUY] [SIMULATION] paper trade recorded for %s", alert.TokenAddress)
		return fmt.Sprintf("[SIMULATION] Paper trade recorded — $%.2f position opened (no real tx)", cfg.AutoBuyAmountUSD)
	}

	buyCtx, cancel := context.WithTimeout(ctx, autoBuyTimeout)
	defer cancel()

	if rb, ok := buyer.(resultBuyer); ok {
		sig, err := rb.BuyTokenWithResult(buyCtx, alert.TokenAddress, cfg.AutoBuyAmountUSD)
		if err != nil {
			log.Printf("[AUTOBUY] failed for %s: %v", alert.TokenAddress, err)
			return fmt.Sprintf("Auto-Buy: FAILED — %v", err)
		}
		log.Printf("[AUTOBUY] success for %s: tx %s", alert.TokenAddress, sig)
		if seller != nil {
			seller.RecordBuy(ctx, alert.TokenAddress, alert.TokenSymbol, alert.Chain, sig, cfg.AutoBuyAmountUSD)
		}
		return fmt.Sprintf("Auto-Buy: SUCCESS\nTx: %s", sig)
	}

	if err := buyer.BuyToken(buyCtx, alert.TokenAddress, cfg.AutoBuyAmountUSD); err != nil {
		log.Printf("[AUTOBUY] failed for %s: %v", alert.TokenAddress, err)
		return fmt.Sprintf("Auto-Buy: FAILED — %v", err)
	}
	return "Auto-Buy: SUCCESS"
}

// SendWhaleActivityAlert fires a Telegram notification when a tracked whale
// executes a buy detected via the Helius webhook. Called from helius.go after
// a swap event arrives from a known smart wallet.
func SendWhaleActivityAlert(client *Client, adminChatID int64, whaleAddr, tokenSymbol, tokenAddress, chain string, amountUSD float64) {
	dex := "https://dexscreener.com/solana/" + tokenAddress
	gmgn := "https://gmgn.ai/sol/token/" + tokenAddress

	msg := fmt.Sprintf(
		"🐋 Whale Activity Alert\n\n"+
			"Wallet: <code>%s</code>\n"+
			"Bought: <b>%s</b>\n"+
			"Chain: %s\n"+
			"Amount: ~$%.0f\n\n"+
			"Contract:\n<code>%s</code>",
		html.EscapeString(whaleAddr),
		html.EscapeString(or(tokenSymbol, "Unknown")),
		html.EscapeString(chain),
		amountUSD,
		tokenAddress,
	)

	kb := &InlineKeyboardMarkup{
		InlineKeyboard: [][]InlineKeyboardButton{
			{
				{Text: "📊 DexScreener", URL: dex},
				{Text: "📈 GMGN", URL: gmgn},
			},
			{
				{Text: "🐋 Add Whale", CallbackData: "cb:whale:add:" + whaleAddr},
			},
		},
	}

	if err := client.SendMessageWithKeyboard(adminChatID, msg, kb); err != nil {
		log.Printf("[WHALE ALERT] send failed for %s: %v", whaleAddr, err)
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
	sb.WriteString("Daily Digest — Solo Sniper Station\n")
	fmt.Fprintf(&sb, "%s UTC\n\n", time.Now().UTC().Format("02 Jan 2006"))

	sb.WriteString("24h Summary:\n")
	if stats != nil {
		fmt.Fprintf(&sb,
			"Clusters: %d\nVolume: $%s\nTop Token: %s\nTop Chain: %s\n\n",
			stats.TotalClusters,
			html.EscapeString(fmtFloat(stats.TotalVolumeUSD)),
			html.EscapeString(or(stats.TopToken, "—")),
			html.EscapeString(or(stats.TopChain, "—")),
		)
	} else {
		sb.WriteString("No data\n\n")
	}

	if len(clusters) > 0 {
		sb.WriteString("Top Clusters:\n")
		for i, c := range clusters {
			fmt.Fprintf(&sb,
				"%d. %s (%s) — $%s, %d wallets\n%s\n",
				i+1,
				html.EscapeString(c.TokenSymbol),
				html.EscapeString(c.Chain),
				html.EscapeString(fmtFloat(c.TotalVolumeUSD)),
				c.BuyCount,
				c.TokenAddress,
			)
		}
		sb.WriteString("\n")
	}

	if len(hotWallets) > 0 {
		sb.WriteString("Hot Wallets:\n")
		for _, w := range hotWallets {
			fmt.Fprintf(&sb,
				"%s — %d clusters, $%s\n",
				html.EscapeString(w.WalletAddress),
				w.ClusterCount,
				html.EscapeString(fmtFloat(w.TotalVolumeUSD)),
			)
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
