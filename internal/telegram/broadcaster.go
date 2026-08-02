package telegram

import (
	"context"
	"fmt"
	"log"

	"smart-cluster-bot/internal/detector"
	"smart-cluster-bot/internal/i18n"
	"smart-cluster-bot/internal/storage"
)

// StartAlertBroadcaster starts a background worker that listens to alerts from the detector engine
// and broadcasts them to all registered users according to their subscription status and language.
func StartAlertBroadcaster(ctx context.Context, client *Client, store *storage.Storage, alertsChan <-chan detector.ClusterAlert) {
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case alert, ok := <-alertsChan:
				if !ok {
					return
				}

				users, err := store.GetAllUsers()
				if err != nil {
					log.Printf("ERROR: failed to get users for alert broadcast: %v", err)
					continue
				}

				for _, user := range users {
					lang := user.Language
					if lang == "" {
						lang = "en"
					}

					var msgText string
					var inlineKeyboard [][]InlineKeyboardButton

					if user.IsSubscribed {
						// VIP User: full token address, symbol, buy stats, unique wallets, volume, time window
						template := i18n.T(lang, "cluster_alert_vip")
						msgText = fmt.Sprintf(template,
							alert.TokenSymbol,
							alert.Chain,
							alert.Chain,
							alert.TokenAddress,
							alert.BuyCount,
							alert.TotalVolumeUSD,
							alert.TimeWindowSeconds,
						)
					} else {
						// Free User: blurred token address (e.g. 0x12...a4f), symbol, volume stats, CTA button
						blurredAddr := blurAddress(alert.TokenAddress)
						template := i18n.T(lang, "cluster_alert_free")
						msgText = fmt.Sprintf(template,
							alert.TokenSymbol,
							alert.Chain,
							alert.Chain,
							blurredAddr,
							alert.TotalVolumeUSD,
						)

						// Add upgrade CTA button
						btnText := i18n.T(lang, "btn_upgrade")
						inlineKeyboard = [][]InlineKeyboardButton{
							{
								{
									Text: btnText,
									URL:  "https://t.me/SmartClusterBot?start=upgrade",
								},
							},
						}
					}

					var replyMarkup *InlineKeyboardMarkup
					if len(inlineKeyboard) > 0 {
						replyMarkup = &InlineKeyboardMarkup{
							InlineKeyboard: inlineKeyboard,
						}
					}

					if err := client.SendMessageWithKeyboard(user.UserID, msgText, replyMarkup); err != nil {
						log.Printf("ERROR: failed to send cluster alert to user %d: %v", user.UserID, err)
					}
				}
			}
		}
	}()
}

// blurAddress masks a crypto address for free users, e.g. 0x1234...abcd -> 0x12...abcd
func blurAddress(addr string) string {
	if len(addr) <= 10 {
		return addr
	}
	return addr[:4] + "..." + addr[len(addr)-4:]
}
