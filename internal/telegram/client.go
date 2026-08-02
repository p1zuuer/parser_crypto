// Package telegram provides a minimal Telegram Bot API client and webhook
// handler built entirely on the Go standard library.
package telegram

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const apiBase = "https://api.telegram.org"

// Client is a minimal Telegram Bot API client.
type Client struct {
	token      string
	baseURL    string // overridable for tests
	httpClient *http.Client
}

// NewClient creates a Client for the given bot token.
func NewClient(token string) *Client {
	token = strings.TrimSpace(strings.Trim(token, `"'`))
	return &Client{
		token:   token,
		baseURL: apiBase,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

// NewClientWithBaseURL creates a Client pointed at a custom base URL (useful
// for tests with a fake HTTP server).
func NewClientWithBaseURL(baseURL, token string) *Client {
	c := NewClient(token)
	c.baseURL = baseURL
	return c
}

// SendPhotoRequest is the body sent to sendPhoto.
type SendPhotoRequest struct {
	ChatID      int64                 `json:"chat_id"`
	Photo       string                `json:"photo"`
	Caption     string                `json:"caption,omitempty"`
	ParseMode   string                `json:"parse_mode,omitempty"`
	ReplyMarkup *InlineKeyboardMarkup `json:"reply_markup,omitempty"`
}

// SendPhoto sends a photo with an optional caption and inline keyboard (fixes squished UI).
func (c *Client) SendPhoto(chatID int64, photoURL, caption string, kb *InlineKeyboardMarkup) error {
	return c.post("sendPhoto", SendPhotoRequest{
		ChatID:      chatID,
		Photo:       photoURL,
		Caption:     caption,
		ParseMode:   "HTML",
		ReplyMarkup: kb,
	})
}

// SendMessage sends a plain-text (Markdown) message to chatID.
func (c *Client) SendMessage(chatID int64, text string) error {
	return c.SendMessageWithKeyboard(chatID, text, nil)
}

// SendMessageWithKeyboard sends a message with an optional inline keyboard.
func (c *Client) SendMessageWithKeyboard(chatID int64, text string, kb *InlineKeyboardMarkup) error {
	return c.post("sendMessage", SendMessageRequest{
		ChatID:      chatID,
		Text:        text,
		ParseMode:   "HTML",
		ReplyMarkup: kb,
	})
}

// EditMessageText replaces the text (and keyboard) of an already-sent message.
func (c *Client) EditMessageText(chatID int64, messageID int, text string, kb *InlineKeyboardMarkup) error {
	return c.post("editMessageText", EditMessageTextRequest{
		ChatID:      chatID,
		MessageID:   messageID,
		Text:        text,
		ParseMode:   "HTML",
		ReplyMarkup: kb,
	})
}

// AnswerCallbackQuery acknowledges a button tap (removes the spinner in the app).
func (c *Client) AnswerCallbackQuery(queryID, text string) error {
	return c.post("answerCallbackQuery", AnswerCallbackQueryRequest{
		CallbackQueryID: queryID,
		Text:            text,
	})
}

// SetWebhook registers webhookURL as the target for Telegram updates.
func (c *Client) SetWebhook(webhookURL string) error {
	ep := fmt.Sprintf("%s/bot%s/setWebhook?url=%s",
		c.baseURL, c.token, url.QueryEscape(webhookURL))
	resp, err := c.httpClient.Get(ep)
	if err != nil {
		return fmt.Errorf("telegram: setWebhook: %w", err)
	}
	defer resp.Body.Close()
	return c.checkResponse(resp.Body)
}

// SetChatMenuButton configures the persistent menu button to open the WebApp.
func (c *Client) SetChatMenuButton(webAppURL string) error {
	return c.post("setChatMenuButton", map[string]interface{}{
		"menu_button": map[string]interface{}{
			"type":    "web_app",
			"text":    "📊 Terminal",
			"web_app": map[string]string{"url": webAppURL},
		},
	})
}

// ── Internals ──────────────────────────────────────────────────────────────────

// post marshals body, POSTs it to the named Telegram method, and checks the
// response envelope.
func (c *Client) post(method string, body interface{}) error {
	data, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("telegram: marshal %s: %w", method, err)
	}

	ep := fmt.Sprintf("%s/bot%s/%s", c.baseURL, c.token, method)
	log.Printf("[TELEGRAM] → %s", method)

	resp, err := c.httpClient.Post(ep, "application/json", bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("telegram: POST %s: %w", method, err)
	}
	defer resp.Body.Close()
	return c.checkResponse(resp.Body)
}

func (c *Client) checkResponse(body io.Reader) error {
	var r APIResponse
	if err := json.NewDecoder(body).Decode(&r); err != nil {
		return fmt.Errorf("telegram: decode response: %w", err)
	}
	if !r.Ok {
		return fmt.Errorf("telegram API error %d: %s", r.ErrorCode, r.Description)
	}
	return nil
}
