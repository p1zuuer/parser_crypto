package telegram

import (
	"encoding/json"
	"log"
	"net/http"

	"smart-cluster-bot/internal/i18n"
	"smart-cluster-bot/internal/storage"
)

// WebhookHandler handles incoming HTTP requests from Telegram webhooks.
type WebhookHandler struct {
	client  *Client
	storage *storage.Storage
}

// NewWebhookHandler creates a new WebhookHandler instance.
func NewWebhookHandler(client *Client, store *storage.Storage) *WebhookHandler {
	return &WebhookHandler{
		client:  client,
		storage: store,
	}
}

// ServeHTTP implements the http.Handler interface.
func (h *WebhookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Always respond with 200 OK immediately as per Webhook best practices
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))

	var update Update
	if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
		log.Printf("ERROR: failed to decode telegram update: %v", err)
		return
	}

	// Process update asynchronously or synchronously without holding up response
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
				log.Printf("ERROR: failed to get or create user %d in DB: %v", user.ID, err)
			} else {
				if err := h.storage.SetUserLanguage(user.ID, langCode); err != nil {
					log.Printf("ERROR: failed to set user language for %d: %v", user.ID, err)
				}
			}
		}

		welcome := i18n.T(langCode, "welcome_message")
		chooseLang := i18n.T(langCode, "choose_language")
		responseText := welcome + "\n\n" + chooseLang

		if err := h.client.SendMessage(chatID, responseText); err != nil {
			log.Printf("ERROR: failed to send message to chat %d: %v", chatID, err)
		}
		return
	}

	// For other messages, fetch user or use tg language code
	currentLang := langCode
	if h.storage != nil {
		dbUser, err := h.storage.GetOrCreateUser(user.ID, user.Username, langCode)
		if err == nil && dbUser != nil && dbUser.Language != "" {
			currentLang = dbUser.Language
		}
	}

	responseText := i18n.T(currentLang, "welcome_message")
	if err := h.client.SendMessage(chatID, responseText); err != nil {
		log.Printf("ERROR: failed to send message to chat %d: %v", chatID, err)
	}
}
