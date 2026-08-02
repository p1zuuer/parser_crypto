package telegram

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"

	"smart-cluster-bot/internal/config"
	"smart-cluster-bot/internal/i18n"
	"smart-cluster-bot/internal/storage"
)

// ── WebhookHandler ─────────────────────────────────────────────────────────────

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

	// Respond fast — Telegram retries if we don't reply within a few seconds.
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

	// Ensure user exists in DB.
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
		h.handleWatchCommand(chatID, from.ID, text)

	case text == "/watchlist":
		h.sendWatchlistMenu(chatID, from.ID)

	case text == "/stats":
		h.sendStats24h(chatID)

	case text == "/hot":
		h.sendHotWallets(chatID)

	default:
		// Unknown command — show the main menu.
		h.sendStartMenu(chatID, from.FirstName, user)
	}
}

// ── /start menu ────────────────────────────────────────────────────────────────

func (h *WebhookHandler) sendStartMenu(chatID int64, firstName string, user *storage.User) {
	plan := "FREE"
	if user.IsVIP {
		plan = "👑 VIP"
	}

	_ = h.i18n // available for future localisation of menu strings
	webAppURL := h.webAppURL()

	body := fmt.Sprintf(
		"👋 *Привет, %s\\!*\n\n"+
			"📊 *Smart Cluster Terminal*\n"+
			"Отслеживание умных денег в реальном времени\\.\n\n"+
			"👤 План: *%s*\n"+
			"🔔 Мин\\. объём: *$%s*\n"+
			"🌐 Сети: %s\n\n"+
			"Выберите действие:",
		escMD(firstName),
		escMD(plan),
		fmtVolume(user.MinVolume),
		enabledNetworks(user),
	)

	kb := &InlineKeyboardMarkup{
		InlineKeyboard: [][]InlineKeyboardButton{
			{
				{Text: "📊 Открыть Terminal", WebApp: &WebAppInfo{URL: webAppURL}},
			},
			{
				{Text: "🔥 Свежие кластеры", CallbackData: "cb:clusters"},
				{Text: "📈 Статистика 24h", CallbackData: "cb:stats"},
			},
			{
				{Text: "⭐ Мой Watchlist", CallbackData: "cb:watchlist"},
				{Text: "⚙️ Настройки", CallbackData: "cb:settings"},
			},
			{
				{Text: "🔥 Горячие кошельки", CallbackData: "cb:hot"},
				{Text: "❓ Помощь", CallbackData: "cb:help"},
			},
			{
				{Text: "👑 VIP Пасс", CallbackData: "cb:vip"},
			},
		},
	}

	if err := h.client.SendMessageWithKeyboard(chatID, body, kb); err != nil {
		log.Printf("[HANDLER] sendStartMenu %d: %v", chatID, err)
	}
}

// ── /watch command ─────────────────────────────────────────────────────────────

// handleWatchCommand parses "/watch <address> [note...]" and adds the wallet to
// the user's watchlist.
func (h *WebhookHandler) handleWatchCommand(chatID, userID int64, text string) {
	parts := strings.Fields(text)
	if len(parts) < 2 {
		h.client.SendMessage(chatID,
			"ℹ️ Usage: `/watch <wallet_address> [optional note]`\n\n"+
				"Example:\n`/watch 0xABCDEF... big whale`")
		return
	}
	addr := parts[1]
	note := strings.Join(parts[2:], " ")

	if err := h.storage.AddWatchlistWallet(userID, addr, note); err != nil {
		log.Printf("[HANDLER] AddWatchlistWallet %d: %v", userID, err)
		h.client.SendMessage(chatID, "❌ Не удалось добавить кошелёк. Попробуйте позже.")
		return
	}

	reply := fmt.Sprintf(
		"✅ *Кошелёк добавлен в Watchlist\\!*\n\n"+
			"`%s`\n"+
			"📝 Заметка: %s\n\n"+
			"Вы получите уведомление, как только этот кошелёк купит что-нибудь\\.",
		escMD(addr),
		escMD(or(note, "—")),
	)
	h.client.SendMessageWithKeyboard(chatID, reply, backToMenuKB())
}

// ── Watchlist menu ─────────────────────────────────────────────────────────────

func (h *WebhookHandler) sendWatchlistMenu(chatID, userID int64) {
	entries, err := h.storage.GetWatchlist(userID)
	if err != nil {
		log.Printf("[HANDLER] GetWatchlist %d: %v", userID, err)
		h.client.SendMessage(chatID, "❌ Ошибка загрузки watchlist.")
		return
	}

	if len(entries) == 0 {
		h.client.SendMessageWithKeyboard(chatID,
			"⭐ *Мой Watchlist*\n\nСписок пуст\\.\n\n"+
				"Добавьте кошелёк командой:\n`/watch <address> [заметка]`",
			backToMenuKB(),
		)
		return
	}

	var sb strings.Builder
	sb.WriteString("⭐ *Мой Watchlist*\n\n")

	// Build remove buttons (one per row).
	var rows [][]InlineKeyboardButton
	for _, e := range entries {
		masked := maskAddr(e.WalletAddress)
		sb.WriteString("• `")
		sb.WriteString(escMD(masked))
		sb.WriteString("`")
		if e.Note != "" {
			sb.WriteString(" — ")
			sb.WriteString(escMD(e.Note))
		}
		sb.WriteString("\n")

		rows = append(rows, []InlineKeyboardButton{
			{
				Text:         "🗑 Удалить " + masked,
				CallbackData: fmt.Sprintf("cb:watchrm:%d", e.ID),
			},
		})
	}
	sb.WriteString("\nНажмите кнопку чтобы удалить запись\\.")

	rows = append(rows, []InlineKeyboardButton{
		{Text: "⬅️ Назад", CallbackData: "cb:menu"},
	})
	h.client.SendMessageWithKeyboard(chatID, sb.String(), &InlineKeyboardMarkup{InlineKeyboard: rows})
}

// ── Stats 24h ──────────────────────────────────────────────────────────────────

func (h *WebhookHandler) sendStats24h(chatID int64) {
	stats, err := h.storage.GetStats24h()
	if err != nil {
		log.Printf("[HANDLER] GetStats24h: %v", err)
		h.client.SendMessage(chatID, "❌ Ошибка загрузки статистики.")
		return
	}

	body := fmt.Sprintf(
		"📈 *Статистика за 24 часа*\n\n"+
			"🔢 Кластеров обнаружено: *%d*\n"+
			"💰 Общий объём: *$%s*\n"+
			"🏆 Топ токен: *%s*\n"+
			"🌐 Активная сеть: *%s*",
		stats.TotalClusters,
		fmtFloat(stats.TotalVolumeUSD),
		escMD(or(stats.TopToken, "—")),
		escMD(or(stats.TopChain, "—")),
	)
	h.client.SendMessageWithKeyboard(chatID, body, backToMenuKB())
}

// ── Hot wallets ─────────────────────────────────────────────────────────────────

func (h *WebhookHandler) sendHotWallets(chatID int64) {
	wallets, err := h.storage.GetTopWallets(24, 5)
	if err != nil {
		log.Printf("[HANDLER] GetTopWallets: %v", err)
		h.client.SendMessage(chatID, "❌ Ошибка загрузки горячих кошельков.")
		return
	}

	if len(wallets) == 0 {
		h.client.SendMessageWithKeyboard(chatID,
			"🔥 *Горячие кошельки*\n\nДанных пока нет\\.",
			backToMenuKB(),
		)
		return
	}

	var sb strings.Builder
	sb.WriteString("🔥 *Горячие кошельки — 24h*\n")
	sb.WriteString("_Кошельки, появляющиеся в наибольшем числе кластеров_\n\n")

	medals := []string{"🥇", "🥈", "🥉", "4️⃣", "5️⃣"}
	for i, w := range wallets {
		medal := "•"
		if i < len(medals) {
			medal = medals[i]
		}
		sb.WriteString(fmt.Sprintf(
			"%s `%s`\n   %d кластеров · $%s\n\n",
			medal,
			escMD(maskAddr(w.WalletAddress)),
			w.ClusterCount,
			fmtFloat(w.TotalVolumeUSD),
		))
	}
	sb.WriteString("_Повторные покупки = наиболее убеждённые умные деньги_")

	h.client.SendMessageWithKeyboard(chatID, sb.String(), backToMenuKB())
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

	// Acknowledge quickly so Telegram removes the loading spinner.
	_ = h.client.AnswerCallbackQuery(cb.ID, "")

	switch {
	case data == "cb:menu":
		user, _ := h.storage.GetOrCreateUser(userID, cb.From.Username, cb.From.LanguageCode)
		h.editStartMenu(chatID, msgID, cb.From.FirstName, user)

	case data == "cb:clusters":
		h.editRecentClusters(chatID, msgID)

	case data == "cb:stats":
		h.editStats24h(chatID, msgID)

	case data == "cb:watchlist":
		h.editWatchlistMenu(chatID, msgID, userID)

	case data == "cb:hot":
		h.editHotWallets(chatID, msgID)

	case data == "cb:settings":
		user, _ := h.storage.GetOrCreateUser(userID, cb.From.Username, cb.From.LanguageCode)
		h.editSettingsMenu(chatID, msgID, userID, user)

	case data == "cb:vip":
		h.editVIPInfo(chatID, msgID)

	case data == "cb:help":
		h.editHelp(chatID, msgID)

	case strings.HasPrefix(data, "cb:vol:"):
		h.handleVolumeChange(chatID, msgID, userID, cb.From.Username, cb.From.LanguageCode, data)

	case strings.HasPrefix(data, "cb:net:"):
		h.handleNetworkToggle(chatID, msgID, userID, cb.From.Username, cb.From.LanguageCode, data)

	case strings.HasPrefix(data, "cb:watchrm:"):
		h.handleWatchlistRemove(chatID, msgID, userID, data)
	}
}

// ── Edit-in-place helpers (keep the same message, swap content) ───────────────

func (h *WebhookHandler) editStartMenu(chatID int64, msgID int, firstName string, user *storage.User) {
	plan := "FREE"
	if user.IsVIP {
		plan = "👑 VIP"
	}
	body := fmt.Sprintf(
		"👋 *Привет, %s\\!*\n\n"+
			"📊 *Smart Cluster Terminal*\n\n"+
			"👤 План: *%s*\n"+
			"🔔 Мин\\. объём: *$%s*\n"+
			"🌐 Сети: %s\n\nВыберите действие:",
		escMD(firstName), escMD(plan),
		fmtVolume(user.MinVolume), enabledNetworks(user),
	)
	kb := &InlineKeyboardMarkup{
		InlineKeyboard: [][]InlineKeyboardButton{
			{{Text: "📊 Открыть Terminal", WebApp: &WebAppInfo{URL: h.webAppURL()}}},
			{
				{Text: "🔥 Свежие кластеры", CallbackData: "cb:clusters"},
				{Text: "📈 Статистика 24h", CallbackData: "cb:stats"},
			},
			{
				{Text: "⭐ Мой Watchlist", CallbackData: "cb:watchlist"},
				{Text: "⚙️ Настройки", CallbackData: "cb:settings"},
			},
			{
				{Text: "🔥 Горячие кошельки", CallbackData: "cb:hot"},
				{Text: "❓ Помощь", CallbackData: "cb:help"},
			},
			{{Text: "👑 VIP Пасс", CallbackData: "cb:vip"}},
		},
	}
	h.client.EditMessageText(chatID, msgID, body, kb)
}

func (h *WebhookHandler) editRecentClusters(chatID int64, msgID int) {
	clusters, err := h.storage.GetRecentClusters(5)
	if err != nil || len(clusters) == 0 {
		h.client.EditMessageText(chatID, msgID,
			"🔥 *Свежие кластеры*\n\nДанных пока нет\\.", backToMenuKB())
		return
	}
	var sb strings.Builder
	sb.WriteString("🔥 *Последние кластеры*\n\n")
	for _, c := range clusters {
		sb.WriteString(fmt.Sprintf(
			"• *%s* \\(%s\\)\n  💰 $%s · %d покупок\n\n",
			escMD(c.TokenSymbol), escMD(c.Chain),
			fmtFloat(c.TotalVolumeUSD), c.BuyCount,
		))
	}
	h.client.EditMessageText(chatID, msgID, sb.String(), backToMenuKB())
}

func (h *WebhookHandler) editStats24h(chatID int64, msgID int) {
	stats, _ := h.storage.GetStats24h()
	var body string
	if stats != nil {
		body = fmt.Sprintf(
			"📈 *Статистика за 24 часа*\n\n"+
				"🔢 Кластеров: *%d*\n"+
				"💰 Объём: *$%s*\n"+
				"🏆 Топ токен: *%s*\n"+
				"🌐 Топ сеть: *%s*",
			stats.TotalClusters, fmtFloat(stats.TotalVolumeUSD),
			escMD(or(stats.TopToken, "—")),
			escMD(or(stats.TopChain, "—")),
		)
	} else {
		body = "❌ Ошибка загрузки статистики\\."
	}
	h.client.EditMessageText(chatID, msgID, body, backToMenuKB())
}

func (h *WebhookHandler) editWatchlistMenu(chatID int64, msgID int, userID int64) {
	entries, _ := h.storage.GetWatchlist(userID)
	if len(entries) == 0 {
		h.client.EditMessageText(chatID, msgID,
			"⭐ *Мой Watchlist*\n\nСписок пуст\\.\n\n"+
				"Добавьте командой: `/watch <address> [заметка]`",
			backToMenuKB(),
		)
		return
	}
	var sb strings.Builder
	sb.WriteString("⭐ *Мой Watchlist*\n\n")
	var rows [][]InlineKeyboardButton
	for _, e := range entries {
		masked := maskAddr(e.WalletAddress)
		sb.WriteString("• `")
		sb.WriteString(escMD(masked))
		sb.WriteString("`")
		if e.Note != "" {
			sb.WriteString(" — ")
			sb.WriteString(escMD(e.Note))
		}
		sb.WriteString("\n")
		rows = append(rows, []InlineKeyboardButton{
			{Text: "🗑 " + masked, CallbackData: fmt.Sprintf("cb:watchrm:%d", e.ID)},
		})
	}
	rows = append(rows, []InlineKeyboardButton{{Text: "⬅️ Назад", CallbackData: "cb:menu"}})
	h.client.EditMessageText(chatID, msgID, sb.String(), &InlineKeyboardMarkup{InlineKeyboard: rows})
}

func (h *WebhookHandler) editHotWallets(chatID int64, msgID int) {
	wallets, _ := h.storage.GetTopWallets(24, 5)
	if len(wallets) == 0 {
		h.client.EditMessageText(chatID, msgID, "🔥 *Горячие кошельки*\n\nДанных пока нет\\.", backToMenuKB())
		return
	}
	var sb strings.Builder
	sb.WriteString("🔥 *Горячие кошельки — 24h*\n\n")
	medals := []string{"🥇", "🥈", "🥉", "4️⃣", "5️⃣"}
	for i, w := range wallets {
		medal := "•"
		if i < len(medals) {
			medal = medals[i]
		}
		sb.WriteString(fmt.Sprintf("%s `%s` — %d кластеров · $%s\n",
			medal, escMD(maskAddr(w.WalletAddress)),
			w.ClusterCount, fmtFloat(w.TotalVolumeUSD),
		))
	}
	h.client.EditMessageText(chatID, msgID, sb.String(), backToMenuKB())
}

// ── Settings menu ──────────────────────────────────────────────────────────────

func (h *WebhookHandler) editSettingsMenu(chatID int64, msgID int, userID int64, user *storage.User) {
	body := fmt.Sprintf(
		"⚙️ *Настройки алертов*\n\n"+
			"Текущий мин\\. объём: *$%s*\n"+
			"Активные сети: %s\n\n"+
			"Изменения сохраняются мгновенно\\.",
		fmtVolume(user.MinVolume), enabledNetworks(user),
	)

	checkETH := emojiCheck(user.EthEnabled)
	checkSOL := emojiCheck(user.SolEnabled)
	checkBASE := emojiCheck(user.BaseEnabled)
	checkBSC := emojiCheck(user.BscEnabled)

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
			{{Text: "⬅️ Назад", CallbackData: "cb:menu"}},
		},
	}
	h.client.EditMessageText(chatID, msgID, body, kb)
}

func (h *WebhookHandler) handleVolumeChange(chatID int64, msgID int, userID int64, username, lang, data string) {
	// data = "cb:vol:<userID>:<volume>"
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
	// data = "cb:net:<userID>:<eth|sol|base|bsc>"
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

func (h *WebhookHandler) handleWatchlistRemove(chatID int64, msgID int, userID int64, data string) {
	// data = "cb:watchrm:<entryID>"
	parts := strings.Split(data, ":")
	if len(parts) < 3 {
		return
	}
	entryID, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return
	}
	_ = h.storage.RemoveWatchlistWallet(userID, entryID)
	h.editWatchlistMenu(chatID, msgID, userID)
}

// ── VIP info ───────────────────────────────────────────────────────────────────

func (h *WebhookHandler) editVIPInfo(chatID int64, msgID int) {
	body := "👑 *VIP Пасс — Smart Cluster Terminal*\n\n" +
		"*Что входит:*\n" +
		"🔓 100% адресов всех кошельков без маскировки\n" +
		"⚡ Мгновенные Telegram алерты\n" +
		"🎯 Кастомные фильтры по токену и объёму\n" +
		"📈 Полный архив кластеров + экспорт CSV\n" +
		"🔥 Персональный список горячих кошельков\n\n" +
		"💳 *Оплата:* Telegram Stars или крипто\n\n" +
		"Свяжитесь с @support для активации\\."

	kb := &InlineKeyboardMarkup{
		InlineKeyboard: [][]InlineKeyboardButton{
			{{Text: "🔑 Купить VIP", URL: "https://t.me/support"}},
			{{Text: "⬅️ Назад", CallbackData: "cb:menu"}},
		},
	}
	h.client.EditMessageText(chatID, msgID, body, kb)
}

// ── Help ───────────────────────────────────────────────────────────────────────

func (h *WebhookHandler) editHelp(chatID int64, msgID int) {
	body := "❓ *Помощь — команды бота*\n\n" +
		"`/start` — главное меню\n" +
		"`/watch <addr> [заметка]` — добавить кошелёк в watchlist\n" +
		"`/watchlist` — показать watchlist\n" +
		"`/stats` — статистика за 24 часа\n" +
		"`/hot` — топ горячих кошельков\n\n" +
		"*Как работают кластеры:*\n" +
		"Система отслеживает покупки на DEX и сигнализирует, " +
		"когда ≥3 умных кошелька аккумулируют один токен в течение 5 минут\\."

	h.client.EditMessageText(chatID, msgID, body, backToMenuKB())
}

// ── URL helpers ────────────────────────────────────────────────────────────────

func (h *WebhookHandler) webAppURL() string {
	if h.config != nil && h.config.RenderURL != "" {
		return h.config.RenderURL + "/app"
	}
	return "http://localhost:8080/app"
}

// ── Shared UI helpers ──────────────────────────────────────────────────────────

func backToMenuKB() *InlineKeyboardMarkup {
	return &InlineKeyboardMarkup{
		InlineKeyboard: [][]InlineKeyboardButton{
			{{Text: "⬅️ Главное меню", CallbackData: "cb:menu"}},
		},
	}
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
		return "_none_"
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

// escMD escapes special characters for MarkdownV2.
func escMD(s string) string {
	var sb strings.Builder
	for _, r := range s {
		switch r {
		case '_', '*', '[', ']', '(', ')', '~', '`', '>', '#', '+', '-', '=', '|', '{', '}', '.', '!', '\\':
			sb.WriteRune('\\')
		}
		sb.WriteRune(r)
	}
	return sb.String()
}

func or(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
