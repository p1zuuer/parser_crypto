package telegram

// ── Incoming update types ──────────────────────────────────────────────────────

// Update is an incoming Telegram webhook payload.
type Update struct {
	UpdateID      int            `json:"update_id"`
	Message       *Message       `json:"message,omitempty"`
	CallbackQuery *CallbackQuery `json:"callback_query,omitempty"`
}

// Message is a Telegram message received from a user.
type Message struct {
	MessageID         int                `json:"message_id"`
	From              *User              `json:"from,omitempty"`
	Chat              Chat               `json:"chat"`
	Text              string             `json:"text,omitempty"`
	SuccessfulPayment *SuccessfulPayment `json:"successful_payment,omitempty"`
}

// SuccessfulPayment contains data about a successful payment.
type SuccessfulPayment struct {
	Currency                string `json:"currency"`
	TotalAmount             int    `json:"total_amount"`
	InvoicePayload          string `json:"invoice_payload"`
	TelegramPaymentChargeID string `json:"telegram_payment_charge_id"`
}

// CallbackQuery is fired when a user taps an inline keyboard button.
type CallbackQuery struct {
	ID      string   `json:"id"`
	From    User     `json:"from"`
	Message *Message `json:"message,omitempty"`
	Data    string   `json:"data"`
}

// Chat holds the chat ID.
type Chat struct {
	ID int64 `json:"id"`
}

// User is a Telegram user/bot.
type User struct {
	ID           int64  `json:"id"`
	IsBot        bool   `json:"is_bot"`
	FirstName    string `json:"first_name"`
	Username     string `json:"username,omitempty"`
	LanguageCode string `json:"language_code,omitempty"`
}

// ── Outgoing request / response types ─────────────────────────────────────────

// SendMessageRequest is the body sent to the Telegram sendMessage API.
type SendMessageRequest struct {
	ChatID      int64                 `json:"chat_id"`
	Text        string                `json:"text"`
	ParseMode   string                `json:"parse_mode,omitempty"`
	ReplyMarkup *InlineKeyboardMarkup `json:"reply_markup,omitempty"`
}

// EditMessageTextRequest is the body sent to editMessageText.
type EditMessageTextRequest struct {
	ChatID      int64                 `json:"chat_id"`
	MessageID   int                   `json:"message_id"`
	Text        string                `json:"text"`
	ParseMode   string                `json:"parse_mode,omitempty"`
	ReplyMarkup *InlineKeyboardMarkup `json:"reply_markup,omitempty"`
}

// AnswerCallbackQueryRequest acknowledges a callback query.
type AnswerCallbackQueryRequest struct {
	CallbackQueryID string `json:"callback_query_id"`
	Text            string `json:"text,omitempty"`
	ShowAlert       bool   `json:"show_alert,omitempty"`
}

// InlineKeyboardMarkup wraps a 2-D grid of inline buttons.
type InlineKeyboardMarkup struct {
	InlineKeyboard [][]InlineKeyboardButton `json:"inline_keyboard"`
}

// InlineKeyboardButton is a single tappable inline button.
type InlineKeyboardButton struct {
	Text         string      `json:"text"`
	CallbackData string      `json:"callback_data,omitempty"`
	URL          string      `json:"url,omitempty"`
	WebApp       *WebAppInfo `json:"web_app,omitempty"`
}

// WebAppInfo carries the URL for Telegram Mini App (WebApp) buttons.
type WebAppInfo struct {
	URL string `json:"url"`
}

// APIResponse is Telegram's generic response envelope.
type APIResponse struct {
	Ok          bool   `json:"ok"`
	Description string `json:"description,omitempty"`
	ErrorCode   int    `json:"error_code,omitempty"`
}

// LabeledPrice represents a portion of the price for goods or services.
type LabeledPrice struct {
	Label  string `json:"label"`
	Amount int    `json:"amount"`
}

// SendInvoiceRequest is the body sent to sendInvoice.
type SendInvoiceRequest struct {
	ChatID         int64                 `json:"chat_id"`
	Title          string                `json:"title"`
	Description    string                `json:"description"`
	Payload        string                `json:"payload"`
	ProviderToken  string                `json:"provider_token"`
	Currency       string                `json:"currency"`
	Prices         []LabeledPrice        `json:"prices"`
	StartParameter string                `json:"start_parameter,omitempty"`
	ReplyMarkup    *InlineKeyboardMarkup `json:"reply_markup,omitempty"`
}
