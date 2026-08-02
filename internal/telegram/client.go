package telegram

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
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
	return &Client{
		token: token,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
		apiBaseURL: "https://api.telegram.org/bot" + token,
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

	url := c.apiBaseURL + "/sendMessage"
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
