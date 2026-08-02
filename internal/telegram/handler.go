package telegram

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strings"

	"smart-cluster-bot/internal/config"
	"smart-cluster-bot/internal/i18n"
	"smart-cluster-bot/internal/storage"
)

// WebhookHandler handles incoming HTTP requests from Telegram webhooks.
type WebhookHandler struct {
	client  *Client
	storage *storage.Storage
	config  *config.Config
}

// NewWebhookHandler creates a new WebhookHandler instance.
func NewWebhookHandler(client *Client, store *storage.Storage, cfg *config.Config) *WebhookHandler {
	return &WebhookHandler{
		client:  client,
		storage: store,
		config:  cfg,
	}
}

// ServeHTTP implements the http.Handler interface.
func (h *WebhookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	log.Printf("[WEBHOOK] Received %s request on path: %s", r.Method, r.URL.Path)

	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var update Update
	if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
		log.Printf("[WEBHOOK] ERROR: failed to decode telegram update: %v", err)
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}

	if update.Message != nil && update.Message.From != nil {
		log.Printf("[WEBHOOK] Processing update from user %d: %s", update.Message.From.ID, update.Message.Text)
	}

	// Always respond with 200 OK as per Webhook best practices
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
	log.Printf("[WEBHOOK] Successful response status code: %d", http.StatusOK)

	// Process update asynchronously
	go h.handleUpdate(&update)
}

func (h *WebhookHandler) handleUpdate(update *Update) {
	if update.Message == nil || update.Message.From == nil {
		return
	}

	msg := update.Message
	user := msg.From
	chatID := msg.Chat.ID

	// Determine preferred language from Telegram user or default to 'en'
	langCode := user.LanguageCode
	if langCode == "" {
		langCode = "en"
	}

	// If /start, save/update user in DB and set default language
	if msg.Text == "/start" {
		if h.storage != nil {
			_, err := h.storage.GetOrCreateUser(user.ID, user.Username, langCode)
			if err != nil {
				log.Printf("[WEBHOOK] Database error getting or creating user %d: %v", user.ID, err)
			} else {
				if err := h.storage.SetUserLanguage(user.ID, langCode); err != nil {
					log.Printf("[WEBHOOK] Database error setting user language for %d: %v", user.ID, err)
				}
			}
		}

		welcome := i18n.T(langCode, "welcome_message")
		chooseLang := i18n.T(langCode, "choose_language")
		responseText := welcome + "\n\n" + chooseLang

		// Include inline keyboard button with web_app
		webAppURL := "http://localhost:8080/app"
		if h.config != nil && h.config.RenderURL != "" {
			webAppURL = h.config.RenderURL + "/app"
		} else {
			webhookURL := os.Getenv("WEBHOOK_URL")
			if webhookURL == "" {
				webhookURL = os.Getenv("RENDER_EXTERNAL_URL")
			}
			if webhookURL == "" {
				webhookURL = os.Getenv("RENDER_URL")
			}
			if webhookURL != "" {
				webAppURL = strings.TrimSuffix(webhookURL, "/") + "/app"
			}
		}
		replyMarkup := &InlineKeyboardMarkup{
			InlineKeyboard: [][]InlineKeyboardButton{
				{
					{
						Text: "📊 Открыть WebApp",
						WebApp: &WebAppInfo{
							URL: webAppURL,
						},
					},
				},
			},
		}

		if err := h.client.SendMessageWithKeyboard(chatID, responseText, replyMarkup); err != nil {
			log.Printf("[WEBHOOK] Error sending message to Telegram API for chat %d: %v", chatID, err)
		}
		return
	}

	// For other messages, fetch user or use tg language code
	currentLang := langCode
	if h.storage != nil {
		dbUser, err := h.storage.GetOrCreateUser(user.ID, user.Username, langCode)
		if err != nil {
			log.Printf("[WEBHOOK] Database error looking up user %d: %v", user.ID, err)
		} else if dbUser != nil && dbUser.Language != "" {
			currentLang = dbUser.Language
		}
	}

	responseText := i18n.T(currentLang, "welcome_message")
	if err := h.client.SendMessage(chatID, responseText); err != nil {
		log.Printf("[WEBHOOK] Error sending message to Telegram API for chat %d: %v", chatID, err)
	}
}
