package telegram

import (
	"encoding/json"
	"fmt"
	"html"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"smart-cluster-bot/internal/config"
	"smart-cluster-bot/internal/detector"
	"smart-cluster-bot/internal/i18n"
	"smart-cluster-bot/internal/storage"
)

// WebhookHandler receives Telegram webhook updates, returns 200 OK immediately,
// then processes the update asynchronously in a goroutine. This is a solo,
// single-admin bot — every update is checked against config.AdminChatID and
// anything else is silently dropped.
type WebhookHandler struct {
	client      *Client
	storage     *storage.Storage
	config      *config.Config
	i18n        *i18n.Bundle
	engine      *detector.ClusterEngine
	whaleFinder func()

	// awaitingWhaleInput is set to true when the admin taps "➕ Add Whale"
	// so the next plain-text message is treated as a wallet address input.
	awaitMu            sync.Mutex
	awaitingWhaleInput bool
}

// NewWebhookHandler wires all dependencies, including the live cluster engine
// so Sniper Settings changes take effect immediately.
func NewWebhookHandler(client *Client, store *storage.Storage, cfg *config.Config, bundle *i18n.Bundle, engine *detector.ClusterEngine, whaleFinder func()) *WebhookHandler {
	return &WebhookHandler{client: client, storage: store, config: cfg, i18n: bundle, engine: engine, whaleFinder: whaleFinder}
}

func (h *WebhookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	var update Update
	if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
		log.Printf("[WEBHOOK] decode error: %v", err)
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusOK)
	go h.dispatch(&update)
}

// dispatch routes an update, wrapped in panic recovery so a single malformed
// update or downstream bug never crashes the whole bot process.
func (h *WebhookHandler) dispatch(u *Update) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[PANIC RECOVER] dispatch recovered: %v", r)
		}
	}()

	switch {
	case u.CallbackQuery != nil:
		if !h.isAdmin(u.CallbackQuery.From.ID) {
			log.Printf("[SECURITY] ignored callback from non-admin user %d", u.CallbackQuery.From.ID)
			return
		}
		h.handleCallback(u.CallbackQuery)
	case u.Message != nil && u.Message.From != nil:
		if !h.isAdmin(u.Message.From.ID) {
			log.Printf("[SECURITY] ignored message from non-admin user %d", u.Message.From.ID)
			return
		}
		h.handleMessage(u.Message)
	}
}

// isAdmin reports whether userID is the sole authorized operator.
func (h *WebhookHandler) isAdmin(userID int64) bool {
	return h.config != nil && userID == h.config.AdminChatID
}

// ── Message handling ───────────────────────────────────────────────────────────

func (h *WebhookHandler) handleMessage(msg *Message) {
	chatID := msg.Chat.ID
	text := strings.TrimSpace(msg.Text)

	// State machine: if the admin tapped "➕ Add Whale", the next
	// non-command message is treated as a wallet address.
	h.awaitMu.Lock()
	waiting := h.awaitingWhaleInput
	if waiting {
		h.awaitingWhaleInput = false
	}
	h.awaitMu.Unlock()

	if waiting && !strings.HasPrefix(text, "/") {
		h.handleInlineWhaleAdd(chatID, text)
		return
	}

	switch {
	case text == "/start":
		h.sendStartMenu(chatID)
	case strings.HasPrefix(text, "/addwhale"):
		h.handleAddWhaleCommand(chatID, text)
	case text == "/whales":
		h.sendWhalesMenu(chatID)
	case text == "/clusters":
		h.sendClustersMenu(chatID)
	case text == "/settings":
		h.sendSettingsMenu(chatID)
	default:
		h.sendStartMenu(chatID)
	}
}

// ── Start menu ───────────────────────────────────────────────────────────────

func (h *WebhookHandler) sendStartMenu(chatID int64) {
	body := h.startMenuText()
	if err := h.client.SendMessageWithKeyboard(chatID, body, h.mainMenuKB()); err != nil {
		log.Printf("[HANDLER] sendStartMenu %d: %v", chatID, err)
	}
}

func (h *WebhookHandler) editStartMenu(chatID int64, msgID int) {
	body := h.startMenuText()
	if err := h.client.EditMessageText(chatID, msgID, body, h.mainMenuKB()); err != nil {
		log.Printf("[HANDLER] editStartMenu fallback %d/%d: %v", chatID, msgID, err)
		_ = h.client.DeleteMessage(chatID, msgID)
		_ = h.client.SendMessageWithKeyboard(chatID, body, h.mainMenuKB())
	}
}

func (h *WebhookHandler) startMenuText() string {
	autoBuyStatus := "Off"
	if h.config != nil && h.config.AutoBuyEnabled {
		autoBuyStatus = fmt.Sprintf("On ($%.2f per trade)", h.config.AutoBuyAmountUSD)
	}

	modeLine := "Mode: 🚀 REAL TRADING"
	if h.config != nil && h.config.SimulationMode {
		modeLine = "Mode: 🧪 SIMULATION (no real txs)"
	}

	return "Solo Sniper Station\n\n" +
		"Monitoring is active. Choose a module below.\n\n" +
		"Auto-Buy: " + autoBuyStatus + "\n" +
		modeLine
}

func (h *WebhookHandler) mainMenuKB() *InlineKeyboardMarkup {
	rows := [][]InlineKeyboardButton{
		{{Text: "📡 Active Clusters", CallbackData: "cb:clusters"}},
		{{Text: "🐋 Manage Whales", CallbackData: "cb:whales"}},
		{{Text: "💼 Open Positions", CallbackData: "cb:positions"}},
		{{Text: "🔍 Find Shadow Whales", CallbackData: "cb:findwhales"}},
		{{Text: "⚙️ Sniper Settings", CallbackData: "cb:settings"}},
	}
	if u := h.webAppURL(); u != "" {
		rows = append([][]InlineKeyboardButton{
			{{Text: "📊 Open Terminal", WebApp: &WebAppInfo{URL: u}}},
		}, rows...)
	}
	return &InlineKeyboardMarkup{InlineKeyboard: rows}
}

// ── Active Clusters ────────────────────────────────────────────────────────────

func (h *WebhookHandler) sendClustersMenu(chatID int64) {
	body, kb := h.buildClustersContent()
	if err := h.client.SendMessageWithKeyboard(chatID, body, kb); err != nil {
		log.Printf("[HANDLER] sendClustersMenu %d: %v", chatID, err)
	}
}

func (h *WebhookHandler) editClustersMenu(chatID int64, msgID int) {
	body, kb := h.buildClustersContent()
	if err := h.client.EditMessageText(chatID, msgID, body, kb); err != nil {
		log.Printf("[HANDLER] editClustersMenu %d/%d: %v", chatID, msgID, err)
	}
}

func (h *WebhookHandler) buildClustersContent() (string, *InlineKeyboardMarkup) {
	minWallets, minVolume, window := h.engineThresholds()

	stats, statsErr := h.storage.GetStats24h()
	clusters, clustersErr := h.storage.GetRecentClusters(5)

	var sb strings.Builder
	sb.WriteString("Active Clusters\n\n")
	fmt.Fprintf(&sb,
		"Detection thresholds: %d+ wallets, $%.0f+ volume, %ds window.\n\n",
		minWallets, minVolume, int(window.Seconds()),
	)

	if statsErr == nil && stats != nil {
		fmt.Fprintf(&sb, "Last 24h: %d clusters, $%s total volume.\n\n",
			stats.TotalClusters, fmtFloat(stats.TotalVolumeUSD))
	}

	if clustersErr != nil || len(clusters) == 0 {
		sb.WriteString("No clusters detected yet.")
	} else {
		sb.WriteString("Recent clusters:\n\n")
		for _, c := range clusters {
			fmt.Fprintf(&sb,
				"%s (%s)\nVolume: $%s — Buys: %d\nContract: %s\n\n",
				html.EscapeString(c.TokenSymbol),
				html.EscapeString(c.Chain),
				fmtFloat(c.TotalVolumeUSD),
				c.BuyCount,
				c.TokenAddress,
			)
		}
	}

	kb := &InlineKeyboardMarkup{
		InlineKeyboard: [][]InlineKeyboardButton{
			{{Text: "🔄 Refresh", CallbackData: "cb:clusters"}},
			{{Text: "⬅️ Main Menu", CallbackData: "cb:menu"}},
		},
	}
	return sb.String(), kb
}

// ── Manage Whales ──────────────────────────────────────────────────────────────

func (h *WebhookHandler) sendWhalesMenu(chatID int64) {
	body, kb := h.buildWhalesContent()
	if err := h.client.SendMessageWithKeyboard(chatID, body, kb); err != nil {
		log.Printf("[HANDLER] sendWhalesMenu %d: %v", chatID, err)
	}
}

func (h *WebhookHandler) editWhalesMenu(chatID int64, msgID int) {
	body, kb := h.buildWhalesContent()
	if err := h.client.EditMessageText(chatID, msgID, body, kb); err != nil {
		log.Printf("[HANDLER] editWhalesMenu %d/%d: %v", chatID, msgID, err)
	}
}

func (h *WebhookHandler) buildWhalesContent() (string, *InlineKeyboardMarkup) {
	wallets, err := h.storage.GetSmartWallets()
	header := "Manage Whales\n\n"

	addBtn := InlineKeyboardButton{Text: "➕ Add Whale", CallbackData: "cb:whale:prompt"}
	backBtn := InlineKeyboardButton{Text: "⬅️ Main Menu", CallbackData: "cb:menu"}

	if err != nil || len(wallets) == 0 {
		body := header + "No whales tracked yet.\n\nTap ➕ Add Whale or use /addwhale &lt;address&gt; [note]"
		return body, &InlineKeyboardMarkup{
			InlineKeyboard: [][]InlineKeyboardButton{
				{addBtn},
				{backBtn},
			},
		}
	}

	var sb strings.Builder
	sb.WriteString(header)
	fmt.Fprintf(&sb, "Tracking %d wallet(s):\n\n", len(wallets))

	var rows [][]InlineKeyboardButton
	for _, w := range wallets {
		fmt.Fprintf(&sb, "<code>%s</code>", html.EscapeString(w.WalletAddress))
		if w.Note != "" {
			fmt.Fprintf(&sb, " — %s", html.EscapeString(w.Note))
		}
		sb.WriteString("\n")
		rows = append(rows, []InlineKeyboardButton{
			{Text: "🗑 " + shortLabel(w.WalletAddress), CallbackData: fmt.Sprintf("cb:whale:rm:%d", w.ID)},
		})
	}
	rows = append(rows, []InlineKeyboardButton{addBtn})
	rows = append(rows, []InlineKeyboardButton{backBtn})
	return sb.String(), &InlineKeyboardMarkup{InlineKeyboard: rows}
}

func (h *WebhookHandler) handleAddWhaleCommand(chatID int64, text string) {
	parts := strings.Fields(text)
	if len(parts) < 2 {
		h.client.SendMessage(chatID, "Usage: /addwhale &lt;address&gt; [note]")
		return
	}
	h.saveWhaleAddress(chatID, parts[1], strings.Join(parts[2:], " "), 0)
}

// handleInlineWhaleAdd processes the address the admin typed after tapping
// "➕ Add Whale". Validates it and saves to the DB.
func (h *WebhookHandler) handleInlineWhaleAdd(chatID int64, input string) {
	parts := strings.Fields(input)
	addr := parts[0]
	note := strings.Join(parts[1:], " ")
	h.saveWhaleAddress(chatID, addr, note, 0)
}

// saveWhaleAddress validates a Solana address, saves it to the DB, and
// sends a confirmation. msgID=0 means send a new message; non-zero edits.
func (h *WebhookHandler) saveWhaleAddress(chatID int64, addr, note string, msgID int) {
	if !isValidSolanaAddress(addr) {
		msg := fmt.Sprintf(
			"❌ Invalid Solana address:\n<code>%s</code>\n\n"+
				"A valid Solana address is 32–44 base58 characters.\n"+
				"Please try again or tap ⬅️ to cancel.",
			html.EscapeString(addr),
		)
		kb := &InlineKeyboardMarkup{
			InlineKeyboard: [][]InlineKeyboardButton{
				{{Text: "⬅️ Back to Whales", CallbackData: "cb:whales"}},
			},
		}
		if msgID != 0 {
			h.client.EditMessageText(chatID, msgID, msg, kb)
		} else {
			h.client.SendMessageWithKeyboard(chatID, msg, kb)
		}
		return
	}
	if note == "" {
		note = "Manually added"
	}
	if err := h.storage.AddSmartWallet(addr, note); err != nil {
		log.Printf("[HANDLER] AddSmartWallet: %v", err)
		h.client.SendMessage(chatID, "❌ Failed to save whale. Try again.")
		return
	}
	reply := fmt.Sprintf(
		"✅ Whale added!\n\n<code>%s</code>\nNote: %s",
		html.EscapeString(addr), html.EscapeString(note),
	)
	h.client.SendMessageWithKeyboard(chatID, reply, &InlineKeyboardMarkup{
		InlineKeyboard: [][]InlineKeyboardButton{
			{{Text: "🐋 View Whales", CallbackData: "cb:whales"}},
		},
	})
}

// isValidSolanaAddress checks that addr is a plausible base58 Solana public
// key — 32 to 44 characters, containing only base58 alphabet characters.
// This is a format check only, not a cryptographic verification.
func isValidSolanaAddress(addr string) bool {
	const base58Alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"
	if len(addr) < 32 || len(addr) > 44 {
		return false
	}
	for _, c := range addr {
		if !strings.ContainsRune(base58Alphabet, c) {
			return false
		}
	}
	return true
}

// handleWhalePrompt is triggered by the "➕ Add Whale" inline button.
// It edits the current message to show instructions and sets the awaiting
// state so the next plain-text message from the admin is treated as an address.
func (h *WebhookHandler) handleWhalePrompt(chatID int64, msgID int) {
	h.awaitMu.Lock()
	h.awaitingWhaleInput = true
	h.awaitMu.Unlock()

	prompt := "🐋 <b>Add Whale</b>\n\n" +
		"Send me the Solana wallet address you want to track.\n" +
		"You can optionally add a note after the address:\n\n" +
		"<code>7xKXtg2CW87d97TXJSDpbD5jBkheTqA83TZRuJosgAsU my whale note</code>\n\n" +
		"Type the address now, or tap Cancel."

	kb := &InlineKeyboardMarkup{
		InlineKeyboard: [][]InlineKeyboardButton{
			{{Text: "❌ Cancel", CallbackData: "cb:whale:cancel"}},
		},
	}
	if err := h.client.EditMessageText(chatID, msgID, prompt, kb); err != nil {
		log.Printf("[HANDLER] handleWhalePrompt edit %d/%d: %v", chatID, msgID, err)
	}
}

func (h *WebhookHandler) handleRemoveWhale(chatID int64, msgID int, data string) {
	parts := strings.Split(data, ":")
	if len(parts) < 3 {
		return
	}
	id, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return
	}
	if err := h.storage.RemoveSmartWallet(id); err != nil {
		log.Printf("[HANDLER] RemoveSmartWallet %d: %v", id, err)
	}
	h.editWhalesMenu(chatID, msgID)
}

// ── Sniper Settings ────────────────────────────────────────────────────────────

func (h *WebhookHandler) sendSettingsMenu(chatID int64) {
	body, kb := h.buildSettingsContent()
	if err := h.client.SendMessageWithKeyboard(chatID, body, kb); err != nil {
		log.Printf("[HANDLER] sendSettingsMenu %d: %v", chatID, err)
	}
}

func (h *WebhookHandler) editSettingsMenu(chatID int64, msgID int) {
	body, kb := h.buildSettingsContent()
	if err := h.client.EditMessageText(chatID, msgID, body, kb); err != nil {
		log.Printf("[HANDLER] editSettingsMenu %d/%d: %v", chatID, msgID, err)
	}
}

func (h *WebhookHandler) buildSettingsContent() (string, *InlineKeyboardMarkup) {
	st, err := h.storage.GetSniperSettings()
	if err != nil || st == nil {
		return "Error loading settings.", &InlineKeyboardMarkup{
			InlineKeyboard: [][]InlineKeyboardButton{{{Text: "⬅️ Main Menu", CallbackData: "cb:menu"}}},
		}
	}

	body := fmt.Sprintf(
		"Sniper Settings (Anti-FOMO)\n\n"+
			"Minimum wallets: %d\n"+
			"Minimum volume: $%.0f\n"+
			"Time window: %ds\n"+
			"Chains: %s\n\n"+
			"Changes apply immediately — no restart needed.",
		st.MinWallets, st.MinVolumeUSD, st.WindowSeconds, enabledChains(st),
	)

	kb := &InlineKeyboardMarkup{
		InlineKeyboard: [][]InlineKeyboardButton{
			{
				{Text: "Wallets: 2", CallbackData: "cb:set:wallets:2"},
				{Text: "3", CallbackData: "cb:set:wallets:3"},
				{Text: "4", CallbackData: "cb:set:wallets:4"},
				{Text: "5", CallbackData: "cb:set:wallets:5"},
			},
			{
				{Text: "$500", CallbackData: "cb:set:vol:500"},
				{Text: "$1.5k", CallbackData: "cb:set:vol:1500"},
				{Text: "$3k", CallbackData: "cb:set:vol:3000"},
				{Text: "$5k", CallbackData: "cb:set:vol:5000"},
			},
			{
				{Text: "60s", CallbackData: "cb:set:window:60"},
				{Text: "120s", CallbackData: "cb:set:window:120"},
				{Text: "180s", CallbackData: "cb:set:window:180"},
			},
			{
				{Text: emojiCheck(st.EthEnabled) + " ETH", CallbackData: "cb:set:net:eth"},
				{Text: emojiCheck(st.SolEnabled) + " SOL", CallbackData: "cb:set:net:sol"},
				{Text: emojiCheck(st.BaseEnabled) + " BASE", CallbackData: "cb:set:net:base"},
				{Text: emojiCheck(st.BscEnabled) + " BSC", CallbackData: "cb:set:net:bsc"},
			},
			{{Text: "⬅️ Main Menu", CallbackData: "cb:menu"}},
		},
	}
	return body, kb
}

func (h *WebhookHandler) handleSettingsUpdate(chatID int64, msgID int, data string) {
	st, err := h.storage.GetSniperSettings()
	if err != nil || st == nil {
		log.Printf("[HANDLER] handleSettingsUpdate load: %v", err)
		return
	}

	parts := strings.Split(data, ":")
	if len(parts) < 4 {
		return
	}
	field, value := parts[2], parts[3]

	switch field {
	case "wallets":
		if n, err := strconv.Atoi(value); err == nil {
			st.MinWallets = n
		}
	case "vol":
		if n, err := strconv.ParseFloat(value, 64); err == nil {
			st.MinVolumeUSD = n
		}
	case "window":
		if n, err := strconv.Atoi(value); err == nil {
			st.WindowSeconds = n
		}
	case "net":
		switch value {
		case "eth":
			st.EthEnabled = !st.EthEnabled
		case "sol":
			st.SolEnabled = !st.SolEnabled
		case "base":
			st.BaseEnabled = !st.BaseEnabled
		case "bsc":
			st.BscEnabled = !st.BscEnabled
		}
	default:
		return
	}

	if err := h.storage.UpdateSniperSettings(*st); err != nil {
		log.Printf("[HANDLER] UpdateSniperSettings: %v", err)
		return
	}

	if h.engine != nil {
		h.engine.UpdateThresholds(st.MinWallets, st.MinVolumeUSD, secondsToDuration(st.WindowSeconds))
	}

	h.editSettingsMenu(chatID, msgID)
}

// ── Callback routing ───────────────────────────────────────────────────────────

func (h *WebhookHandler) handleCallback(cb *CallbackQuery) {
	chatID := int64(0)
	msgID := 0
	if cb.Message != nil {
		chatID = cb.Message.Chat.ID
		msgID = cb.Message.MessageID
	}
	data := cb.Data

	if err := h.client.AnswerCallbackQuery(cb.ID, ""); err != nil {
		log.Printf("[HANDLER] AnswerCallbackQuery %s: %v", cb.ID, err)
	}

	switch {
	case data == "cb:menu":
		h.editStartMenu(chatID, msgID)
	case data == "cb:clusters":
		h.editClustersMenu(chatID, msgID)
	case data == "cb:whales":
		h.editWhalesMenu(chatID, msgID)
	case data == "cb:positions":
		h.editPositionsMenu(chatID, msgID)
	case data == "cb:findwhales":
		h.handleFindWhales(chatID, msgID)
	case data == "cb:settings":
		h.editSettingsMenu(chatID, msgID)
	case strings.HasPrefix(data, "cb:whale:rm:"):
		h.handleRemoveWhale(chatID, msgID, data)
	case data == "cb:whale:prompt":
		h.handleWhalePrompt(chatID, msgID)
	case data == "cb:whale:cancel":
		h.awaitMu.Lock()
		h.awaitingWhaleInput = false
		h.awaitMu.Unlock()
		h.editWhalesMenu(chatID, msgID)
	case strings.HasPrefix(data, "cb:whale:add:"):
		addr := strings.TrimPrefix(data, "cb:whale:add:")
		if err := h.storage.AddSmartWallet(addr, "Added from alert"); err != nil {
			log.Printf("[HANDLER] AddSmartWallet from alert: %v", err)
		}
		h.editWhalesMenu(chatID, msgID)
	case strings.HasPrefix(data, "cb:set:"):
		h.handleSettingsUpdate(chatID, msgID, data)
	}
}

// ── Engine helpers ─────────────────────────────────────────────────────────────

func (h *WebhookHandler) engineThresholds() (int, float64, time.Duration) {
	if h.engine == nil {
		return 3, 1500, 120 * time.Second
	}
	return h.engine.Thresholds()
}

func secondsToDuration(seconds int) time.Duration {
	return time.Duration(seconds) * time.Second
}

// ── Open Positions ─────────────────────────────────────────────────────────────

func (h *WebhookHandler) editPositionsMenu(chatID int64, msgID int) {
	body, kb := h.buildPositionsContent()
	if err := h.client.EditMessageText(chatID, msgID, body, kb); err != nil {
		log.Printf("[HANDLER] editPositionsMenu %d/%d: %v", chatID, msgID, err)
	}
}

func (h *WebhookHandler) buildPositionsContent() (string, *InlineKeyboardMarkup) {
	positions, err := h.storage.GetOpenPositions()
	back := &InlineKeyboardMarkup{
		InlineKeyboard: [][]InlineKeyboardButton{{{Text: "⬅️ Main Menu", CallbackData: "cb:menu"}}},
	}

	if err != nil {
		return "Error loading positions.", back
	}
	if len(positions) == 0 {
		return "Open Positions\n\nNo open positions. Auto-buy will create entries here.", back
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "Open Positions (%d)\n\n", len(positions))
	for _, p := range positions {
		status := "🟡 Open"
		if p.Status == "tp_partial" {
			status = "🟢 TP Hit — Riding"
		}
		fmt.Fprintf(&sb,
			"%s %s\n"+
				"Entry: $%.6f\n"+
				"Size: $%.2f\n"+
				"TP: +%.0f%% | SL: -%.0f%%\n"+
				"Contract: %s\n\n",
			status, p.TokenSymbol,
			p.EntryPriceUSD,
			p.BuyAmountUSD,
			p.TakeProfitPct, p.StopLossPct,
			p.TokenAddress,
		)
	}
	return sb.String(), back
}

// ── Find Shadow Whales ────────────────────────────────────────────────────────

func (h *WebhookHandler) handleFindWhales(chatID int64, msgID int) {
	// Acknowledge immediately with a "scanning" message
	scanning := "Scanning DexScreener + GMGN for shadow whale candidates...\n\nThis takes 30–60 seconds. Results will arrive as a separate message."
	if err := h.client.EditMessageText(chatID, msgID, scanning, &InlineKeyboardMarkup{
		InlineKeyboard: [][]InlineKeyboardButton{{{Text: "⬅️ Main Menu", CallbackData: "cb:menu"}}},
	}); err != nil {
		log.Printf("[HANDLER] handleFindWhales edit: %v", err)
	}

	if h.whaleFinder != nil {
		go h.whaleFinder()
	}
}

// ── URL helpers ────────────────────────────────────────────────────────────────

func (h *WebhookHandler) webAppURL() string {
	if h.config == nil {
		return ""
	}
	if h.config.WebAppURL != "" {
		return h.config.WebAppURL
	}
	if h.config.RenderURL != "" {
		return h.config.RenderURL + "/app"
	}
	return ""
}

// ── Shared formatting helpers ──────────────────────────────────────────────────

func enabledChains(st *storage.SniperSettings) string {
	var nets []string
	if st.EthEnabled {
		nets = append(nets, "ETH")
	}
	if st.SolEnabled {
		nets = append(nets, "SOL")
	}
	if st.BaseEnabled {
		nets = append(nets, "BASE")
	}
	if st.BscEnabled {
		nets = append(nets, "BSC")
	}
	if len(nets) == 0 {
		return "none"
	}
	return strings.Join(nets, ", ")
}

func emojiCheck(v bool) string {
	if v {
		return "✅"
	}
	return "⬜"
}

// shortLabel produces a short button-only label (Telegram buttons have tight
// width limits); this is purely cosmetic for the button text and never used
// for the full address shown in message bodies.
func shortLabel(addr string) string {
	if len(addr) <= 10 {
		return addr
	}
	return addr[:4] + "…" + addr[len(addr)-4:]
}

func fmtFloat(v float64) string {
	switch {
	case v >= 1_000_000:
		return fmt.Sprintf("%.2fM", v/1_000_000)
	case v >= 1_000:
		return fmt.Sprintf("%.1fK", v/1_000)
	default:
		return fmt.Sprintf("%.2f", v)
	}
}

func or(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
