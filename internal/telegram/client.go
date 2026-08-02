package telegram

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
)

// Client wraps the net/http client for interacting with the Telegram Bot API.
type Client struct {
	token      string
	httpClient *http.Client
	apiBaseURL string
}

// NewClient creates a new Telegram HTTP client.
func NewClient(token string) *Client {
	cleanToken := strings.TrimSpace(token)
	cleanToken = strings.Trim(cleanToken, `"'`)
	return &Client{
		token: cleanToken,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
		apiBaseURL: fmt.Sprintf("https://api.telegram.org/bot%s", cleanToken),
	}
}

// SendMessage sends a text message to a specific chat ID.
func (c *Client) SendMessage(chatID int64, text string) error {
	return c.SendMessageWithKeyboard(chatID, text, nil)
}

// SendMessageWithKeyboard sends a text message with an optional inline keyboard markup.
func (c *Client) SendMessageWithKeyboard(chatID int64, text string, replyMarkup *InlineKeyboardMarkup) error {
	reqBody := SendMessageRequest{
		ChatID:      chatID,
		Text:        text,
		ParseMode:   "Markdown",
		ReplyMarkup: replyMarkup,
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("failed to marshal send message request: %w", err)
	}

	method := "sendMessage"
	log.Printf("[TELEGRAM API] Sending request to method: %s", method)

	url := c.apiBaseURL + "/" + method
	resp, err := c.httpClient.Post(url, "application/json", bytes.NewBuffer(bodyBytes))
	if err != nil {
		return fmt.Errorf("failed to send http request to telegram: %w", err)
	}
	defer resp.Body.Close()

	var apiResp APIResponse
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return fmt.Errorf("failed to decode telegram response: %w", err)
	}

	if !apiResp.Ok {
		return fmt.Errorf("telegram api error: %s (code: %d)", apiResp.Description, apiResp.ErrorCode)
	}

	return nil
}

// SetChatMenuButton sets the chat menu button for WebApp.
func (c *Client) SetChatMenuButton(webAppURL string) error {
	reqBody := map[string]interface{}{
		"menu_button": map[string]interface{}{
			"type": "web_app",
			"text": "📊 WebApp",
			"web_app": map[string]string{
				"url": webAppURL,
			},
		},
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("failed to marshal set chat menu button request: %w", err)
	}

	url := c.apiBaseURL + "/setChatMenuButton"
	resp, err := c.httpClient.Post(url, "application/json", bytes.NewBuffer(bodyBytes))
	if err != nil {
		return fmt.Errorf("failed to send setChatMenuButton request: %w", err)
	}
	defer resp.Body.Close()

	var apiResp APIResponse
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return fmt.Errorf("failed to decode telegram response: %w", err)
	}

	if !apiResp.Ok {
		return fmt.Errorf("telegram api error: %s (code: %d)", apiResp.Description, apiResp.ErrorCode)
	}

	return nil
}
