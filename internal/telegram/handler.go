package telegram

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"smart-cluster-bot/internal/config"
	"smart-cluster-bot/internal/i18n"
	"smart-cluster-bot/internal/storage"
)

// WebhookHandler receives Telegram webhook updates, returns 200 OK immediately,
// then processes the update asynchronously in a goroutine.
type WebhookHandler struct {
	client  *Client
	storage *storage.Storage
	config  *config.Config
	i18n    *i18n.Bundle
}

func NewWebhookHandler(client *Client, store *storage.Storage, cfg *config.Config, bundle *i18n.Bundle) *WebhookHandler {
	return &WebhookHandler{client: client, storage: store, config: cfg, i18n: bundle}
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

func (h *WebhookHandler) dispatch(u *Update) {
	switch {
	case u.CallbackQuery != nil:
		h.handleCallback(u.CallbackQuery)
	case u.Message != nil && u.Message.From != nil && u.Message.SuccessfulPayment != nil:
		h.handleSuccessfulPayment(u.Message)
	case u.Message != nil && u.Message.From != nil:
		h.handleMessage(u.Message)
	}
}

// ── Successful Payment (Telegram Stars) ───────────────────────────────────────
// Telegram sends a Message with SuccessfulPayment populated (not a separate
// update type) after the user completes a Stars invoice.

func (h *WebhookHandler) handleSuccessfulPayment(msg *Message) {
	userID := msg.From.ID
	chatID := msg.Chat.ID
	sp := msg.SuccessfulPayment

	// Guard: only honour Stars (XTR) payments for our VIP product.
	if sp == nil || sp.Currency != "XTR" || sp.InvoicePayload != "vip_stars_30d" {
		log.Printf("[PAYMENT] unexpected payment ignored: currency=%s payload=%s", sp.Currency, sp.InvoicePayload)
		return
	}

	if err := h.storage.SetUserVIP(userID, true); err != nil {
		log.Printf("[PAYMENT] SetUserVIP %d: %v", userID, err)
		h.client.SendMessage(chatID, "⚠️ Payment received but VIP activation failed. Contact @StarkWonder.")
		return
	}

	bootstrapLang := "en"
	if strings.HasPrefix(strings.ToLower(msg.From.LanguageCode), "ru") {
		bootstrapLang = "ru"
	}
	user, _ := h.storage.GetOrCreateUser(userID, msg.From.Username, bootstrapLang)
	lang := bootstrapLang
	if user != nil {
		lang = user.Language
	}

	log.Printf("[PAYMENT] Stars VIP activated for user %d", userID)
	h.sendVIPActivatedMessage(chatID, lang)
}

// sendVIPActivatedMessage is shared by both Stars and CryptoBot flows.
func (h *WebhookHandler) sendVIPActivatedMessage(chatID int64, lang string) {
	var body string
	if lang == "ru" {
		body = "👑 <b>VIP АКТИВИРОВАН</b>\n" +
			"<b>━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━</b>\n\n" +
			"<code>> ACCESS_LEVEL: VIP\n" +
			"> MASK: DISABLED\n" +
			"> ALERTS: UNLIMITED\n" +
			"> STATUS: ACTIVE [30d]</code>\n\n" +
			"Добро пожаловать в элиту 🔥\n" +
			"Все адреса кошельков теперь полностью открыты."
	} else {
		body = "👑 <b>VIP ACTIVATED</b>\n" +
			"<b>━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━</b>\n\n" +
			"<code>> ACCESS_LEVEL: VIP\n" +
			"> MASK: DISABLED\n" +
			"> ALERTS: UNLIMITED\n" +
			"> STATUS: ACTIVE [30d]</code>\n\n" +
			"Welcome to the elite 🔥\n" +
			"All wallet addresses are now fully unmasked."
	}
	if err := h.client.SendMessageWithKeyboard(chatID, body, backToMenuKB(lang)); err != nil {
		log.Printf("[PAYMENT] sendVIPActivatedMessage %d: %v", chatID, err)
	}
}

// ── Message handling ───────────────────────────────────────────────────────────

func (h *WebhookHandler) handleMessage(msg *Message) {
	chatID := msg.Chat.ID
	from := msg.From
	bootstrapLang := "en"
	if strings.HasPrefix(strings.ToLower(from.LanguageCode), "ru") {
		bootstrapLang = "ru"
	}
	user, err := h.storage.GetOrCreateUser(from.ID, from.Username, bootstrapLang)
	if err != nil {
		log.Printf("[HANDLER] GetOrCreateUser %d: %v", from.ID, err)
		return
	}

	text := strings.TrimSpace(msg.Text)

	if !user.TosAccepted {
		h.sendTosDisclaimer(chatID, user.Language)
		return
	}

	switch {
	case text == "/start":
		h.sendStartMenu(chatID, from.FirstName, user)
	case text == "/manual" || text == "/help":
		h.sendManual(chatID, user.Language)
	case strings.HasPrefix(text, "/watch ") || text == "/watch":
		h.handleWatchCommand(chatID, from.ID, text, user.Language)
	case text == "/watchlist":
		h.sendWatchlistMenu(chatID, from.ID, user.Language)
	case text == "/stats":
		h.sendStats24h(chatID, user.Language)
	case text == "/hot":
		h.sendHotWallets(chatID, user.Language)
	case text == "/vip":
		h.sendVIPMenu(chatID, user.Language)
	default:
		h.sendStartMenu(chatID, from.FirstName, user)
	}
}

// ── ToS ────────────────────────────────────────────────────────────────────────

func (h *WebhookHandler) sendTosDisclaimer(chatID int64, lang string) {
	msg := "⚠️ <b>Внимание!</b> Smart Cluster Terminal — исключительно аналитический инструмент. Бот НЕ даёт финансовых советов и НЕ является призывом к инвестициям. Рынок криптовалют несёт сверхвысокие риски. Подтверждая, вы соглашаетесь с условиями и подтверждаете, что вам есть 18 лет."
	if lang == "en" {
		msg = "⚠️ <b>Disclaimer!</b> Smart Cluster Terminal is an analytical tool only. It does NOT provide financial advice. Crypto markets carry extreme risk of total loss. By confirming, you agree to the terms and confirm you are 18 or older."
	}
	kb := &InlineKeyboardMarkup{
		InlineKeyboard: [][]InlineKeyboardButton{
			{{Text: "✅ Принимаю / I Accept", CallbackData: "accept_tos"}},
		},
	}
	if err := h.client.SendMessageWithKeyboard(chatID, msg, kb); err != nil {
		log.Printf("[HANDLER] sendTosDisclaimer %d: %v", chatID, err)
	}
}

// ── Start menu ─────────────────────────────────────────────────────────────────

func (h *WebhookHandler) sendStartMenu(chatID int64, firstName string, user *storage.User) {
	body := h.buildStartMenuText(firstName, user)
	kb := h.buildStartMenuKB(user.Language)
	if err := h.client.SendMessageWithKeyboard(chatID, body, kb); err != nil {
		log.Printf("[HANDLER] sendStartMenu %d: %v", chatID, err)
	}
}

func (h *WebhookHandler) editStartMenu(chatID int64, msgID int, firstName string, user *storage.User) {
	body := h.buildStartMenuText(firstName, user)
	kb := h.buildStartMenuKB(user.Language)
	if err := h.client.EditMessageText(chatID, msgID, body, kb); err != nil {
		log.Printf("[HANDLER] editStartMenu fallback %d/%d: %v", chatID, msgID, err)
		_ = h.client.DeleteMessage(chatID, msgID)
		_ = h.client.SendMessageWithKeyboard(chatID, body, kb)
	}
}

func (h *WebhookHandler) buildStartMenuText(firstName string, user *storage.User) string {
	plan := "FREE"
	planIcon := "⬜"
	if user.IsVIP {
		plan = "VIP"
		planIcon = "👑"
	}
	lang := user.Language
	nets := enabledNetworksRaw(user)
	name := html.EscapeString(firstName)

	if lang == "ru" {
		return fmt.Sprintf(
			"⬛ <b>SMART CLUSTER TERMINAL v2.0</b>\n"+
				"<b>━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━</b>\n\n"+
				"<code>> SYSTEM_BOOT........ [OK]\n"+
				"> AUTH_USER......... %s\n"+
				"> ACCESS_LEVEL...... %s %s\n"+
				"> MIN_VOL_FILTER.... $%s\n"+
				"> ACTIVE_CHAINS..... %s\n"+
				"> LANG.............. RU\n"+
				"──────────────────────\n"+
				"[ 🟢 TERMINAL ONLINE ]</code>\n\n"+
				"Выберите модуль:",
			name, planIcon, plan,
			fmtVolume(user.MinVolume), nets,
		)
	}
	return fmt.Sprintf(
		"⬛ <b>SMART CLUSTER TERMINAL v2.0</b>\n"+
			"<b>━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━</b>\n\n"+
			"<code>> SYSTEM_BOOT........ [OK]\n"+
			"> AUTH_USER......... %s\n"+
			"> ACCESS_LEVEL...... %s %s\n"+
			"> MIN_VOL_FILTER.... $%s\n"+
			"> ACTIVE_CHAINS..... %s\n"+
			"> LANG.............. EN\n"+
			"──────────────────────\n"+
			"[ 🟢 TERMINAL ONLINE ]</code>\n\n"+
			"Select module:",
		name, planIcon, plan,
		fmtVolume(user.MinVolume), nets,
	)
}

func (h *WebhookHandler) buildStartMenuKB(lang string) *InlineKeyboardMarkup {
	return &InlineKeyboardMarkup{
		InlineKeyboard: [][]InlineKeyboardButton{
			{
				{Text: tr(lang, "📊 Open Terminal", "📊 Открыть Terminal"), WebApp: &WebAppInfo{URL: h.webAppURL()}},
			},
			{
				{Text: tr(lang, "🔥 Fresh Clusters", "🔥 Свежие кластеры"), CallbackData: "cb:clusters"},
				{Text: "📈 24h Stats", CallbackData: "cb:stats"},
			},
			{
				{Text: tr(lang, "⭐ Watchlist", "⭐ Watchlist"), CallbackData: "cb:watchlist"},
				{Text: tr(lang, "⚙️ Settings", "⚙️ Настройки"), CallbackData: "cb:settings"},
			},
			{
				{Text: tr(lang, "🔥 Hot Wallets", "🔥 Горячие кошельки"), CallbackData: "cb:hot"},
				{Text: tr(lang, "❓ Help", "❓ Помощь"), CallbackData: "cb:help"},
			},
			{
				{Text: tr(lang, "👑 VIP Pass", "👑 VIP Пасс"), CallbackData: "cb:vip"},
			},
		},
	}
}

// ── /watch command ─────────────────────────────────────────────────────────────

func (h *WebhookHandler) handleWatchCommand(chatID, userID int64, text, lang string) {
	parts := strings.Fields(text)
	if len(parts) < 2 {
		hint := "ℹ️ Usage: <code>/watch &lt;address&gt; [note]</code>"
		if lang == "ru" {
			hint = "ℹ️ Использование: <code>/watch &lt;адрес&gt; [заметка]</code>"
		}
		h.client.SendMessage(chatID, hint)
		return
	}
	addr := parts[1]
	note := strings.Join(parts[2:], " ")
	if err := h.storage.AddWatchlistWallet(userID, addr, note); err != nil {
		log.Printf("[HANDLER] AddWatchlistWallet %d: %v", userID, err)
		msg := "❌ Failed to add wallet."
		if lang == "ru" {
			msg = "❌ Не удалось добавить кошелёк."
		}
		h.client.SendMessage(chatID, msg)
		return
	}
	var reply string
	if lang == "ru" {
		reply = fmt.Sprintf(
			"✅ <b>Кошелёк добавлен в Watchlist</b>\n\n"+
				"<code>%s</code>\n📝 %s",
			html.EscapeString(addr), html.EscapeString(or(note, "—")),
		)
	} else {
		reply = fmt.Sprintf(
			"✅ <b>Wallet added to Watchlist</b>\n\n"+
				"<code>%s</code>\n📝 %s",
			html.EscapeString(addr), html.EscapeString(or(note, "—")),
		)
	}
	h.client.SendMessageWithKeyboard(chatID, reply, backToMenuKB(lang))
}

// ── Watchlist ──────────────────────────────────────────────────────────────────

func (h *WebhookHandler) sendWatchlistMenu(chatID, userID int64, lang string) {
	text, kb := h.buildWatchlistContent(userID, lang)
	if err := h.client.SendMessageWithKeyboard(chatID, text, kb); err != nil {
		log.Printf("[HANDLER] sendWatchlistMenu %d: %v", chatID, err)
	}
}

func (h *WebhookHandler) editWatchlistMenu(chatID int64, msgID int, userID int64, lang string) {
	text, kb := h.buildWatchlistContent(userID, lang)
	if err := h.client.EditMessageText(chatID, msgID, text, kb); err != nil {
		log.Printf("[HANDLER] editWatchlistMenu %d/%d: %v", chatID, msgID, err)
	}
}

func (h *WebhookHandler) buildWatchlistContent(userID int64, lang string) (string, *InlineKeyboardMarkup) {
	entries, _ := h.storage.GetWatchlist(userID)
	header := "⭐ <b>WATCHLIST</b>\n<b>━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━</b>\n\n"
	if len(entries) == 0 {
		empty := header + "No wallets tracked.\n\n<code>/watch &lt;address&gt; [note]</code>"
		if lang == "ru" {
			empty = header + "Список пуст.\n\n<code>/watch &lt;адрес&gt; [заметка]</code>"
		}
		return empty, backToMenuKB(lang)
	}
	var sb strings.Builder
	sb.WriteString(header)
	var rows [][]InlineKeyboardButton
	for _, e := range entries {
		masked := maskAddr(e.WalletAddress)
		sb.WriteString("• <code>" + html.EscapeString(masked) + "</code>")
		if e.Note != "" {
			sb.WriteString(" — " + html.EscapeString(e.Note))
		}
		sb.WriteString("\n")
		rows = append(rows, []InlineKeyboardButton{
			{Text: "🗑 " + masked, CallbackData: fmt.Sprintf("cb:watchrm:%d", e.ID)},
		})
	}
	rows = append(rows, []InlineKeyboardButton{{Text: tr(lang, "⬅️ Back", "⬅️ Назад"), CallbackData: "cb:menu"}})
	return sb.String(), &InlineKeyboardMarkup{InlineKeyboard: rows}
}

// ── Stats 24h ──────────────────────────────────────────────────────────────────

func (h *WebhookHandler) sendStats24h(chatID int64, lang string) {
	text, kb := h.buildStats24hContent(lang)
	if err := h.client.SendMessageWithKeyboard(chatID, text, kb); err != nil {
		log.Printf("[HANDLER] sendStats24h %d: %v", chatID, err)
	}
}

func (h *WebhookHandler) editStats24h(chatID int64, msgID int, lang string) {
	text, kb := h.buildStats24hContent(lang)
	if err := h.client.EditMessageText(chatID, msgID, text, kb); err != nil {
		log.Printf("[HANDLER] editStats24h %d/%d: %v", chatID, msgID, err)
	}
}

func (h *WebhookHandler) buildStats24hContent(lang string) (string, *InlineKeyboardMarkup) {
	stats, err := h.storage.GetStats24h()
	if err != nil || stats == nil {
		msg := "❌ Error loading statistics."
		if lang == "ru" {
			msg = "❌ Ошибка загрузки статистики."
		}
		return msg, backToMenuKB(lang)
	}

	topToken := html.EscapeString(or(stats.TopToken, "N/A"))
	topChain := html.EscapeString(or(stats.TopChain, "N/A"))

	var body string
	if lang == "ru" {
		body = "📈 <b>СТАТИСТИКА — 24h СКАН</b>\n" +
			"<b>━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━</b>\n\n" +
			fmt.Sprintf(
				"<code>> CLUSTERS_FOUND..... %d\n"+
					"> TOTAL_VOLUME...... $%s\n"+
					"> TOP_TOKEN......... %s\n"+
					"> TOP_CHAIN......... %s\n"+
					"──────────────────────\n"+
					"[ 🟢 SCAN COMPLETE ]</code>",
				stats.TotalClusters,
				fmtFloat(stats.TotalVolumeUSD),
				topToken,
				topChain,
			)
	} else {
		body = "📈 <b>STATISTICS — 24h SCAN</b>\n" +
			"<b>━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━</b>\n\n" +
			fmt.Sprintf(
				"<code>> CLUSTERS_FOUND..... %d\n"+
					"> TOTAL_VOLUME...... $%s\n"+
					"> TOP_TOKEN......... %s\n"+
					"> TOP_CHAIN......... %s\n"+
					"──────────────────────\n"+
					"[ 🟢 SCAN COMPLETE ]</code>",
				stats.TotalClusters,
				fmtFloat(stats.TotalVolumeUSD),
				topToken,
				topChain,
			)
	}
	return body, backToMenuKB(lang)
}

// ── Hot Wallets ────────────────────────────────────────────────────────────────

func (h *WebhookHandler) sendHotWallets(chatID int64, lang string) {
	text, kb := h.buildHotWalletsContent(lang)
	if err := h.client.SendMessageWithKeyboard(chatID, text, kb); err != nil {
		log.Printf("[HANDLER] sendHotWallets %d: %v", chatID, err)
	}
}

func (h *WebhookHandler) editHotWallets(chatID int64, msgID int, lang string) {
	text, kb := h.buildHotWalletsContent(lang)
	if err := h.client.EditMessageText(chatID, msgID, text, kb); err != nil {
		log.Printf("[HANDLER] editHotWallets %d/%d: %v", chatID, msgID, err)
	}
}

func (h *WebhookHandler) buildHotWalletsContent(lang string) (string, *InlineKeyboardMarkup) {
	wallets, _ := h.storage.GetTopWallets(24, 5)
	header := "🔥 <b>HOT WALLETS — 24h</b>\n<b>━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━</b>\n\n"
	if len(wallets) == 0 {
		msg := header + "<code>> NO_DATA_YET</code>"
		return msg, backToMenuKB(lang)
	}
	var sb strings.Builder
	sb.WriteString(header)
	medals := []string{"🥇", "🥈", "🥉", "4️⃣", "5️⃣"}
	for i, w := range wallets {
		medal := "•"
		if i < len(medals) {
			medal = medals[i]
		}
		sb.WriteString(fmt.Sprintf(
			"%s <code>%s</code>\n   <code>clusters: %d  vol: $%s</code>\n\n",
			medal,
			html.EscapeString(maskAddr(w.WalletAddress)),
			w.ClusterCount,
			fmtFloat(w.TotalVolumeUSD),
		))
	}
	if lang == "ru" {
		sb.WriteString("<i>Повторные покупки = наибольшая убеждённость</i>")
	} else {
		sb.WriteString("<i>Repeated buys = highest conviction smart money</i>")
	}
	return sb.String(), backToMenuKB(lang)
}

// ── Recent Clusters ────────────────────────────────────────────────────────────

func (h *WebhookHandler) editRecentClusters(chatID int64, msgID int, lang string) {
	clusters, err := h.storage.GetRecentClusters(5)
	var body string
	header := "🔥 <b>FRESH CLUSTERS</b>\n<b>━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━</b>\n\n"
	if err != nil || len(clusters) == 0 {
		body = header + "<code>> NO_DATA_YET</code>"
		if lang == "ru" {
			body = "🔥 <b>СВЕЖИЕ КЛАСТЕРЫ</b>\n<b>━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━</b>\n\n<code>> NO_DATA_YET</code>"
		}
		_ = h.client.EditMessageText(chatID, msgID, body, backToMenuKB(lang))
		return
	}
	if lang == "ru" {
		header = "🔥 <b>СВЕЖИЕ КЛАСТЕРЫ</b>\n<b>━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━</b>\n\n"
	}
	var sb strings.Builder
	sb.WriteString(header)
	for _, c := range clusters {
		sb.WriteString(fmt.Sprintf(
			"• <b>%s</b> <code>[%s]</code>\n"+
				"  <code>vol: $%s  buys: %d</code>\n"+
				"  <code>%s</code>\n\n",
			html.EscapeString(c.TokenSymbol),
			html.EscapeString(c.Chain),
			fmtFloat(c.TotalVolumeUSD),
			c.BuyCount,
			c.TokenAddress,
		))
	}
	if err := h.client.EditMessageText(chatID, msgID, sb.String(), backToMenuKB(lang)); err != nil {
		log.Printf("[HANDLER] editRecentClusters %d/%d: %v", chatID, msgID, err)
	}
}

// ── Callback routing ───────────────────────────────────────────────────────────

func (h *WebhookHandler) handleCallback(cb *CallbackQuery) {
	chatID := int64(0)
	msgID := 0
	if cb.Message != nil {
		chatID = cb.Message.Chat.ID
		msgID = cb.Message.MessageID
	}
	userID := cb.From.ID
	data := cb.Data

	if err := h.client.AnswerCallbackQuery(cb.ID, ""); err != nil {
		log.Printf("[HANDLER] AnswerCallbackQuery %s: %v", cb.ID, err)
	}

	bootstrapLang := "en"
	if strings.HasPrefix(strings.ToLower(cb.From.LanguageCode), "ru") {
		bootstrapLang = "ru"
	}
	user, err := h.storage.GetOrCreateUser(userID, cb.From.Username, bootstrapLang)
	if err != nil {
		log.Printf("[HANDLER] GetOrCreateUser callback %d: %v", userID, err)
		return
	}
	lang := user.Language

	switch {
	case data == "accept_tos":
		_ = h.storage.SetUserTosAccepted(userID, true)
		user.TosAccepted = true
		_ = h.client.DeleteMessage(chatID, msgID)
		h.sendStartMenu(chatID, cb.From.FirstName, user)

	case data == "cb:menu":
		h.editStartMenu(chatID, msgID, cb.From.FirstName, user)

	case data == "cb:clusters":
		h.editRecentClusters(chatID, msgID, lang)

	case data == "cb:stats":
		h.editStats24h(chatID, msgID, lang)

	case data == "cb:watchlist":
		h.editWatchlistMenu(chatID, msgID, userID, lang)

	case data == "cb:hot":
		h.editHotWallets(chatID, msgID, lang)

	case data == "cb:settings":
		h.editSettingsMenu(chatID, msgID, userID, user)

	case data == "cb:vip":
		h.editVIPInfo(chatID, msgID, lang)

	case data == "cb:help":
		h.editHelp(chatID, msgID, lang)

	case data == "pay:stars":
		h.editPaymentStars(chatID, msgID, lang)

	case data == "pay:cryptobot":
		h.editPaymentCryptoBot(chatID, msgID, userID, lang)

	// CryptoBot payment check: "pay:cbcheck:<invoiceID>"
	case strings.HasPrefix(data, "pay:cbcheck:"):
		h.handleCryptoBotCheck(chatID, msgID, userID, data, lang)

	case data == "cb:lang":
		newLang := "ru"
		if user.Language == "ru" {
			newLang = "en"
		}
		_ = h.storage.SetUserLanguage(userID, newLang)
		user.Language = newLang
		h.editSettingsMenu(chatID, msgID, userID, user)

	case strings.HasPrefix(data, "cb:vol:"):
		h.handleVolumeChange(chatID, msgID, userID, cb.From.Username, lang, data)

	case strings.HasPrefix(data, "cb:net:"):
		h.handleNetworkToggle(chatID, msgID, userID, cb.From.Username, lang, data)

	case strings.HasPrefix(data, "cb:watchrm:"):
		h.handleWatchlistRemove(chatID, msgID, userID, data, lang)
	}
}

// ── Settings ───────────────────────────────────────────────────────────────────

func (h *WebhookHandler) editSettingsMenu(chatID int64, msgID int, userID int64, user *storage.User) {
	lang := user.Language
	langLabel := "EN"
	if lang == "ru" {
		langLabel = "RU"
	}

	var body string
	if lang == "ru" {
		body = "⚙️ <b>НАСТРОЙКИ</b>\n" +
			"<b>━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━</b>\n\n" +
			fmt.Sprintf(
				"<code>> MIN_VOL_FILTER.... $%s\n"+
					"> ACTIVE_CHAINS..... %s\n"+
					"> LANG.............. %s\n"+
					"──────────────────────\n"+
					"[ 🔧 CONFIG MODE ]</code>",
				fmtVolume(user.MinVolume),
				enabledNetworksRaw(user),
				langLabel,
			)
	} else {
		body = "⚙️ <b>SETTINGS</b>\n" +
			"<b>━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━</b>\n\n" +
			fmt.Sprintf(
				"<code>> MIN_VOL_FILTER.... $%s\n"+
					"> ACTIVE_CHAINS..... %s\n"+
					"> LANG.............. %s\n"+
					"──────────────────────\n"+
					"[ 🔧 CONFIG MODE ]</code>",
				fmtVolume(user.MinVolume),
				enabledNetworksRaw(user),
				langLabel,
			)
	}

	langToggle := "🌐 Switch → RU"
	if lang == "ru" {
		langToggle = "🌐 Switch → EN"
	}

	kb := &InlineKeyboardMarkup{
		InlineKeyboard: [][]InlineKeyboardButton{
			{
				{Text: "$10k", CallbackData: fmt.Sprintf("cb:vol:%d:10000", userID)},
				{Text: "$25k", CallbackData: fmt.Sprintf("cb:vol:%d:25000", userID)},
				{Text: "$50k", CallbackData: fmt.Sprintf("cb:vol:%d:50000", userID)},
				{Text: "$100k", CallbackData: fmt.Sprintf("cb:vol:%d:100000", userID)},
			},
			{
				{Text: emojiCheck(user.EthEnabled) + " ETH", CallbackData: fmt.Sprintf("cb:net:%d:eth", userID)},
				{Text: emojiCheck(user.SolEnabled) + " SOL", CallbackData: fmt.Sprintf("cb:net:%d:sol", userID)},
				{Text: emojiCheck(user.BaseEnabled) + " BASE", CallbackData: fmt.Sprintf("cb:net:%d:base", userID)},
				{Text: emojiCheck(user.BscEnabled) + " BSC", CallbackData: fmt.Sprintf("cb:net:%d:bsc", userID)},
			},
			{{Text: langToggle, CallbackData: "cb:lang"}},
			{{Text: tr(lang, "⬅️ Back", "⬅️ Назад"), CallbackData: "cb:menu"}},
		},
	}
	if err := h.client.EditMessageText(chatID, msgID, body, kb); err != nil {
		log.Printf("[HANDLER] editSettingsMenu %d/%d: %v", chatID, msgID, err)
	}
}

func (h *WebhookHandler) handleVolumeChange(chatID int64, msgID int, userID int64, username, lang, data string) {
	parts := strings.Split(data, ":")
	if len(parts) < 4 {
		return
	}
	vol, err := strconv.Atoi(parts[3])
	if err != nil {
		return
	}
	user, err := h.storage.GetOrCreateUser(userID, username, lang)
	if err != nil {
		return
	}
	if err := h.storage.UpdateUserSettings(userID, vol, user.EthEnabled, user.SolEnabled, user.BaseEnabled, user.BscEnabled); err != nil {
		log.Printf("[HANDLER] UpdateUserSettings %d: %v", userID, err)
		return
	}
	user.MinVolume = vol
	h.editSettingsMenu(chatID, msgID, userID, user)
}

func (h *WebhookHandler) handleNetworkToggle(chatID int64, msgID int, userID int64, username, lang, data string) {
	parts := strings.Split(data, ":")
	if len(parts) < 4 {
		return
	}
	user, err := h.storage.GetOrCreateUser(userID, username, lang)
	if err != nil {
		return
	}
	switch parts[3] {
	case "eth":
		user.EthEnabled = !user.EthEnabled
	case "sol":
		user.SolEnabled = !user.SolEnabled
	case "base":
		user.BaseEnabled = !user.BaseEnabled
	case "bsc":
		user.BscEnabled = !user.BscEnabled
	}
	if err := h.storage.UpdateUserSettings(userID, user.MinVolume,
		user.EthEnabled, user.SolEnabled, user.BaseEnabled, user.BscEnabled); err != nil {
		log.Printf("[HANDLER] UpdateUserSettings net %d: %v", userID, err)
	}
	h.editSettingsMenu(chatID, msgID, userID, user)
}

func (h *WebhookHandler) handleWatchlistRemove(chatID int64, msgID int, userID int64, data, lang string) {
	parts := strings.Split(data, ":")
	if len(parts) < 3 {
		return
	}
	entryID, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return
	}
	_ = h.storage.RemoveWatchlistWallet(userID, entryID)
	h.editWatchlistMenu(chatID, msgID, userID, lang)
}

// ── VIP Info ───────────────────────────────────────────────────────────────────

func (h *WebhookHandler) editVIPInfo(chatID int64, msgID int, lang string) {
	body, kb := h.buildVIPContent(lang)
	if err := h.client.EditMessageText(chatID, msgID, body, kb); err != nil {
		log.Printf("[HANDLER] editVIPInfo %d/%d: %v", chatID, msgID, err)
	}
}

func (h *WebhookHandler) sendVIPMenu(chatID int64, lang string) {
	body, kb := h.buildVIPContent(lang)
	if err := h.client.SendMessageWithKeyboard(chatID, body, kb); err != nil {
		log.Printf("[HANDLER] sendVIPMenu %d: %v", chatID, err)
	}
}

func (h *WebhookHandler) buildVIPContent(lang string) (string, *InlineKeyboardMarkup) {
	var body string
	if lang == "ru" {
		body = "👑 <b>VIP ACCESS — SMART CLUSTER TERMINAL</b>\n" +
			"<b>━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━</b>\n\n" +
			"<code>> MASK.............. DISABLED\n" +
			"> ALERTS............ UNLIMITED\n" +
			"> ARCHIVE........... FULL\n" +
			"> EXPORT............ CSV_ON\n" +
			"> HOT_WALLETS....... PRIORITY\n" +
			"──────────────────────\n" +
			"[ 💳 PRICE: $9.99 / 30d ]</code>\n\n" +
			"Выберите способ оплаты:"
	} else {
		body = "👑 <b>VIP ACCESS — SMART CLUSTER TERMINAL</b>\n" +
			"<b>━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━</b>\n\n" +
			"<code>> MASK.............. DISABLED\n" +
			"> ALERTS............ UNLIMITED\n" +
			"> ARCHIVE........... FULL\n" +
			"> EXPORT............ CSV_ON\n" +
			"> HOT_WALLETS....... PRIORITY\n" +
			"──────────────────────\n" +
			"[ 💳 PRICE: $9.99 / 30d ]</code>\n\n" +
			"Select payment method:"
	}
	kb := &InlineKeyboardMarkup{
		InlineKeyboard: [][]InlineKeyboardButton{
			{{Text: "⭐ Telegram Stars (XTR)", CallbackData: "pay:stars"}},
			{{Text: "🤖 CryptoBot — USDT / TON", CallbackData: "pay:cryptobot"}},
			{{Text: tr(lang, "⬅️ Back", "⬅️ Назад"), CallbackData: "cb:menu"}},
		},
	}
	return body, kb
}

// ── Payment: Telegram Stars ────────────────────────────────────────────────────
// Stars use Telegram's native invoice flow. We send an invoice link.
// Telegram delivers a SuccessfulPayment message to our webhook when paid —
// handled in handleSuccessfulPayment above.

func (h *WebhookHandler) editPaymentStars(chatID int64, msgID int, lang string) {
	var body string
	if lang == "ru" {
		body = "⭐ <b>ОПЛАТА — TELEGRAM STARS</b>\n" +
			"<b>━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━</b>\n\n" +
			"<code>> METHOD......... STARS (XTR)\n" +
			"> AMOUNT......... 250 XTR\n" +
			"> DURATION....... 30 DAYS\n" +
			"> AUTO_ACTIVATE.. YES\n" +
			"──────────────────────\n" +
			"[ ⚡ INSTANT ACTIVATION ]</code>\n\n" +
			"После оплаты VIP активируется автоматически."
	} else {
		body = "⭐ <b>PAYMENT — TELEGRAM STARS</b>\n" +
			"<b>━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━</b>\n\n" +
			"<code>> METHOD......... STARS (XTR)\n" +
			"> AMOUNT......... 250 XTR\n" +
			"> DURATION....... 30 DAYS\n" +
			"> AUTO_ACTIVATE.. YES\n" +
			"──────────────────────\n" +
			"[ ⚡ INSTANT ACTIVATION ]</code>\n\n" +
			"VIP activates automatically after payment."
	}

	// The Stars invoice URL must be generated by calling sendInvoice via the
	// Telegram Bot API. Until you wire that up, this link opens @StarkWonder
	// for manual Stars transfer. Replace with your sendInvoice call when ready.
	// The invoice payload "vip_stars_30d" is what handleSuccessfulPayment checks.
	kb := &InlineKeyboardMarkup{
		InlineKeyboard: [][]InlineKeyboardButton{
			{{Text: tr(lang, "⭐ Pay 250 Stars", "⭐ Оплатить 250 Stars"),
				URL: "https://t.me/StarkWonder?start=stars_vip"}},
			{{Text: tr(lang, "⬅️ Back", "⬅️ Назад"), CallbackData: "cb:vip"}},
		},
	}
	if err := h.client.EditMessageText(chatID, msgID, body, kb); err != nil {
		log.Printf("[HANDLER] editPaymentStars %d/%d: %v", chatID, msgID, err)
	}
}

// ── Payment: CryptoBot ─────────────────────────────────────────────────────────

func (h *WebhookHandler) editPaymentCryptoBot(chatID int64, msgID int, userID int64, lang string) {
	// Show loading state immediately so user gets feedback while we call the API.
	loadMsg := tr(lang, "⏳ <code>CONNECTING TO CRYPTOBOT...</code>", "⏳ <code>ПОДКЛЮЧЕНИЕ К CRYPTOBOT...</code>")
	_ = h.client.EditMessageText(chatID, msgID, loadMsg, nil)

	invoice, err := h.createCryptoBotInvoice(userID)
	if err != nil {
		log.Printf("[HANDLER] createCryptoBotInvoice %d: %v", userID, err)
		errMsg := "❌ <code>CRYPTOBOT_UNAVAILABLE</code>\n\nContact @StarkWonder."
		if lang == "ru" {
			errMsg = "❌ <code>CRYPTOBOT_UNAVAILABLE</code>\n\nСвяжитесь с @StarkWonder."
		}
		kb := &InlineKeyboardMarkup{
			InlineKeyboard: [][]InlineKeyboardButton{
				{{Text: tr(lang, "⬅️ Back", "⬅️ Назад"), CallbackData: "cb:vip"}},
			},
		}
		_ = h.client.EditMessageText(chatID, msgID, errMsg, kb)
		return
	}

	var body string
	if lang == "ru" {
		body = "🤖 <b>ОПЛАТА — CRYPTOBOT</b>\n" +
			"<b>━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━</b>\n\n" +
			fmt.Sprintf(
				"<code>> METHOD......... CRYPTOBOT\n"+
					"> AMOUNT......... $9.99 USDT\n"+
					"> INVOICE_ID..... %d\n"+
					"> DURATION....... 30 DAYS\n"+
					"> AUTO_ACTIVATE.. YES\n"+
					"──────────────────────\n"+
					"[ ⚡ INVOICE READY ]</code>\n\n"+
					"После оплаты нажмите «Проверить оплату».",
				invoice.ID,
			)
	} else {
		body = "🤖 <b>PAYMENT — CRYPTOBOT</b>\n" +
			"<b>━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━</b>\n\n" +
			fmt.Sprintf(
				"<code>> METHOD......... CRYPTOBOT\n"+
					"> AMOUNT......... $9.99 USDT\n"+
					"> INVOICE_ID..... %d\n"+
					"> DURATION....... 30 DAYS\n"+
					"> AUTO_ACTIVATE.. YES\n"+
					"──────────────────────\n"+
					"[ ⚡ INVOICE READY ]</code>\n\n"+
					"After paying, tap «Check Payment».",
				invoice.ID,
			)
	}

	checkBtn := tr(lang, "🔄 Check Payment", "🔄 Проверить оплату")
	kb := &InlineKeyboardMarkup{
		InlineKeyboard: [][]InlineKeyboardButton{
			{{Text: tr(lang, "💳 Pay Invoice", "💳 Оплатить счёт"), URL: invoice.PayURL}},
			{{Text: checkBtn, CallbackData: fmt.Sprintf("pay:cbcheck:%d", invoice.ID)}},
			{{Text: tr(lang, "⬅️ Back", "⬅️ Назад"), CallbackData: "cb:vip"}},
		},
	}
	if err := h.client.EditMessageText(chatID, msgID, body, kb); err != nil {
		log.Printf("[HANDLER] editPaymentCryptoBot final %d/%d: %v", chatID, msgID, err)
	}
}

// handleCryptoBotCheck polls getInvoices and activates VIP if status == "paid".
func (h *WebhookHandler) handleCryptoBotCheck(chatID int64, msgID int, userID int64, data, lang string) {
	// data format: "pay:cbcheck:<invoiceID>"
	parts := strings.SplitN(data, ":", 3)
	if len(parts) < 3 {
		return
	}
	invoiceID, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return
	}

	status, err := h.getCryptoBotInvoiceStatus(invoiceID)
	if err != nil {
		log.Printf("[HANDLER] getCryptoBotInvoiceStatus %d: %v", invoiceID, err)
		notice := tr(lang, "⚠️ Could not reach CryptoBot. Try again.", "⚠️ Не удалось подключиться. Попробуйте ещё раз.")
		if err2 := h.client.AnswerCallbackQuery("", notice); err2 != nil {
			log.Printf("[HANDLER] answerCallback notice: %v", err2)
		}
		return
	}

	if status != "paid" {
		notice := tr(lang,
			"⏳ Not paid yet. Complete the invoice then try again.",
			"⏳ Оплата не найдена. Оплатите счёт и проверьте снова.")
		// AnswerCallbackQuery was already called at the top of handleCallback.
		// We cannot call it again for the same queryID (it's already answered).
		// Instead, update the message text with a status line.
		// Re-show the cryptobot screen with a status note appended.
		statusNote := "\n\n<code>> PAYMENT_STATUS.... PENDING ⏳</code>"
		// fetch current message text and append — simplest: just re-edit with note
		_ = h.client.EditMessageText(chatID, msgID,
			fmt.Sprintf("<code>> PAYMENT_STATUS.... PENDING ⏳\n> Try again after completing the invoice.</code>\n\n%s",
				notice),
			&InlineKeyboardMarkup{
				InlineKeyboard: [][]InlineKeyboardButton{
					{{Text: tr(lang, "🔄 Check Again", "🔄 Проверить снова"),
						CallbackData: fmt.Sprintf("pay:cbcheck:%d", invoiceID)}},
					{{Text: tr(lang, "⬅️ Back", "⬅️ Назад"), CallbackData: "cb:vip"}},
				},
			},
		)
		_ = statusNote
		return
	}

	// Status is "paid" — activate VIP.
	if err := h.storage.SetUserVIP(userID, true); err != nil {
		log.Printf("[HANDLER] SetUserVIP (cryptobot) %d: %v", userID, err)
		h.client.SendMessage(chatID, "⚠️ Payment verified but VIP activation failed. Contact @StarkWonder.")
		return
	}
	log.Printf("[PAYMENT] CryptoBot VIP activated for user %d (invoice %d)", userID, invoiceID)
	h.sendVIPActivatedMessage(chatID, lang)
}

// cryptoBotInvoice holds what we need from a createInvoice response.
type cryptoBotInvoice struct {
	ID     int64
	PayURL string
}

// createCryptoBotInvoice calls POST /api/createInvoice and returns the invoice.
func (h *WebhookHandler) createCryptoBotInvoice(userID int64) (*cryptoBotInvoice, error) {
	token := h.cryptoBotToken()
	if token == "" {
		return nil, fmt.Errorf("CRYPTOBOT_TOKEN not configured")
	}

	payload := map[string]interface{}{
		"asset":       "USDT",
		"amount":      "9.99",
		"description": "Smart Cluster Terminal — VIP Pass (30 days)",
		"payload":     fmt.Sprintf("vip_%d_%d", userID, time.Now().Unix()),
	}
	jsonBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}
	req, err := http.NewRequest(http.MethodPost, "https://pay.crypt.bot/api/createInvoice", bytes.NewBuffer(jsonBytes))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Crypto-Pay-API-Token", token)

	resp, err := (&http.Client{Timeout: 8 * time.Second}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("http do: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		Ok     bool   `json:"ok"`
		Error  string `json:"error"`
		Result struct {
			InvoiceID int64  `json:"invoice_id"`
			PayURL    string `json:"pay_url"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	if !result.Ok {
		return nil, fmt.Errorf("CryptoBot API error: %s", result.Error)
	}
	if result.Result.PayURL == "" {
		return nil, fmt.Errorf("empty pay_url in response")
	}
	return &cryptoBotInvoice{
		ID:     result.Result.InvoiceID,
		PayURL: result.Result.PayURL,
	}, nil
}

// getCryptoBotInvoiceStatus calls GET /api/getInvoices?invoice_ids=<id>
// and returns the status string ("active", "paid", "expired").
func (h *WebhookHandler) getCryptoBotInvoiceStatus(invoiceID int64) (string, error) {
	token := h.cryptoBotToken()
	if token == "" {
		return "", fmt.Errorf("CRYPTOBOT_TOKEN not configured")
	}

	url := fmt.Sprintf("https://pay.crypt.bot/api/getInvoices?invoice_ids=%d", invoiceID)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Crypto-Pay-API-Token", token)

	resp, err := (&http.Client{Timeout: 8 * time.Second}).Do(req)
	if err != nil {
		return "", fmt.Errorf("http do: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		Ok     bool   `json:"ok"`
		Error  string `json:"error"`
		Result struct {
			Items []struct {
				InvoiceID int64  `json:"invoice_id"`
				Status    string `json:"status"`
			} `json:"items"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode: %w", err)
	}
	if !result.Ok {
		return "", fmt.Errorf("CryptoBot API: %s", result.Error)
	}
	if len(result.Result.Items) == 0 {
		return "", fmt.Errorf("invoice %d not found", invoiceID)
	}
	return result.Result.Items[0].Status, nil
}

func (h *WebhookHandler) cryptoBotToken() string {
	if h.config != nil && h.config.CryptoBotToken != "" {
		return h.config.CryptoBotToken
	}
	return os.Getenv("CRYPTOBOT_TOKEN")
}

// ── Help ───────────────────────────────────────────────────────────────────────

func (h *WebhookHandler) editHelp(chatID int64, msgID int, lang string) {
	var body string
	if lang == "ru" {
		body = "❓ <b>HELP — КОМАНДЫ</b>\n" +
			"<b>━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━</b>\n\n" +
			"<code>/start</code>          — главное меню\n" +
			"<code>/watch &lt;addr&gt;</code>  — добавить в watchlist\n" +
			"<code>/watchlist</code>      — мой watchlist\n" +
			"<code>/stats</code>          — статистика 24h\n" +
			"<code>/hot</code>            — горячие кошельки\n" +
			"<code>/vip</code>            — VIP пасс\n\n" +
			"<b>━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━</b>\n" +
			"<b>Как работают кластеры:</b>\n" +
			"Система обнаруживает кластер когда <b>≥3</b> смарт-мани кошелька " +
			"аккумулируют один токен в течение <b>5 минут</b>.\n\n" +
			"🛡 <b>ВСЕГДА проверяйте токен через RugCheck до покупки!</b>"
	} else {
		body = "❓ <b>HELP — COMMANDS</b>\n" +
			"<b>━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━</b>\n\n" +
			"<code>/start</code>          — main menu\n" +
			"<code>/watch &lt;addr&gt;</code>  — add to watchlist\n" +
			"<code>/watchlist</code>      — my watchlist\n" +
			"<code>/stats</code>          — 24h statistics\n" +
			"<code>/hot</code>            — hot wallets\n" +
			"<code>/vip</code>            — VIP pass\n\n" +
			"<b>━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━</b>\n" +
			"<b>How clusters work:</b>\n" +
			"A cluster fires when <b>≥3</b> smart money wallets accumulate " +
			"the same token within <b>5 minutes</b>.\n\n" +
			"🛡 <b>ALWAYS verify tokens via RugCheck before buying!</b>"
	}
	if err := h.client.EditMessageText(chatID, msgID, body, backToMenuKB(lang)); err != nil {
		log.Printf("[HANDLER] editHelp %d/%d: %v", chatID, msgID, err)
	}
}

// ── Manual ─────────────────────────────────────────────────────────────────────

func (h *WebhookHandler) sendManual(chatID int64, lang string) {
	var body string
	if lang == "ru" {
		body = "📖 <b>РУКОВОДСТВО — Smart Cluster Terminal</b>\n" +
			"<b>━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━</b>\n\n" +
			"<b>1. Что такое кластер?</b>\n" +
			"≥3 независимых Smart Money кошелька аккумулируют один токен за 5 минут.\n\n" +
			"<b>2. Золотое правило:</b>\n" +
			"🛡 Всегда проверяйте через RugCheck! До 90% новых токенов — скам.\n\n" +
			"<b>3. Как пользоваться:</b>\n" +
			"• Открывайте WebApp-терминал для анализа в реальном времени\n" +
			"• Настройте фильтры объёма и сети в ⚙️ Настройках\n" +
			"• Добавляйте кошельки: <code>/watch &lt;addr&gt;</code>\n\n" +
			"⚠️ Это аналитический инструмент. Не финансовый совет (NFA / DYOR)."
	} else {
		body = "📖 <b>USER GUIDE — Smart Cluster Terminal</b>\n" +
			"<b>━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━</b>\n\n" +
			"<b>1. What is a cluster?</b>\n" +
			"≥3 independent Smart Money wallets accumulate the same token within 5 minutes.\n\n" +
			"<b>2. The golden rule:</b>\n" +
			"🛡 Always check via RugCheck first! Up to 90% of new tokens are scams.\n\n" +
			"<b>3. How to use:</b>\n" +
			"• Open the WebApp terminal for real-time interactive analysis\n" +
			"• Configure volume & network filters in ⚙️ Settings\n" +
			"• Track wallets: <code>/watch &lt;addr&gt;</code>\n\n" +
			"⚠️ This is an analytical tool. Not financial advice (NFA / DYOR)."
	}
	h.client.SendMessageWithKeyboard(chatID, body, backToMenuKB(lang))
}

// ── URL helpers ────────────────────────────────────────────────────────────────

func (h *WebhookHandler) webAppURL() string {
	if u := os.Getenv("WEBAPP_URL"); u != "" {
		return u
	}
	if h.config != nil && h.config.RenderURL != "" {
		return h.config.RenderURL + "/app"
	}
	return "http://localhost:8080/app"
}

// ── Shared UI helpers ──────────────────────────────────────────────────────────

func backToMenuKB(lang string) *InlineKeyboardMarkup {
	text := "⬅️ Main Menu"
	if lang == "ru" {
		text = "⬅️ Главное меню"
	}
	return &InlineKeyboardMarkup{
		InlineKeyboard: [][]InlineKeyboardButton{
			{{Text: text, CallbackData: "cb:menu"}},
		},
	}
}

func tr(lang, en, ru string) string {
	if lang == "ru" {
		return ru
	}
	return en
}

// enabledNetworksRaw returns a plain-text comma list for use inside <code> blocks.
// Do NOT use html.EscapeString on the output — it's already safe plain ASCII.
func enabledNetworksRaw(u *storage.User) string {
	var nets []string
	if u.EthEnabled {
		nets = append(nets, "ETH")
	}
	if u.SolEnabled {
		nets = append(nets, "SOL")
	}
	if u.BaseEnabled {
		nets = append(nets, "BASE")
	}
	if u.BscEnabled {
		nets = append(nets, "BSC")
	}
	if len(nets) == 0 {
		return "NONE"
	}
	return strings.Join(nets, "+")
}

func emojiCheck(v bool) string {
	if v {
		return "✅"
	}
	return "⬜"
}

func maskAddr(addr string) string {
	if len(addr) <= 10 {
		return addr
	}
	return addr[:6] + "…" + addr[len(addr)-4:]
}

func fmtVolume(v int) string { return fmtFloat(float64(v)) }

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
