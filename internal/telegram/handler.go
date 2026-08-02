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
	// Use user.Language for the rest of the function.
	if err != nil {
		log.Printf("[HANDLER] GetOrCreateUser %d: %v", from.ID, err)
		return
	}

	text := strings.TrimSpace(msg.Text)

	// Gatekeeper: if TOS not accepted, block all access and send strict disclaimer
	if !user.TosAccepted {
		if text == "accept_tos" || strings.HasPrefix(text, "accept_tos") || text == "/start" {
			// handled below via callback or explicit flow, but for normal text messages when not accepted:
			h.sendTosDisclaimer(chatID, user.Language)
			return
		}
		h.sendTosDisclaimer(chatID, user.Language)
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

	default:
		h.sendStartMenu(chatID, from.FirstName, user)
	}
}

// ── ToS Disclaimer Gatekeeper ───────────────────────────────────────────────────

func (h *WebhookHandler) sendTosDisclaimer(chatID int64, lang string) {
	msg := "⚠️ <b>Внимание!</b> Smart Cluster Terminal — это исключительно аналитический инструмент для отслеживания публичных транзакций в блокчейне. Бот НЕ дает финансовых советов (Not Financial Advice) и НЕ является призывом к инвестициям. Рынок криптовалют несет сверхвысокие риски полной потери средств. Создатели бота не несут никакой ответственности за ваши торговые решения и финансовые потери. Вы используете сервис на свой страх и риск. Подтверждая, вы соглашаетесь с условиями и подтверждаете, что вам есть 18 лет."

	kb := &InlineKeyboardMarkup{
		InlineKeyboard: [][]InlineKeyboardButton{
			{
				{Text: "✅ Я прочитал, принимаю риски и мне есть 18 лет", CallbackData: "accept_tos"},
			},
		},
	}

	if err := h.client.SendMessageWithKeyboard(chatID, msg, kb); err != nil {
		log.Printf("[HANDLER] sendTosDisclaimer %d: %v", chatID, err)
	}
}

// ── /start menu ────────────────────────────────────────────────────────────────

func (h *WebhookHandler) sendStartMenu(chatID int64, firstName string, user *storage.User) {
	plan := "FREE"
	if user.IsVIP {
		plan = "👑 VIP"
	}

	lang := user.Language
	webAppURL := h.webAppURL()

	var body string
	if lang == "ru" {
		body = fmt.Sprintf(
			"<pre>�� SMART CLUSTER TERMINAL</pre>\n"+
				" <b>Привет, %s!</b>\n"+
				"<b>━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━</b>\n"+
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
	} else {
		body = fmt.Sprintf(
			"<pre>💎 SMART CLUSTER TERMINAL</pre>\n"+
				"👋 <b>Hello, %s!</b>\n"+
				"<b>━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━</b>\n"+
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

	kb := &InlineKeyboardMarkup{
		InlineKeyboard: [][]InlineKeyboardButton{
			{
				{Text: tr(lang, "📊 Open Terminal", "📊 Открыть Terminal"), WebApp: &WebAppInfo{URL: webAppURL}},
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
			},
		},
	}

	// Use SendPhoto with a professional crypto banner placeholder URL to force wide bubble rendering, or fallback if photo fails
	heroBannerURL := "https://images.unsplash.com/photo-1639762681485-074b7f938ba0?w=800&q=80"
	if err := h.client.SendPhoto(chatID, heroBannerURL, body, kb); err != nil {
		log.Printf("[HANDLER] sendStartMenu SendPhoto fallback: %v", err)
		if err := h.client.SendMessageWithKeyboard(chatID, body, kb); err != nil {
			log.Printf("[HANDLER] sendStartMenu %d: %v", chatID, err)
		}
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

// ── Watchlist menu ─────────────────────────────────────────────────────────────

func (h *WebhookHandler) sendWatchlistMenu(chatID, userID int64, lang string) {
	entries, err := h.storage.GetWatchlist(userID)
	if err != nil {
		log.Printf("[HANDLER] GetWatchlist %d: %v", userID, err)
		h.client.SendMessage(chatID, "❌ Error loading watchlist.")
		return
	}

	if len(entries) == 0 {
		msg := "⭐ <b>My Watchlist</b>\n<b>━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━</b>\n\nYour list is empty.\n\nAdd a wallet using:\n<code>/watch <address> [note]</code>"
		if lang == "ru" {
			msg = "⭐ <b>Мой Watchlist</b>\n<b>━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━</b>\n\nСписок пуст.\n\nДобавьте кошелёк командой:\n<code>/watch <address> [заметка]</code>"
		}
		h.client.SendMessageWithKeyboard(chatID, msg, backToMenuKB(lang))
		return
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
		sb.WriteString("• <code>")
		sb.WriteString(html.EscapeString(masked))
		sb.WriteString("</code>")
		if e.Note != "" {
			sb.WriteString(" — ")
			sb.WriteString(html.EscapeString(e.Note))
		}
		sb.WriteString("\n")

		delText := "🗑 Remove " + masked
		if lang == "ru" {
			delText = "🗑 Удалить " + masked
		}
		rows = append(rows, []InlineKeyboardButton{
			{
				Text:         delText,
				CallbackData: fmt.Sprintf("cb:watchrm:%d", e.ID),
			},
		})
	}
	if lang == "ru" {
		sb.WriteString("\nНажмите кнопку чтобы удалить запись.")
	} else {
		sb.WriteString("\nTap a button to remove an entry.")
	}

	backText := "⬅️ Back"
	if lang == "ru" {
		backText = "⬅️ Назад"
	}
	rows = append(rows, []InlineKeyboardButton{
		{Text: backText, CallbackData: "cb:menu"},
	})
	h.client.SendMessageWithKeyboard(chatID, sb.String(), &InlineKeyboardMarkup{InlineKeyboard: rows})
}

// ── Stats 24h ──────────────────────────────────────────────────────────────────

func (h *WebhookHandler) sendStats24h(chatID int64, lang string) {
	stats, err := h.storage.GetStats24h()
	if err != nil {
		log.Printf("[HANDLER] GetStats24h: %v", err)
		h.client.SendMessage(chatID, "❌ Error loading statistics.")
		return
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
			stats.TotalClusters,
			fmtFloat(stats.TotalVolumeUSD),
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
			stats.TotalClusters,
			fmtFloat(stats.TotalVolumeUSD),
			html.EscapeString(or(stats.TopToken, "—")),
			html.EscapeString(or(stats.TopChain, "—")),
		)
	}
	h.client.SendMessageWithKeyboard(chatID, body, backToMenuKB(lang))
}

// ── Hot wallets ─────────────────────────────────────────────────────────────────

func (h *WebhookHandler) sendHotWallets(chatID int64, lang string) {
	wallets, err := h.storage.GetTopWallets(24, 5)
	if err != nil {
		log.Printf("[HANDLER] GetTopWallets: %v", err)
		h.client.SendMessage(chatID, "❌ Error loading hot wallets.")
		return
	}

	if len(wallets) == 0 {
		msg := "🔥 <b>Hot Wallets</b>\n\nNo data available yet."
		if lang == "ru" {
			msg = "🔥 <b>Горячие кошельки</b>\n\nДанных пока нет."
		}
		h.client.SendMessageWithKeyboard(chatID, msg, backToMenuKB(lang))
		return
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

	h.client.SendMessageWithKeyboard(chatID, sb.String(), backToMenuKB(lang))
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

	_ = h.client.AnswerCallbackQuery(cb.ID, "")

	// Fetch user to know their language preference
	bootstrapLang := "en"
	if strings.HasPrefix(strings.ToLower(cb.From.LanguageCode), "ru") {
		bootstrapLang = "ru"
	}
	user, err := h.storage.GetOrCreateUser(userID, cb.From.Username, bootstrapLang)
	// Use user.Language for the rest of the function.
	if err != nil {
		log.Printf("[HANDLER] GetOrCreateUser callback %d: %v", userID, err)
		return
	}
	currentLang := user.Language

	switch {
	case data == "accept_tos":
		_ = h.storage.SetUserTosAccepted(userID, true)
		user.TosAccepted = true
		h.editStartMenu(chatID, msgID, cb.From.FirstName, user)

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

	case data == "cb:vip":
		h.editVIPInfo(chatID, msgID, currentLang)

	case data == "cb:help":
		h.editHelp(chatID, msgID, currentLang)

	case data == "pay:stars":
		h.editPaymentStars(chatID, msgID, currentLang)

	case data == "pay:cryptobot":
		h.editPaymentCryptoBot(chatID, msgID, currentLang)

	case data == "pay:wallet":
		h.editPaymentDirectWallet(chatID, msgID, currentLang)

	case strings.HasPrefix(data, "pay:done:"):
		h.handleDirectPaymentSubmitted(chatID, msgID, userID, data, currentLang)

	case data == "cb:lang":
		// Toggle language between en and ru
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

// ── Edit-in-place helpers ──────────────────────────────────────────────────────

func (h *WebhookHandler) editStartMenu(chatID int64, msgID int, firstName string, user *storage.User) {
	h.sendStartMenu(chatID, firstName, user)
}

func wideText(body string) string {
	const spacer = "<pre>                                                            </pre>\n"
	return spacer + body
}

func (h *WebhookHandler) editRecentClusters(chatID int64, msgID int, lang string) {
	clusters, err := h.storage.GetRecentClusters(5)
	if err != nil || len(clusters) == 0 {
		msg := "🔥 <b>Fresh Clusters</b>\n\nNo data available yet."
		if lang == "ru" {
			msg = "🔥 <b>Свежие кластеры</b>\n\nДанных пока нет."
		}
		h.client.EditMessageText(chatID, msgID, msg, backToMenuKB(lang))
		return
	}
	var sb strings.Builder
	if lang == "ru" {
		sb.WriteString("🔥 <b>Последние кластеры</b>\n\n")
	} else {
		sb.WriteString("🔥 <b>Recent Clusters</b>\n\n")
	}
	for _, c := range clusters {
		sb.WriteString(fmt.Sprintf(
			"• <b>%s</b> (%s)\n  💰 $%s · %d buys\n  <code>%s</code>\n\n",
			html.EscapeString(c.TokenSymbol), html.EscapeString(c.Chain),
			fmtFloat(c.TotalVolumeUSD), c.BuyCount,
			c.TokenAddress,
		))
	}
	h.client.EditMessageText(chatID, msgID, sb.String(), backToMenuKB(lang))
}

func (h *WebhookHandler) editStats24h(chatID int64, msgID int, lang string) {
	stats, _ := h.storage.GetStats24h()
	var body string
	if stats != nil {
		if lang == "ru" {
			body = fmt.Sprintf(
				"📈 <b>Статистика за 24 часа</b>\n\n"+
					"🔢 Кластеров: <b>%d</b>\n"+
					"💰 Объём: <b>$%s</b>\n"+
					"🏆 Топ токен: <b>%s</b>\n"+
					"🌐 Топ сеть: <b>%s</b>",
				stats.TotalClusters, fmtFloat(stats.TotalVolumeUSD),
				html.EscapeString(or(stats.TopToken, "—")),
				html.EscapeString(or(stats.TopChain, "—")),
			)
		} else {
			body = fmt.Sprintf(
				"📈 <b>24h Statistics</b>\n\n"+
					"🔢 Clusters: <b>%d</b>\n"+
					"💰 Volume: <b>$%s</b>\n"+
					"🏆 Top Token: <b>%s</b>\n"+
					"🌐 Top Chain: <b>%s</b>",
				stats.TotalClusters, fmtFloat(stats.TotalVolumeUSD),
				html.EscapeString(or(stats.TopToken, "—")),
				html.EscapeString(or(stats.TopChain, "—")),
			)
		}
	} else {
		body = "❌ Error loading statistics."
		if lang == "ru" {
			body = "❌ Ошибка загрузки статистики."
		}
	}
	h.client.EditMessageText(chatID, msgID, body, backToMenuKB(lang))
}

func (h *WebhookHandler) editWatchlistMenu(chatID int64, msgID int, userID int64, lang string) {
	entries, _ := h.storage.GetWatchlist(userID)
	if len(entries) == 0 {
		msg := "⭐ <b>My Watchlist</b>\n\nYour list is empty.\n\nAdd using: <code>/watch <address> [note]</code>"
		if lang == "ru" {
			msg = "⭐ <b>Мой Watchlist</b>\n\nСписок пуст.\n\nДобавьте командой: <code>/watch <address> [заметка]</code>"
		}
		h.client.EditMessageText(chatID, msgID, msg, backToMenuKB(lang))
		return
	}
	var sb strings.Builder
	if lang == "ru" {
		sb.WriteString("⭐ <b>Мой Watchlist</b>\n\n")
	} else {
		sb.WriteString("⭐ <b>My Watchlist</b>\n\n")
	}
	var rows [][]InlineKeyboardButton
	for _, e := range entries {
		masked := maskAddr(e.WalletAddress)
		sb.WriteString("• <code>")
		sb.WriteString(html.EscapeString(masked))
		sb.WriteString("</code>")
		if e.Note != "" {
			sb.WriteString(" — ")
			sb.WriteString(html.EscapeString(e.Note))
		}
		sb.WriteString("\n")

		delText := "🗑 " + masked
		rows = append(rows, []InlineKeyboardButton{
			{Text: delText, CallbackData: fmt.Sprintf("cb:watchrm:%d", e.ID)},
		})
	}
	backText := "⬅️ Back"
	if lang == "ru" {
		backText = "⬅️ Назад"
	}
	rows = append(rows, []InlineKeyboardButton{{Text: backText, CallbackData: "cb:menu"}})
	h.client.EditMessageText(chatID, msgID, sb.String(), &InlineKeyboardMarkup{InlineKeyboard: rows})
}

func (h *WebhookHandler) editHotWallets(chatID int64, msgID int, lang string) {
	wallets, _ := h.storage.GetTopWallets(24, 5)
	if len(wallets) == 0 {
		msg := "🔥 <b>Hot Wallets</b>\n\nNo data available yet."
		if lang == "ru" {
			msg = "🔥 <b>Горячие кошельки</b>\n\nДанных пока нет."
		}
		h.client.EditMessageText(chatID, msgID, msg, backToMenuKB(lang))
		return
	}
	var sb strings.Builder
	if lang == "ru" {
		sb.WriteString("🔥 <b>Горячие кошельки — 24h</b>\n\n")
	} else {
		sb.WriteString("🔥 <b>Hot Wallets — 24h</b>\n\n")
	}
	medals := []string{"🥇", "🥈", "🥉", "4️⃣", "5️⃣"}
	for i, w := range wallets {
		medal := "•"
		if i < len(medals) {
			medal = medals[i]
		}
		sb.WriteString(fmt.Sprintf("%s <code>%s</code> — %d clusters · $%s\n",
			medal, html.EscapeString(maskAddr(w.WalletAddress)),
			w.ClusterCount, fmtFloat(w.TotalVolumeUSD),
		))
	}
	h.client.EditMessageText(chatID, msgID, sb.String(), backToMenuKB(lang))
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

	backText := "⬅️ Back"
	if lang == "ru" {
		backText = "⬅️ Назад"
	}

	kb := &InlineKeyboardMarkup{
		InlineKeyboard: [][]InlineKeyboardButton{
			// Volume filter row
			{
				{Text: "$10k", CallbackData: fmt.Sprintf("cb:vol:%d:10000", userID)},
				{Text: "$25k", CallbackData: fmt.Sprintf("cb:vol:%d:25000", userID)},
				{Text: "$50k", CallbackData: fmt.Sprintf("cb:vol:%d:50000", userID)},
				{Text: "$100k", CallbackData: fmt.Sprintf("cb:vol:%d:100000", userID)},
			},
			// Network toggles
			{
				{Text: checkETH + " ETH", CallbackData: fmt.Sprintf("cb:net:%d:eth", userID)},
				{Text: checkSOL + " SOL", CallbackData: fmt.Sprintf("cb:net:%d:sol", userID)},
				{Text: checkBASE + " BASE", CallbackData: fmt.Sprintf("cb:net:%d:base", userID)},
				{Text: checkBSC + " BSC", CallbackData: fmt.Sprintf("cb:net:%d:bsc", userID)},
			},
			// Language Toggle Button
			{
				{Text: langToggleText, CallbackData: "cb:lang"},
			},
			{{Text: backText, CallbackData: "cb:menu"}},
		},
	}
	h.client.EditMessageText(chatID, msgID, body, kb)
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

	if err := h.storage.UpdateUserSettings(
		userID, user.MinVolume,
		user.EthEnabled, user.SolEnabled, user.BaseEnabled, user.BscEnabled,
	); err != nil {
		log.Printf("[HANDLER] UpdateUserSettings %d: %v", userID, err)
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

// ── VIP Payment Gateways Implementation ───────────────────────────────────────

func (h *WebhookHandler) editPaymentStars(chatID int64, msgID int, lang string) {
	var body string
	if lang == "ru" {
		body = "⭐ <b>Оплата через Telegram Stars (XTR)</b>\n<b>━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━</b>\n\n" +
			"Получите 30 дней VIP доступа мгновенно через официальную валюту Telegram Stars.\n\n" +
			"Стоимость: <b>250 XTR</b>"
	} else {
		body = "⭐ <b>Telegram Stars Payment (XTR)</b>\n<b>━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━</b>\n\n" +
			"Get 30 days VIP access instantly using official Telegram Stars.\n\n" +
			"Price: <b>250 XTR</b>"
	}

	payBtn := "⭐ Pay 250 Stars"
	if lang == "ru" {
		payBtn = "⭐ Оплатить 250 Stars"
	}
	backText := tr(lang, "⬅️ Back", "⬅️ Назад")

	kb := &InlineKeyboardMarkup{
		InlineKeyboard: [][]InlineKeyboardButton{
			{{Text: payBtn, URL: "https://t.me/StarkWonder?start=stars_vip"}},
			{{Text: backText, CallbackData: "cb:vip"}},
		},
	}
	h.client.EditMessageText(chatID, msgID, body, kb)
}

func (h *WebhookHandler) editPaymentCryptoBot(chatID int64, msgID int, lang string) {
	invoiceURL, _ := h.createCryptoBotInvoice(chatID)
	if invoiceURL == "" {
		invoiceURL = "https://t.me/send?start=IVW392..." // Fallback
	}

	var body string
	if lang == "ru" {
		body = "🤖 <b>Оплата через CryptoBot (@CryptoBot)</b>\n" +
			"<pre>                                                            </pre>\n\n" +
			"Быстрая оплата в USDT или TON через официального бота @CryptoBot.\n\n" +
			"Стоимость: <b>$9.99 (USDT / TON)</b>"
	} else {
		body = "🤖 <b>CryptoBot Payment (@CryptoBot)</b>\n" +
			"<pre>                                                            </pre>\n\n" +
			"Fast payment in USDT or TON via official @CryptoBot.\n\n" +
			"Price: <b>$9.99 (USDT / TON)</b>"
	}

	payBtn := "💳 Оплатить счет"
	if lang != "ru" {
		payBtn = "💳 Pay Invoice"
	}
	backText := tr(lang, "⬅️ Back", "⬅️ Назад")

	kb := &InlineKeyboardMarkup{
		InlineKeyboard: [][]InlineKeyboardButton{
			{{Text: payBtn, URL: invoiceURL}},
			{{Text: backText, CallbackData: "cb:vip"}},
		},
	}
	h.client.EditMessageText(chatID, msgID, body, kb)
}

func (h *WebhookHandler) editPaymentDirectWallet(chatID int64, msgID int, lang string) {
	var body string
	if lang == "ru" {
		body = "💎 <b>Прямой перевод крипто (TRC20 / SOL)</b>\n<b>━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━</b>\n\n" +
			"Отправьте ровно <b>$10 USDT</b> (TRC20) или <b>0.05 SOL</b> на наш официальный кошелёк:\n\n" +
			"📌 <b>USDT (TRC20):</b>\n<code>T9yD14Nj9j7xAB4dbGeiX9h8unkKHxuWwb</code>\n\n" +
			"📌 <b>Solana (SOL):</b>\n<code>7xKXtg2CW87d97TXJSDpbD5jBkheTqA83TZRuJosgAsU</code>\n\n" +
			"После отправки нажмите кнопку ниже или отправьте TxID администратору @StarkWonder для активации."
	} else {
		body = "💎 <b>Direct Crypto Wallet (TRC20 / SOL)</b>\n<b>━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━</b>\n\n" +
			"Transfer exactly <b>$10 USDT</b> (TRC20) or <b>0.05 SOL</b> to our official wallet:\n\n" +
			"📌 <b>USDT (TRC20):</b>\n<code>T9yD14Nj9j7xAB4dbGeiX9h8unkKHxuWwb</code>\n\n" +
			"📌 <b>Solana (SOL):</b>\n<code>7xKXtg2CW87d97TXJSDpbD5jBkheTqA83TZRuJosgAsU</code>\n\n" +
			"After transferring, tap below or message @StarkWonder with your TxID for instant activation."
	}

	submitBtn := "✅ I Have Paid (Submit TxID)"
	if lang == "ru" {
		submitBtn = "✅ Я оплатил (Отправить TxID)"
	}
	backText := tr(lang, "⬅️ Back", "⬅️ Назад")

	kb := &InlineKeyboardMarkup{
		InlineKeyboard: [][]InlineKeyboardButton{
			{{Text: submitBtn, CallbackData: "pay:done:prompt"}},
			{{Text: backText, CallbackData: "cb:vip"}},
		},
	}
	h.client.EditMessageText(chatID, msgID, body, kb)
}

func (h *WebhookHandler) handleDirectPaymentSubmitted(chatID int64, msgID int, userID int64, data, lang string) {
	_ = h.storage.AddPendingPayment(userID, "direct_crypto", "PENDING_TX_VERIFY")
	var body string
	if lang == "ru" {
		body = "✅ <b>Платёж зафиксирован на проверку!</b>\n\n" +
			"Ваш запрос на активацию VIP отправлен администратору. Обычно проверка занимает до 15 минут.\n" +
			"По всем вопросам обращайтесь к @StarkWonder."
	} else {
		body = "✅ <b>Payment submitted for verification!</b>\n\n" +
			"Your VIP activation request has been logged for admin review (usually within 15 minutes).\n" +
			"Contact @StarkWonder if you need urgent support."
	}
	h.client.EditMessageText(chatID, msgID, body, backToMenuKB(lang))
}

func (h *WebhookHandler) editVIPInfo(chatID int64, msgID int, lang string) {
	var body string
	if lang == "ru" {
		body = "👑 <b>VIP Пасс — Smart Cluster Terminal</b>\n<b>━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━</b>\n\n" +
			"<b>Что входит:</b>\n" +
			"🔓 100% адресов всех кошельков без маскировки\n" +
			"⚡ Мгновенные Telegram алерты\n" +
			"🎯 Кастомные фильтры по токену и объёму\n" +
			"📈 Полный архив кластеров + экспорт CSV\n" +
			"🔥 Персональный список горячих кошельков\n\n" +
			"💳 <b>Оплата:</b> Telegram Stars или крипто\n\n" +
			"Свяжитесь с @StarkWonder для активации."
	} else {
		body = "👑 <b>VIP Pass — Smart Cluster Terminal</b>\n<b>━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━</b>\n\n" +
			"<b>What's included:</b>\n" +
			"🔓 100% unmasked wallet addresses\n" +
			"⚡ Instant Telegram alerts\n" +
			"🎯 Custom token & volume filters\n" +
			"📈 Full cluster history + CSV export\n" +
			"🔥 Personalized hot wallet rankings\n\n" +
			"💳 <b>Payment:</b> Telegram Stars or Crypto\n\n" +
			"Contact @StarkWonder for activation."
	}

	btnText := "🔑 Buy VIP"
	if lang == "ru" {
		btnText = "🔑 Купить VIP"
	}
	backText := "⬅️ Back"
	if lang == "ru" {
		backText = "⬅️ Назад"
	}

	kb := &InlineKeyboardMarkup{
		InlineKeyboard: [][]InlineKeyboardButton{
			{{Text: btnText, URL: "https://t.me/StarkWonder"}},
			{{Text: backText, CallbackData: "cb:menu"}},
		},
	}
	h.client.EditMessageText(chatID, msgID, body, kb)
}

// ── Help ───────────────────────────────────────────────────────────────────────

func (h *WebhookHandler) editHelp(chatID int64, msgID int, lang string) {
	var body string
	if lang == "ru" {
		body = "❓ <b>Помощь — команды бота</b>\n<b>━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━</b>\n\n" +
			"<code>/start</code> — главное меню\n" +
			"<code>/watch <addr> [заметка]</code> — добавить кошелёк в watchlist\n" +
			"<code>/watchlist</code> — показать watchlist\n" +
			"<code>/stats</code> — статистика за 24 часа\n" +
			"<code>/hot</code> — топ горячих кошельков\n\n" +
			"<b>Как работают кластеры:</b>\n" +
			"Система отслеживает покупки на DEX и сигнализирует, " +
			"когда ≥3 умных кошелька аккумулируют один токен в течение 5 минут."
	} else {
		body = "❓ <b>Help — Bot Commands</b>\n<b>━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━</b>\n\n" +
			"<code>/start</code> — main menu\n" +
			"<code>/watch <addr> [note]</code> — add wallet to watchlist\n" +
			"<code>/watchlist</code> — view watchlist\n" +
			"<code>/stats</code> — 24h statistics\n" +
			"<code>/hot</code> — top hot wallets\n\n" +
			"<b>How clusters work:</b>\n" +
			"The system tracks DEX swaps and signals when ≥3 smart wallets " +
			"accumulate the same token within 5 minutes."
	}

	h.client.EditMessageText(chatID, msgID, body, backToMenuKB(lang))
}

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
		"asset": "USDT", "amount": "9.99",
		"description": "Smart Cluster Terminal — VIP Pass",
		"payload":     fmt.Sprintf("vip_%d_%d", userID, time.Now().Unix()),
	}
	jsonBytes, _ := json.Marshal(payload)
	req, _ := http.NewRequest(http.MethodPost, "https://pay.crypt.bot/api/createInvoice", bytes.NewBuffer(jsonBytes))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Crypto-Pay-API-Token", token)

	resp, _ := (&http.Client{Timeout: 8 * time.Second}).Do(req)
	defer resp.Body.Close()

	var result struct {
		Ok     bool `json:"ok"`
		Result struct {
			PayURL string `json:"pay_url"`
		} `json:"result"`
	}
	json.NewDecoder(resp.Body).Decode(&result)
	return result.Result.PayURL, nil
}

// ── URL helpers ────────────────────────────────────────────────────────────────

func (h *WebhookHandler) webAppURL() string {
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

// ── Manual / Guide Handler ──────────────────────────────────────────────────────

func (h *WebhookHandler) sendManual(chatID int64, lang string) {
	var body string
	if lang == "ru" {
		body = "📖 <b>Руководство пользователя — Smart Cluster Terminal</b>\n" +
			"<b>━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━</b>\n\n" +
			"<b>1. Что такое Кластер (Cluster)?</b>\n" +
			"Кластер — это момент времени, когда несколько независимых высокоприбыльных кошельков (Smart Money) начинают массово аккумулировать один и тот же токен в течение короткого окна (5 минут).\n\n" +
			"<b>2. Золотое правило (DYOR & RugCheck):</b>\n" +
			"🛡 <b>ВСЕГДА проверяйте токены через [🛡 RugCheck]</b> перед тем как взаимодействовать с ними! До 90% новых токенов являются высокорисковыми или скамом (honeypot, rugpull).\n\n" +
			"<b>3. Как использовать бота:</b>\n" +
			"• 📊 Открывайте WebApp терминал для интерактивного анализа в реальном времени.\n" +
			"• 🔔 Настраивайте минимальный объём и отслеживаемые сети в настройках.\n" +
			"• ⭐ Добавляйте важные кошельки в Watchlist с помощью <code>/watch <addr> [заметка]</code>.\n\n" +
			"⚠️ <b>Важно:</b> Это исключительно аналитический инструмент, а не печатный станок для денег. Делайте собственное исследование (Do Your Own Research — DYOR)!"
	} else {
		body = "📖 <b>User Guide — Smart Cluster Terminal</b>\n" +
			"<b>━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━</b>\n\n" +
			"<b>1. What is a Cluster?</b>\n" +
			"A cluster occurs when multiple independent high-conviction wallets (Smart Money) simultaneously accumulate the same token within a tight rolling window (5 minutes).\n\n" +
			"<b>2. The Golden Rule (DYOR & RugCheck):</b>\n" +
			"🛡 <b>ALWAYS check tokens via [🛡 RugCheck]</b> before interacting! Up to 90% of new tokens carry extreme risk or are scams (honeypots, rugpulls).\n\n" +
			"<b>3. How to use the Bot:</b>\n" +
			"• 📊 Open the WebApp terminal for real-time interactive analytics.\n" +
			"• 🔔 Configure min volume and enabled networks in Settings.\n" +
			"• ⭐ Track specific wallets using <code>/watch <addr> [note]</code>.\n\n" +
			"⚠️ <b>Important:</b> This is strictly an analytical scanner, not a magic money printer. Do Your Own Research (DYOR)!"
	}

	h.client.SendMessageWithKeyboard(chatID, body, backToMenuKB(lang))
}

func (h *WebhookHandler) sendVIPMenu(chatID int64, lang string) {
	var body string
	if lang == "ru" {
		body = "👑 <b>VIP Пасс — Smart Cluster Terminal</b>\n" +
			"<b>━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━</b>\n\n" +
			"Выберите удобный способ оплаты VIP-доступа на 30 дней:\n\n" +
			"🔓 100% адресов без маскировки\n" +
			"⚡ Мгновенные алерты и экспорт CSV"
	} else {
		body = "👑 <b>VIP Pass — Smart Cluster Terminal</b>\n" +
			"<b>━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━</b>\n\n" +
			"Select your preferred payment method for 30 days VIP access:\n\n" +
			"🔓 100% unmasked addresses\n" +
			"⚡ Instant alerts & CSV export"
	}

	btnStars := "⭐ Telegram Stars (XTR)"
	btnCryptoBot := "🤖 CryptoBot (@CryptoBot)"
	btnWallet := "💎 Direct Crypto (TRC20 / SOL)"
	backText := tr(lang, "⬅️ Back", "⬅️ Назад")

	kb := &InlineKeyboardMarkup{
		InlineKeyboard: [][]InlineKeyboardButton{
			{{Text: btnStars, CallbackData: "pay:stars"}},
			{{Text: btnCryptoBot, CallbackData: "pay:cryptobot"}},
			{{Text: btnWallet, CallbackData: "pay:wallet"}},
			{{Text: backText, CallbackData: "cb:menu"}},
		},
	}
	h.client.SendMessageWithKeyboard(chatID, body, kb)
}
