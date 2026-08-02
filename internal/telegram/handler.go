package telegram

import (
	"encoding/json"
	"fmt"
	"html"
	"log"
	"net/http"
	"strconv"
	"strings"

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
	lang := from.LanguageCode
	if lang == "" {
		lang = "en"
	}
	if strings.HasPrefix(strings.ToLower(lang), "ru") {
		lang = "ru"
	} else {
		lang = "en" // Default to English as required
	}

	user, err := h.storage.GetOrCreateUser(from.ID, from.Username, lang)
	if err != nil {
		log.Printf("[HANDLER] GetOrCreateUser %d: %v", from.ID, err)
		return
	}

	text := strings.TrimSpace(msg.Text)

	switch {
	case text == "/start":
		h.sendStartMenu(chatID, from.FirstName, user)

	case strings.HasPrefix(text, "/watch"):
		h.handleWatchCommand(chatID, from.ID, text, user.Language)

	case text == "/watchlist":
		h.sendWatchlistMenu(chatID, from.ID, user.Language)

	case text == "/stats":
		h.sendStats24h(chatID, user.Language)

	case text == "/hot":
		h.sendHotWallets(chatID, user.Language)

	default:
		h.sendStartMenu(chatID, from.FirstName, user)
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
			"👋 <b>Привет, %s!</b>\n"+
				"<b>━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━</b>\n"+
				"📊 <b>Smart Cluster Terminal</b>\n"+
				"Аналитика и безопасность смарт-денег в реальном времени.\n\n"+
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
			"👋 <b>Hello, %s!</b>\n"+
				"<b>━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━</b>\n"+
				"📊 <b>Smart Cluster Terminal</b>\n"+
				"Real-time Smart Money Analytics & Security.\n\n"+
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

	if err := h.client.SendMessageWithKeyboard(chatID, body, kb); err != nil {
		log.Printf("[HANDLER] sendStartMenu %d: %v", chatID, err)
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
	lang := cb.From.LanguageCode
	if lang == "" {
		lang = "en"
	}
	if strings.HasPrefix(strings.ToLower(lang), "ru") {
		lang = "ru"
	} else {
		lang = "en"
	}
	user, _ := h.storage.GetOrCreateUser(userID, cb.From.Username, lang)
	currentLang := user.Language

	switch {
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
	plan := "FREE"
	if user.IsVIP {
		plan = "👑 VIP"
	}
	lang := user.Language
	var body string
	if lang == "ru" {
		body = fmt.Sprintf(
			"👋 <b>Привет, %s!</b>\n\n"+
				"📊 <b>Smart Cluster Terminal</b>\n\n"+
				"👤 План: <b>%s</b>\n"+
				"🔔 Мин. объём: <b>$%s</b>\n"+
				"🌐 Сети: %s\n"+
				"🌐 Язык: <b>Русский (RU)</b>\n\nВыберите действие:",
			html.EscapeString(firstName), html.EscapeString(plan),
			fmtVolume(user.MinVolume), enabledNetworks(user),
		)
	} else {
		body = fmt.Sprintf(
			"👋 <b>Hello, %s!</b>\n\n"+
				"📊 <b>Smart Cluster Terminal</b>\n\n"+
				"👤 Plan: <b>%s</b>\n"+
				"🔔 Min Volume: <b>$%s</b>\n"+
				"🌐 Networks: %s\n"+
				"🌐 Language: <b>English (EN)</b>\n\nChoose an action:",
			html.EscapeString(firstName), html.EscapeString(plan),
			fmtVolume(user.MinVolume), enabledNetworks(user),
		)
	}

	kb := &InlineKeyboardMarkup{
		InlineKeyboard: [][]InlineKeyboardButton{
			{{Text: tr(lang, "📊 Open Terminal", "📊 Открыть Terminal"), WebApp: &WebAppInfo{URL: h.webAppURL()}}},
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
			{{Text: tr(lang, "👑 VIP Pass", "👑 VIP Пасс"), CallbackData: "cb:vip"}},
		},
	}
	h.client.EditMessageText(chatID, msgID, body, kb)
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

// ── VIP info ───────────────────────────────────────────────────────────────────

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
