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

// WebhookHandler is an http.Handler that receives Telegram webhook updates,
// immediately returns 200 OK, then processes the update asynchronously.
type WebhookHandler struct {
	client  *Client
	storage *storage.Storage
	config  *config.Config
	i18n    *i18n.Bundle
}

// NewWebhookHandler wires dependencies together.
func NewWebhookHandler(client *Client, store *storage.Storage, cfg *config.Config, bundle *i18n.Bundle) *WebhookHandler {
	return &WebhookHandler{
		client:  client,
		storage: store,
		config:  cfg,
		i18n:    bundle,
	}
}

// ServeHTTP implements http.Handler.
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

// dispatch routes an update to the correct handler based on its type.
func (h *WebhookHandler) dispatch(u *Update) {
	switch {
	case u.CallbackQuery != nil:
		h.handleCallback(u.CallbackQuery)
	case u.Message != nil && u.Message.From != nil:
		h.handleMessage(u.Message)
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

	// Gatekeeper: ToS must be accepted before anything works.
	if !user.TosAccepted {
		h.sendTosDisclaimer(chatID, user.Language)
		return
	}

	// Deep-link referral: /start ref_<referrerID>
	if strings.HasPrefix(text, "/start ref_") {
		parts := strings.SplitN(text, " ", 2)
		if len(parts) == 2 {
			h.handleReferral(from.ID, parts[1])
		}
		h.sendStartMenu(chatID, from.FirstName, user)
		return
	}

	switch {
	case text == "/start":
		h.sendStartMenu(chatID, from.FirstName, user)

	case text == "/manual" || text == "/help":
		h.sendManual(chatID, user.Language)

	case strings.HasPrefix(text, "/watch"):
		h.handleWatchCommand(chatID, from.ID, text, user.Language)

	case text == "/watchlist":
		h.sendWatchlistMenu(chatID, from.ID, user.Language)

	case text == "/stats":
		h.sendStats24h(chatID, user.Language)

	case text == "/hot":
		h.sendHotWallets(chatID, user.Language)

	case text == "/vip":
		h.sendVIPMenu(chatID, user.Language)

	case text == "/ref" || text == "/referral":
		h.sendReferralMenu(chatID, from.ID, user.Language)

	default:
		h.sendStartMenu(chatID, from.FirstName, user)
	}
}

// handleReferral credits a referrer when a new user arrives via their link.
// It is a no-op if the referrer is the same as the new user.
func (h *WebhookHandler) handleReferral(newUserID int64, payload string) {
	if !strings.HasPrefix(payload, "ref_") {
		return
	}
	referrerID, err := strconv.ParseInt(strings.TrimPrefix(payload, "ref_"), 10, 64)
	if err != nil || referrerID == newUserID {
		return
	}
	if err := h.storage.AddReferral(referrerID, newUserID); err != nil {
		log.Printf("[HANDLER] AddReferral %d→%d: %v", referrerID, newUserID, err)
	}
}

// ── ToS Disclaimer ─────────────────────────────────────────────────────────────

func (h *WebhookHandler) sendTosDisclaimer(chatID int64, lang string) {
	msg := "⚠️ <b>Внимание!</b> Smart Cluster Terminal — это исключительно аналитический инструмент для отслеживания публичных транзакций в блокчейне. Бот НЕ дает финансовых советов (Not Financial Advice) и НЕ является призывом к инвестициям. Рынок криптовалют несет сверхвысокие риски полной потери средств. Создатели бота не несут никакой ответственности за ваши торговые решения и финансовые потери. Вы используете сервис на свой страх и риск. Подтверждая, вы соглашаетесь с условиями и подтверждаете, что вам есть 18 лет."
	if lang == "en" {
		msg = "⚠️ <b>Disclaimer!</b> Smart Cluster Terminal is an analytical tool only. It does NOT provide financial advice and is NOT an invitation to invest. Crypto markets carry extreme risk of total loss. The creators are not responsible for any trading decisions or financial losses. You use this service at your own risk. By confirming, you agree to the terms and confirm you are 18 or older."
	}

	kb := &InlineKeyboardMarkup{
		InlineKeyboard: [][]InlineKeyboardButton{
			{{Text: "✅ Я прочитал, принимаю риски и мне есть 18 лет", CallbackData: "accept_tos"}},
		},
	}

	if err := h.client.SendMessageWithKeyboard(chatID, msg, kb); err != nil {
		log.Printf("[HANDLER] sendTosDisclaimer %d: %v", chatID, err)
	}
}

// ── /start menu (text-only, edit-in-place friendly) ────────────────────────────

func (h *WebhookHandler) sendStartMenu(chatID int64, firstName string, user *storage.User) {
	body := h.buildStartMenuText(firstName, user)
	kb := h.buildStartMenuKB(user.Language)
	if err := h.client.SendMessageWithKeyboard(chatID, body, kb); err != nil {
		log.Printf("[HANDLER] sendStartMenu %d: %v", chatID, err)
	}
}

// editStartMenu morphs an existing message bubble in-place back to the main menu.
// This is the ONLY correct implementation for "Back" navigation — no new messages.
func (h *WebhookHandler) editStartMenu(chatID int64, msgID int, firstName string, user *storage.User) {
	body := h.buildStartMenuText(firstName, user)
	kb := h.buildStartMenuKB(user.Language)
	if err := h.client.EditMessageText(chatID, msgID, body, kb); err != nil {
		log.Printf("[HANDLER] editStartMenu %d/%d: %v", chatID, msgID, err)
		// Fallback: if the original message cannot be edited (e.g. it was a photo),
		// delete the stale bubble and send a fresh one.
		_ = h.client.DeleteMessage(chatID, msgID)
		_ = h.client.SendMessageWithKeyboard(chatID, body, kb)
	}
}

func (h *WebhookHandler) buildStartMenuText(firstName string, user *storage.User) string {
	plan := "FREE"
	if user.IsVIP {
		plan = "👑 VIP"
	}
	lang := user.Language
	if lang == "ru" {
		return fmt.Sprintf(
			"💎 <b>SMART CLUSTER TERMINAL</b>\n"+
				"<b>━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━</b>\n"+
				"👋 <b>Привет, %s!</b>\n\n"+
				"📊 Аналитика и безопасность смарт-денег в реальном времени.\n\n"+
				"👤 План: <b>%s</b>\n"+
				"🔔 Мин. объём: <b>$%s</b>\n"+
				"🌐 Сети: %s\n"+
				"🌐 Язык: <b>Русский (RU)</b>\n\n"+
				"Выберите действие:",
			html.EscapeString(firstName),
			html.EscapeString(plan),
			fmtVolume(user.MinVolume),
			enabledNetworks(user),
		)
	}
	return fmt.Sprintf(
		"💎 <b>SMART CLUSTER TERMINAL</b>\n"+
			"<b>━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━</b>\n"+
			"👋 <b>Hello, %s!</b>\n\n"+
			"📊 Real-time Smart Money Analytics & Security.\n\n"+
			"👤 Plan: <b>%s</b>\n"+
			"🔔 Min Volume: <b>$%s</b>\n"+
			"🌐 Networks: %s\n"+
			"🌐 Language: <b>English (EN)</b>\n\n"+
			"Choose an action:",
		html.EscapeString(firstName),
		html.EscapeString(plan),
		fmtVolume(user.MinVolume),
		enabledNetworks(user),
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
				{Text: tr(lang, "⭐ My Watchlist", "⭐ Мой Watchlist"), CallbackData: "cb:watchlist"},
				{Text: tr(lang, "⚙️ Settings", "⚙️ Настройки"), CallbackData: "cb:settings"},
			},
			{
				{Text: tr(lang, "🔥 Hot Wallets", "🔥 Горячие кошельки"), CallbackData: "cb:hot"},
				{Text: tr(lang, "❓ Help", "❓ Помощь"), CallbackData: "cb:help"},
			},
			{
				{Text: tr(lang, "👑 VIP Pass", "👑 VIP Пасс"), CallbackData: "cb:vip"},
				{Text: tr(lang, "🤝 Referral", "🤝 Рефералы"), CallbackData: "cb:referral"},
			},
		},
	}
}

// ── /watch command ─────────────────────────────────────────────────────────────

func (h *WebhookHandler) handleWatchCommand(chatID, userID int64, text, lang string) {
	parts := strings.Fields(text)
	if len(parts) < 2 {
		if lang == "ru" {
			h.client.SendMessage(chatID,
				"ℹ️ Использование: <code>/watch <wallet_address> [заметка]</code>\n\n"+
					"Пример:\n<code>/watch 0xABCDEF... кит</code>")
		} else {
			h.client.SendMessage(chatID,
				"ℹ️ Usage: <code>/watch <wallet_address> [optional note]</code>\n\n"+
					"Example:\n<code>/watch 0xABCDEF... big whale</code>")
		}
		return
	}
	addr := parts[1]
	note := strings.Join(parts[2:], " ")

	if err := h.storage.AddWatchlistWallet(userID, addr, note); err != nil {
		log.Printf("[HANDLER] AddWatchlistWallet %d: %v", userID, err)
		msg := "❌ Failed to add wallet. Please try again later."
		if lang == "ru" {
			msg = "❌ Не удалось добавить кошелёк. Попробуйте позже."
		}
		h.client.SendMessage(chatID, msg)
		return
	}

	var reply string
	if lang == "ru" {
		reply = fmt.Sprintf(
			"✅ <b>Кошелёк добавлен в Watchlist!</b>\n\n"+
				"<code>%s</code>\n"+
				"📝 Заметка: %s\n\n"+
				"Вы получите уведомление, как только этот кошелёк купит что-нибудь.",
			html.EscapeString(addr),
			html.EscapeString(or(note, "—")),
		)
	} else {
		reply = fmt.Sprintf(
			"✅ <b>Wallet added to Watchlist!</b>\n\n"+
				"<code>%s</code>\n"+
				"📝 Note: %s\n\n"+
				"You will receive an alert as soon as this wallet makes a purchase.",
			html.EscapeString(addr),
			html.EscapeString(or(note, "—")),
		)
	}
	h.client.SendMessageWithKeyboard(chatID, reply, backToMenuKB(lang))
}

// ── Watchlist (send = new message from /command; edit = in-place from button) ──

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
	if len(entries) == 0 {
		msg := "⭐ <b>My Watchlist</b>\n<b>━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━</b>\n\nYour list is empty.\n\nAdd using: <code>/watch &lt;address&gt; [note]</code>"
		if lang == "ru" {
			msg = "⭐ <b>Мой Watchlist</b>\n<b>━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━</b>\n\nСписок пуст.\n\nДобавьте командой: <code>/watch &lt;address&gt; [заметка]</code>"
		}
		return msg, backToMenuKB(lang)
	}

	var sb strings.Builder
	if lang == "ru" {
		sb.WriteString("⭐ <b>Мой Watchlist</b>\n<b>━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━</b>\n\n")
	} else {
		sb.WriteString("⭐ <b>My Watchlist</b>\n<b>━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━</b>\n\n")
	}

	var rows [][]InlineKeyboardButton
	for _, e := range entries {
		masked := maskAddr(e.WalletAddress)
		sb.WriteString("• <code>" + html.EscapeString(masked) + "</code>")
		if e.Note != "" {
			sb.WriteString(" — " + html.EscapeString(e.Note))
		}
		sb.WriteString("\n")
		delText := "🗑 " + masked
		rows = append(rows, []InlineKeyboardButton{
			{Text: delText, CallbackData: fmt.Sprintf("cb:watchrm:%d", e.ID)},
		})
	}
	backText := tr(lang, "⬅️ Back", "⬅️ Назад")
	rows = append(rows, []InlineKeyboardButton{{Text: backText, CallbackData: "cb:menu"}})
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
	var body string
	if lang == "ru" {
		body = fmt.Sprintf(
			"📈 <b>Статистика за 24 часа</b>\n"+
				"<b>━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━</b>\n\n"+
				"🔢 Кластеров обнаружено: <b>%d</b>\n"+
				"💰 Общий объём: <b>$%s</b>\n"+
				"🏆 Топ токен: <b>%s</b>\n"+
				"🌐 Активная сеть: <b>%s</b>",
			stats.TotalClusters, fmtFloat(stats.TotalVolumeUSD),
			html.EscapeString(or(stats.TopToken, "—")),
			html.EscapeString(or(stats.TopChain, "—")),
		)
	} else {
		body = fmt.Sprintf(
			"📈 <b>24h Statistics</b>\n"+
				"<b>━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━</b>\n\n"+
				"🔢 Clusters Detected: <b>%d</b>\n"+
				"💰 Total Volume: <b>$%s</b>\n"+
				"🏆 Top Token: <b>%s</b>\n"+
				"🌐 Top Chain: <b>%s</b>",
			stats.TotalClusters, fmtFloat(stats.TotalVolumeUSD),
			html.EscapeString(or(stats.TopToken, "—")),
			html.EscapeString(or(stats.TopChain, "—")),
		)
	}
	return body, backToMenuKB(lang)
}

// ── Hot wallets ─────────────────────────────────────────────────────────────────

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
	if len(wallets) == 0 {
		msg := "🔥 <b>Hot Wallets</b>\n<b>━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━</b>\n\nNo data available yet."
		if lang == "ru" {
			msg = "🔥 <b>Горячие кошельки</b>\n<b>━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━</b>\n\nДанных пока нет."
		}
		return msg, backToMenuKB(lang)
	}
	var sb strings.Builder
	if lang == "ru" {
		sb.WriteString("🔥 <b>Горячие кошельки — 24h</b>\n<b>━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━</b>\n<i>Кошельки, появляющиеся в наибольшем числе кластеров</i>\n\n")
	} else {
		sb.WriteString("🔥 <b>Hot Wallets — 24h</b>\n<b>━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━</b>\n<i>Wallets appearing in the most clusters</i>\n\n")
	}
	medals := []string{"🥇", "🥈", "🥉", "4️⃣", "5️⃣"}
	for i, w := range wallets {
		medal := "•"
		if i < len(medals) {
			medal = medals[i]
		}
		sb.WriteString(fmt.Sprintf(
			"%s <code>%s</code>\n   %d clusters · $%s\n\n",
			medal,
			html.EscapeString(maskAddr(w.WalletAddress)),
			w.ClusterCount,
			fmtFloat(w.TotalVolumeUSD),
		))
	}
	if lang == "ru" {
		sb.WriteString("<i>Повторные покупки = наиболее убеждённые умные деньги</i>")
	} else {
		sb.WriteString("<i>Repeated buys = highest conviction smart money</i>")
	}
	return sb.String(), backToMenuKB(lang)
}

// ── Recent Clusters ────────────────────────────────────────────────────────────

func (h *WebhookHandler) editRecentClusters(chatID int64, msgID int, lang string) {
	clusters, err := h.storage.GetRecentClusters(5)
	if err != nil || len(clusters) == 0 {
		msg := "🔥 <b>Fresh Clusters</b>\n<b>━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━</b>\n\nNo data available yet."
		if lang == "ru" {
			msg = "🔥 <b>Свежие кластеры</b>\n<b>━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━</b>\n\nДанных пока нет."
		}
		if err := h.client.EditMessageText(chatID, msgID, msg, backToMenuKB(lang)); err != nil {
			log.Printf("[HANDLER] editRecentClusters empty %d/%d: %v", chatID, msgID, err)
		}
		return
	}
	var sb strings.Builder
	if lang == "ru" {
		sb.WriteString("🔥 <b>Последние кластеры</b>\n<b>━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━</b>\n\n")
	} else {
		sb.WriteString("🔥 <b>Recent Clusters</b>\n<b>━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━</b>\n\n")
	}
	for _, c := range clusters {
		sb.WriteString(fmt.Sprintf(
			"• <b>%s</b> (%s)\n  💰 $%s · %d buys\n  <code>%s</code>\n\n",
			html.EscapeString(c.TokenSymbol), html.EscapeString(c.Chain),
			fmtFloat(c.TotalVolumeUSD), c.BuyCount,
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

	// Always answer immediately to stop the spinner, log failures.
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
	currentLang := user.Language

	switch {
	case data == "accept_tos":
		_ = h.storage.SetUserTosAccepted(userID, true)
		user.TosAccepted = true
		// ToS message has no keyboard to morph — delete it, send fresh start menu.
		_ = h.client.DeleteMessage(chatID, msgID)
		h.sendStartMenu(chatID, cb.From.FirstName, user)

	case data == "cb:menu":
		h.editStartMenu(chatID, msgID, cb.From.FirstName, user)

	case data == "cb:clusters":
		h.editRecentClusters(chatID, msgID, currentLang)

	case data == "cb:stats":
		h.editStats24h(chatID, msgID, currentLang)

	case data == "cb:watchlist":
		h.editWatchlistMenu(chatID, msgID, userID, currentLang)

	case data == "cb:hot":
		h.editHotWallets(chatID, msgID, currentLang)

	case data == "cb:settings":
		h.editSettingsMenu(chatID, msgID, userID, user)

	// ── VIP & Help — these call EditMessageText directly (not smartEdit).
	// They work correctly as long as the source message is a TEXT message,
	// which it now always is since sendStartMenu no longer uses SendPhoto.
	case data == "cb:vip":
		h.editVIPInfo(chatID, msgID, currentLang)

	case data == "cb:help":
		h.editHelp(chatID, msgID, currentLang)

	case data == "cb:referral":
		h.editReferralMenu(chatID, msgID, userID, currentLang)

	case data == "pay:stars":
		h.editPaymentStars(chatID, msgID, currentLang)

	case data == "pay:cryptobot":
		h.editPaymentCryptoBot(chatID, msgID, currentLang)

	case data == "pay:wallet":
		h.editPaymentDirectWallet(chatID, msgID, currentLang)

	case strings.HasPrefix(data, "pay:done:"):
		h.handleDirectPaymentSubmitted(chatID, msgID, userID, data, currentLang)

	case data == "cb:lang":
		newLang := "ru"
		if user.Language == "ru" {
			newLang = "en"
		}
		_ = h.storage.SetUserLanguage(userID, newLang)
		user.Language = newLang
		h.editSettingsMenu(chatID, msgID, userID, user)

	case strings.HasPrefix(data, "cb:vol:"):
		h.handleVolumeChange(chatID, msgID, userID, cb.From.Username, currentLang, data)

	case strings.HasPrefix(data, "cb:net:"):
		h.handleNetworkToggle(chatID, msgID, userID, cb.From.Username, currentLang, data)

	case strings.HasPrefix(data, "cb:watchrm:"):
		h.handleWatchlistRemove(chatID, msgID, userID, data, currentLang)
	}
}

// ── Settings menu ──────────────────────────────────────────────────────────────

func (h *WebhookHandler) editSettingsMenu(chatID int64, msgID int, userID int64, user *storage.User) {
	lang := user.Language
	var body string
	if lang == "ru" {
		body = fmt.Sprintf(
			"⚙️ <b>Настройки алертов</b>\n"+
				"<b>━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━</b>\n\n"+
				"Текущий мин. объём: <b>$%s</b>\n"+
				"Активные сети: %s\n"+
				"Язык: <b>Русский (RU)</b>\n\n"+
				"Изменения сохраняются мгновенно.",
			fmtVolume(user.MinVolume), enabledNetworks(user),
		)
	} else {
		body = fmt.Sprintf(
			"⚙️ <b>Alert Settings</b>\n"+
				"<b>━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━</b>\n\n"+
				"Current Min Volume: <b>$%s</b>\n"+
				"Active Networks: %s\n"+
				"Language: <b>English (EN)</b>\n\n"+
				"Changes are saved instantly.",
			fmtVolume(user.MinVolume), enabledNetworks(user),
		)
	}

	checkETH := emojiCheck(user.EthEnabled)
	checkSOL := emojiCheck(user.SolEnabled)
	checkBASE := emojiCheck(user.BaseEnabled)
	checkBSC := emojiCheck(user.BscEnabled)

	langToggleText := "🌐 Language: English (EN)"
	if lang == "ru" {
		langToggleText = "🌐 Язык / Language: Русский (RU)"
	}
	backText := tr(lang, "⬅️ Back", "⬅️ Назад")

	kb := &InlineKeyboardMarkup{
		InlineKeyboard: [][]InlineKeyboardButton{
			{
				{Text: "$10k", CallbackData: fmt.Sprintf("cb:vol:%d:10000", userID)},
				{Text: "$25k", CallbackData: fmt.Sprintf("cb:vol:%d:25000", userID)},
				{Text: "$50k", CallbackData: fmt.Sprintf("cb:vol:%d:50000", userID)},
				{Text: "$100k", CallbackData: fmt.Sprintf("cb:vol:%d:100000", userID)},
			},
			{
				{Text: checkETH + " ETH", CallbackData: fmt.Sprintf("cb:net:%d:eth", userID)},
				{Text: checkSOL + " SOL", CallbackData: fmt.Sprintf("cb:net:%d:sol", userID)},
				{Text: checkBASE + " BASE", CallbackData: fmt.Sprintf("cb:net:%d:base", userID)},
				{Text: checkBSC + " BSC", CallbackData: fmt.Sprintf("cb:net:%d:bsc", userID)},
			},
			{{Text: langToggleText, CallbackData: "cb:lang"}},
			{{Text: backText, CallbackData: "cb:menu"}},
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
	net := parts[3]
	user, err := h.storage.GetOrCreateUser(userID, username, lang)
	if err != nil {
		return
	}
	switch net {
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

// ── VIP Info & Payment flows ───────────────────────────────────────────────────

// editVIPInfo shows the redesigned VIP page with three payment method buttons.
// This is reached via cb:vip callback (edit-in-place).
func (h *WebhookHandler) editVIPInfo(chatID int64, msgID int, lang string) {
	body := h.buildVIPInfoText(lang)
	kb := h.buildVIPInfoKB(lang)
	if err := h.client.EditMessageText(chatID, msgID, body, kb); err != nil {
		log.Printf("[HANDLER] editVIPInfo %d/%d: %v", chatID, msgID, err)
	}
}

// sendVIPMenu is used for the /vip text command — sends a new message.
func (h *WebhookHandler) sendVIPMenu(chatID int64, lang string) {
	body := h.buildVIPInfoText(lang)
	kb := h.buildVIPInfoKB(lang)
	if err := h.client.SendMessageWithKeyboard(chatID, body, kb); err != nil {
		log.Printf("[HANDLER] sendVIPMenu %d: %v", chatID, err)
	}
}

func (h *WebhookHandler) buildVIPInfoText(lang string) string {
	if lang == "ru" {
		return "👑 <b>VIP Пасс — Smart Cluster Terminal</b>\n" +
			"<b>━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━</b>\n\n" +
			"<b>Что входит в VIP:</b>\n" +
			"🔓 100% адресов всех кошельков без маскировки\n" +
			"⚡ Мгновенные Telegram алерты без задержек\n" +
			"🎯 Кастомные фильтры по токену и объёму\n" +
			"📈 Полный архив кластеров + экспорт CSV\n" +
			"🔥 Персональный топ горячих кошельков\n\n" +
			"💳 <b>Стоимость: $9.99 / 30 дней</b>\n" +
			"Выберите удобный способ оплаты:"
	}
	return "👑 <b>VIP Pass — Smart Cluster Terminal</b>\n" +
		"<b>━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━</b>\n\n" +
		"<b>What's included:</b>\n" +
		"🔓 100% unmasked wallet addresses\n" +
		"⚡ Instant Telegram alerts with zero delay\n" +
		"🎯 Custom token & volume filters\n" +
		"📈 Full cluster history + CSV export\n" +
		"🔥 Personalized hot wallet rankings\n\n" +
		"💳 <b>Price: $9.99 / 30 days</b>\n" +
		"Choose your payment method:"
}

func (h *WebhookHandler) buildVIPInfoKB(lang string) *InlineKeyboardMarkup {
	backText := tr(lang, "⬅️ Back", "⬅️ Назад")
	return &InlineKeyboardMarkup{
		InlineKeyboard: [][]InlineKeyboardButton{
			{{Text: "⭐ Telegram Stars (XTR)", CallbackData: "pay:stars"}},
			{{Text: "🤖 CryptoBot — USDT / TON", CallbackData: "pay:cryptobot"}},
			{{Text: "💎 Direct Crypto (TRC20 / SOL)", CallbackData: "pay:wallet"}},
			{{Text: backText, CallbackData: "cb:menu"}},
		},
	}
}

// ── Payment: Telegram Stars ────────────────────────────────────────────────────

func (h *WebhookHandler) editPaymentStars(chatID int64, msgID int, lang string) {
	var body string
	if lang == "ru" {
		body = "⭐ <b>Оплата через Telegram Stars (XTR)</b>\n" +
			"<b>━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━</b>\n\n" +
			"Получите 30 дней VIP-доступа мгновенно через официальную валюту Telegram Stars.\n\n" +
			"Стоимость: <b>250 XTR</b>\n\n" +
			"После оплаты Stars VIP активируется автоматически в течение нескольких минут."
	} else {
		body = "⭐ <b>Telegram Stars Payment (XTR)</b>\n" +
			"<b>━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━</b>\n\n" +
			"Get 30 days VIP access instantly via official Telegram Stars.\n\n" +
			"Price: <b>250 XTR</b>\n\n" +
			"VIP activates automatically within minutes after Stars payment."
	}

	payBtn := tr(lang, "⭐ Pay 250 Stars", "⭐ Оплатить 250 Stars")
	backText := tr(lang, "⬅️ Back", "⬅️ Назад")

	kb := &InlineKeyboardMarkup{
		InlineKeyboard: [][]InlineKeyboardButton{
			{{Text: payBtn, URL: "https://t.me/StarkWonder?start=stars_vip"}},
			{{Text: backText, CallbackData: "cb:vip"}},
		},
	}
	if err := h.client.EditMessageText(chatID, msgID, body, kb); err != nil {
		log.Printf("[HANDLER] editPaymentStars %d/%d: %v", chatID, msgID, err)
	}
}

// ── Payment: CryptoBot (real invoice) ─────────────────────────────────────────

func (h *WebhookHandler) editPaymentCryptoBot(chatID int64, msgID int, lang string) {
	// Show a loading state first so the user sees immediate feedback.
	loadingMsg := tr(lang, "⏳ Creating invoice...", "⏳ Создаём счёт...")
	_ = h.client.EditMessageText(chatID, msgID, loadingMsg, nil)

	invoiceURL, err := h.createCryptoBotInvoice(chatID)
	if err != nil {
		log.Printf("[HANDLER] createCryptoBotInvoice %d: %v", chatID, err)
		errBody := "❌ Payment service temporarily unavailable. Please try again or contact @StarkWonder."
		if lang == "ru" {
			errBody = "❌ Платёжный сервис временно недоступен. Попробуйте позже или свяжитесь с @StarkWonder."
		}
		backText := tr(lang, "⬅️ Back", "⬅️ Назад")
		kb := &InlineKeyboardMarkup{
			InlineKeyboard: [][]InlineKeyboardButton{
				{{Text: backText, CallbackData: "cb:vip"}},
			},
		}
		_ = h.client.EditMessageText(chatID, msgID, errBody, kb)
		return
	}

	var body string
	if lang == "ru" {
		body = "🤖 <b>Оплата через CryptoBot (@CryptoBot)</b>\n" +
			"<b>━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━</b>\n\n" +
			"Быстрая оплата в USDT или TON через официального бота @CryptoBot.\n\n" +
			"Стоимость: <b>$9.99 USDT / TON</b>\n\n" +
			"После оплаты VIP активируется автоматически."
	} else {
		body = "🤖 <b>CryptoBot Payment (@CryptoBot)</b>\n" +
			"<b>━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━</b>\n\n" +
			"Fast payment in USDT or TON via official @CryptoBot.\n\n" +
			"Price: <b>$9.99 USDT / TON</b>\n\n" +
			"VIP activates automatically after payment."
	}

	payBtn := tr(lang, "💳 Pay Invoice", "💳 Оплатить счёт")
	backText := tr(lang, "⬅️ Back", "⬅️ Назад")

	kb := &InlineKeyboardMarkup{
		InlineKeyboard: [][]InlineKeyboardButton{
			{{Text: payBtn, URL: invoiceURL}},
			{{Text: backText, CallbackData: "cb:vip"}},
		},
	}
	if err := h.client.EditMessageText(chatID, msgID, body, kb); err != nil {
		log.Printf("[HANDLER] editPaymentCryptoBot final %d/%d: %v", chatID, msgID, err)
	}
}

// createCryptoBotInvoice calls the Crypto Pay API and returns a real pay_url.
func (h *WebhookHandler) createCryptoBotInvoice(userID int64) (string, error) {
	token := ""
	if h.config != nil {
		token = h.config.CryptoBotToken
	}
	if token == "" {
		token = os.Getenv("CRYPTOBOT_TOKEN")
	}
	if token == "" {
		return "", fmt.Errorf("CRYPTOBOT_TOKEN not configured")
	}

	payload := map[string]interface{}{
		"asset":       "USDT",
		"amount":      "9.99",
		"description": "Smart Cluster Terminal — VIP Pass (30 days)",
		"payload":     fmt.Sprintf("vip_%d_%d", userID, time.Now().Unix()),
	}
	jsonBytes, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, "https://pay.crypt.bot/api/createInvoice", bytes.NewBuffer(jsonBytes))
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
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
			PayURL string `json:"pay_url"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode: %w", err)
	}
	if !result.Ok {
		return "", fmt.Errorf("CryptoBot API: %s", result.Error)
	}
	if result.Result.PayURL == "" {
		return "", fmt.Errorf("CryptoBot returned empty pay_url")
	}
	return result.Result.PayURL, nil
}

// ── Payment: Direct Wallet ─────────────────────────────────────────────────────

func (h *WebhookHandler) editPaymentDirectWallet(chatID int64, msgID int, lang string) {
	var body string
	if lang == "ru" {
		body = "💎 <b>Прямой перевод крипто (TRC20 / SOL)</b>\n" +
			"<b>━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━</b>\n\n" +
			"Отправьте ровно <b>$10 USDT</b> (TRC20) или <b>0.05 SOL</b>:\n\n" +
			"📌 <b>USDT (TRC20):</b>\n<code>T9yD14Nj9j7xAB4dbGeiX9h8unkKHxuWwb</code>\n\n" +
			"📌 <b>Solana (SOL):</b>\n<code>7xKXtg2CW87d97TXJSDpbD5jBkheTqA83TZRuJosgAsU</code>\n\n" +
			"После отправки нажмите кнопку ниже или отправьте TxID администратору @StarkWonder."
	} else {
		body = "💎 <b>Direct Crypto Wallet (TRC20 / SOL)</b>\n" +
			"<b>━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━</b>\n\n" +
			"Transfer exactly <b>$10 USDT</b> (TRC20) or <b>0.05 SOL</b>:\n\n" +
			"📌 <b>USDT (TRC20):</b>\n<code>T9yD14Nj9j7xAB4dbGeiX9h8unkKHxuWwb</code>\n\n" +
			"📌 <b>Solana (SOL):</b>\n<code>7xKXtg2CW87d97TXJSDpbD5jBkheTqA83TZRuJosgAsU</code>\n\n" +
			"After transferring, tap below or message @StarkWonder with your TxID."
	}

	submitBtn := tr(lang, "✅ I Have Paid (Submit TxID)", "✅ Я оплатил (Отправить TxID)")
	backText := tr(lang, "⬅️ Back", "⬅️ Назад")

	kb := &InlineKeyboardMarkup{
		InlineKeyboard: [][]InlineKeyboardButton{
			{{Text: submitBtn, CallbackData: "pay:done:prompt"}},
			{{Text: backText, CallbackData: "cb:vip"}},
		},
	}
	if err := h.client.EditMessageText(chatID, msgID, body, kb); err != nil {
		log.Printf("[HANDLER] editPaymentDirectWallet %d/%d: %v", chatID, msgID, err)
	}
}

func (h *WebhookHandler) handleDirectPaymentSubmitted(chatID int64, msgID int, userID int64, data, lang string) {
	_ = h.storage.AddPendingPayment(userID, "direct_crypto", "PENDING_TX_VERIFY")
	var body string
	if lang == "ru" {
		body = "✅ <b>Платёж зафиксирован на проверку!</b>\n\n" +
			"Ваш запрос на активацию VIP отправлен администратору.\n" +
			"Проверка обычно занимает до 15 минут.\n" +
			"По всем вопросам: @StarkWonder"
	} else {
		body = "✅ <b>Payment submitted for verification!</b>\n\n" +
			"Your VIP activation request has been logged for admin review.\n" +
			"Usually processed within 15 minutes.\n" +
			"Need help? Contact @StarkWonder"
	}
	if err := h.client.EditMessageText(chatID, msgID, body, backToMenuKB(lang)); err != nil {
		log.Printf("[HANDLER] handleDirectPaymentSubmitted %d/%d: %v", chatID, msgID, err)
	}
}

// ── Help ───────────────────────────────────────────────────────────────────────

func (h *WebhookHandler) editHelp(chatID int64, msgID int, lang string) {
	var body string
	if lang == "ru" {
		body = "❓ <b>Помощь — команды бота</b>\n" +
			"<b>━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━</b>\n\n" +
			"<code>/start</code> — главное меню\n" +
			"<code>/watch &lt;addr&gt; [заметка]</code> — добавить кошелёк в watchlist\n" +
			"<code>/watchlist</code> — показать watchlist\n" +
			"<code>/stats</code> — статистика за 24 часа\n" +
			"<code>/hot</code> — топ горячих кошельков\n" +
			"<code>/ref</code> — реферальная программа\n\n" +
			"<b>Как работают кластеры:</b>\n" +
			"Система отслеживает покупки на DEX и сигнализирует, " +
			"когда ≥3 умных кошелька аккумулируют один токен в течение 5 минут."
	} else {
		body = "❓ <b>Help — Bot Commands</b>\n" +
			"<b>━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━</b>\n\n" +
			"<code>/start</code> — main menu\n" +
			"<code>/watch &lt;addr&gt; [note]</code> — add wallet to watchlist\n" +
			"<code>/watchlist</code> — view watchlist\n" +
			"<code>/stats</code> — 24h statistics\n" +
			"<code>/hot</code> — top hot wallets\n" +
			"<code>/ref</code> — referral programme\n\n" +
			"<b>How clusters work:</b>\n" +
			"The system tracks DEX swaps and signals when ≥3 smart wallets " +
			"accumulate the same token within 5 minutes."
	}

	if err := h.client.EditMessageText(chatID, msgID, body, backToMenuKB(lang)); err != nil {
		log.Printf("[HANDLER] editHelp %d/%d: %v", chatID, msgID, err)
	}
}

// ── Referral system ────────────────────────────────────────────────────────────
//
// Revenue model for free users:
//   - Every user gets a unique referral link.
//   - When a referred user buys VIP (any payment method), the referrer earns
//     a commission credit stored in the DB.
//   - Free users can also earn small credits by simply sharing clusters they
//     spotted — a "signal share" mechanic (future: tied to verified accuracy).
//
// This gives non-VIP users a meaningful earning path while growing your user
// base virally. You earn on every VIP sale driven by referrals (net positive).

func (h *WebhookHandler) sendReferralMenu(chatID, userID int64, lang string) {
	text, kb := h.buildReferralContent(userID, lang)
	if err := h.client.SendMessageWithKeyboard(chatID, text, kb); err != nil {
		log.Printf("[HANDLER] sendReferralMenu %d: %v", chatID, err)
	}
}

func (h *WebhookHandler) editReferralMenu(chatID int64, msgID int, userID int64, lang string) {
	text, kb := h.buildReferralContent(userID, lang)
	if err := h.client.EditMessageText(chatID, msgID, text, kb); err != nil {
		log.Printf("[HANDLER] editReferralMenu %d/%d: %v", chatID, msgID, err)
	}
}

func (h *WebhookHandler) buildReferralContent(userID int64, lang string) (string, *InlineKeyboardMarkup) {
	// Fetch referral stats from storage.
	// Falls back gracefully if AddReferral / GetReferralStats are not yet implemented.
	referralCount := 0
	earningsUSD := 0.0
	if stats, err := h.storage.GetReferralStats(userID); err == nil && stats != nil {
		referralCount = stats.TotalReferrals
		earningsUSD = stats.TotalEarningsUSD
	}

	botUsername := "SmartClusterBot" // replace with your actual bot @username
	if h.config != nil && h.config.BotUsername != "" {
		botUsername = h.config.BotUsername
	}
	refLink := fmt.Sprintf("https://t.me/%s?start=ref_%d", botUsername, userID)

	var body string
	if lang == "ru" {
		body = fmt.Sprintf(
			"🤝 <b>Реферальная программа</b>\n"+
				"<b>━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━</b>\n\n"+
				"Приглашайте друзей и зарабатывайте <b>20%% комиссии</b> с каждой их VIP-покупки.\n\n"+
				"👥 Приглашено: <b>%d</b>\n"+
				"💰 Заработано: <b>$%.2f</b>\n\n"+
				"🔗 Ваша реферальная ссылка:\n"+
				"<code>%s</code>\n\n"+
				"<i>Скопируйте ссылку и отправьте друзьям. Когда они купят VIP — вы получите $2 на баланс.</i>",
			referralCount, earningsUSD, refLink,
		)
	} else {
		body = fmt.Sprintf(
			"🤝 <b>Referral Programme</b>\n"+
				"<b>━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━</b>\n\n"+
				"Invite friends and earn <b>20%% commission</b> on every VIP purchase they make.\n\n"+
				"👥 Referred: <b>%d</b>\n"+
				"💰 Earned: <b>$%.2f</b>\n\n"+
				"🔗 Your referral link:\n"+
				"<code>%s</code>\n\n"+
				"<i>Copy the link and share it. When a friend buys VIP, you earn $2 in credit.</i>",
			referralCount, earningsUSD, refLink,
		)
	}

	backText := tr(lang, "⬅️ Back", "⬅️ Назад")
	kb := &InlineKeyboardMarkup{
		InlineKeyboard: [][]InlineKeyboardButton{
			{{Text: backText, CallbackData: "cb:menu"}},
		},
	}
	return body, kb
}

// ── Manual / Guide ─────────────────────────────────────────────────────────────

func (h *WebhookHandler) sendManual(chatID int64, lang string) {
	var body string
	if lang == "ru" {
		body = "📖 <b>Руководство пользователя — Smart Cluster Terminal</b>\n" +
			"<b>━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━</b>\n\n" +
			"<b>1. Что такое Кластер?</b>\n" +
			"Кластер — момент, когда несколько независимых Smart Money кошельков начинают аккумулировать один токен в течение 5 минут.\n\n" +
			"<b>2. Золотое правило — DYOR & RugCheck:</b>\n" +
			"🛡 <b>ВСЕГДА проверяйте токены через RugCheck</b> перед покупкой! До 90% новых токенов — скам.\n\n" +
			"<b>3. Как использовать:</b>\n" +
			"• 📊 Открывайте WebApp-терминал для анализа в реальном времени.\n" +
			"• 🔔 Настраивайте объём и сети в Настройках.\n" +
			"• ⭐ Добавляйте кошельки в Watchlist: <code>/watch &lt;addr&gt; [заметка]</code>\n" +
			"• 🤝 Зарабатывайте через реферальную программу: <code>/ref</code>\n\n" +
			"⚠️ <b>Важно:</b> Это аналитический инструмент, а не «машина для денег». Делайте собственный анализ (DYOR)!"
	} else {
		body = "📖 <b>User Guide — Smart Cluster Terminal</b>\n" +
			"<b>━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━</b>\n\n" +
			"<b>1. What is a Cluster?</b>\n" +
			"A cluster fires when ≥3 independent Smart Money wallets accumulate the same token within a 5-minute window.\n\n" +
			"<b>2. The Golden Rule — DYOR & RugCheck:</b>\n" +
			"🛡 <b>ALWAYS check tokens via RugCheck</b> before acting! Up to 90% of new tokens are scams.\n\n" +
			"<b>3. How to use:</b>\n" +
			"• 📊 Open the WebApp terminal for real-time interactive analysis.\n" +
			"• 🔔 Configure min volume and networks in Settings.\n" +
			"• ⭐ Track wallets: <code>/watch &lt;addr&gt; [note]</code>\n" +
			"• 🤝 Earn money via referrals: <code>/ref</code>\n\n" +
			"⚠️ <b>Important:</b> This is an analytical scanner, not a magic money printer. Do Your Own Research (DYOR)!"
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

func enabledNetworks(u *storage.User) string {
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
		return "<i>none</i>"
	}
	return strings.Join(nets, " · ")
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

func fmtVolume(v int) string {
	return fmtFloat(float64(v))
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
