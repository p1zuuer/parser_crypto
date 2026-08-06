package trading

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"smart-cluster-bot/internal/storage"
)

// Default TP/SL parameters. These match what the user specified and can be
// overridden per-position if needed later.
const (
	DefaultTakeProfitPct = 50.0 // sell 50% of position at +50% gain
	DefaultStopLossPct   = 15.0 // sell 100% of position at -15% loss
)

// priceCheckInterval controls how often the position monitor polls Jupiter
// for current prices. 30 seconds is a reasonable balance between latency
// and API politeness for a personal bot.
const priceCheckInterval = 30 * time.Second

// jupiterPriceURL returns the Jupiter price API URL for a token.
// v2 price API, free, no auth required.
func jupiterPriceURL(tokenAddress string) string {
	return fmt.Sprintf("https://api.jup.ag/price/v2?ids=%s", tokenAddress)
}

// jupiterSellQuoteURL builds the quote URL for selling tokenAddress back to SOL.
func jupiterSellQuoteURL(tokenAddress string, tokenAmount uint64) string {
	return fmt.Sprintf(
		"https://quote-api.jup.ag/v6/quote?inputMint=%s&outputMint=%s&amount=%d&slippageBps=300",
		tokenAddress, wrappedSOLMint, tokenAmount,
	)
}

// PositionStore is the storage interface the seller needs. Keeps seller
// decoupled from the concrete *storage.Storage type.
type PositionStore interface {
	GetOpenPositions() ([]storage.Position, error)
	UpdatePositionPrice(id int64, entryPriceUSD float64) error
	MarkTPPartial(id int64, exitTxHash string, pnlUSD float64) error
	ClosePosition(id int64, reason, exitTxHash string, pnlUSD float64) error
	OpenPosition(tokenAddress, tokenSymbol, chain, entryTxHash string,
		buyAmountUSD, entryPriceUSD, takeProfitPct, stopLossPct float64) (int64, error)
}

// Seller monitors open positions and executes take-profit / stop-loss sells
// automatically. It runs as a background goroutine and is safe to construct
// even when auto-buy is disabled (it simply won't find any positions to monitor).
type Seller struct {
	buyer      *JupiterBuyer // reused for sell execution (same keypair/RPC)
	store      PositionStore
	httpClient *http.Client
	notify     func(msg string) // sends result to Telegram
}

// NewSeller constructs a Seller. buyer may be nil — if so, Seller.Start
// still runs but will log errors rather than executing any transactions.
func NewSeller(buyer *JupiterBuyer, store PositionStore, notify func(msg string)) *Seller {
	return &Seller{
		buyer:      buyer,
		store:      store,
		httpClient: &http.Client{Timeout: 8 * time.Second},
		notify:     notify,
	}
}

// Start launches the position monitor goroutine. Wrapped in panic recovery.
func (s *Seller) Start(ctx context.Context) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[SELLER] panic recovered: %v", r)
			}
		}()

		ticker := time.NewTicker(priceCheckInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.checkPositions(ctx)
			}
		}
	}()
}

// RecordBuy creates a position record after a successful auto-buy. Call this
// immediately after JupiterBuyer.BuyTokenWithResult returns a tx signature.
func (s *Seller) RecordBuy(ctx context.Context, tokenAddress, tokenSymbol, chain, txHash string, amountUSD float64) {
	// Fetch current price to use as entry price
	entryPrice, err := s.fetchPriceUSD(ctx, tokenAddress)
	if err != nil {
		log.Printf("[SELLER] could not fetch entry price for %s: %v", tokenAddress, err)
		entryPrice = 0 // will be left as 0; seller can still track on % basis
	}

	id, err := s.store.OpenPosition(
		tokenAddress, tokenSymbol, chain, txHash,
		amountUSD, entryPrice,
		DefaultTakeProfitPct, DefaultStopLossPct,
	)
	if err != nil {
		log.Printf("[SELLER] OpenPosition %s: %v", tokenAddress, err)
		return
	}
	log.Printf("[SELLER] position %d opened: %s @ $%.6f", id, tokenAddress, entryPrice)
}

// checkPositions fetches all open positions, gets current prices, and fires
// TP or SL sells where thresholds are crossed.
func (s *Seller) checkPositions(ctx context.Context) {
	positions, err := s.store.GetOpenPositions()
	if err != nil {
		log.Printf("[SELLER] GetOpenPositions: %v", err)
		return
	}
	if len(positions) == 0 {
		return
	}

	for _, p := range positions {
		if ctx.Err() != nil {
			return
		}
		s.checkOne(ctx, p)
		// Polite delay between price checks
		select {
		case <-ctx.Done():
			return
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func (s *Seller) checkOne(ctx context.Context, p storage.Position) {
	if p.EntryPriceUSD <= 0 {
		// Try to populate entry price if it was missing at open time
		price, err := s.fetchPriceUSD(ctx, p.TokenAddress)
		if err != nil || price <= 0 {
			return
		}
		_ = s.store.UpdatePositionPrice(p.ID, price)
		p.EntryPriceUSD = price
		return // wait for next tick to evaluate — don't sell based on entry price
	}

	currentPrice, err := s.fetchPriceUSD(ctx, p.TokenAddress)
	if err != nil {
		log.Printf("[SELLER] price check %s: %v", p.TokenAddress, err)
		return
	}
	if currentPrice <= 0 {
		return
	}

	pctChange := ((currentPrice - p.EntryPriceUSD) / p.EntryPriceUSD) * 100.0

	switch p.Status {
	case "open":
		if pctChange >= p.TakeProfitPct {
			// TP hit: sell 50%, keep the rest running (free-ride)
			s.executeSell(ctx, p, "tp", 50, currentPrice, pctChange)
		} else if pctChange <= -p.StopLossPct {
			// SL hit: sell 100% to limit losses
			s.executeSell(ctx, p, "sl", 100, currentPrice, pctChange)
		}

	case "tp_partial":
		// First TP already taken. Now either ride to full close at 2× entry
		// or cut at -30% from entry (tighter SL on the remaining half).
		fullTakeProfit := p.TakeProfitPct * 2 // e.g. 100% for a 50% TP setting
		remainingSL := p.StopLossPct * 2      // e.g. 30% for a 15% SL setting

		if pctChange >= fullTakeProfit {
			s.executeSell(ctx, p, "tp", 100, currentPrice, pctChange)
		} else if pctChange <= -remainingSL {
			s.executeSell(ctx, p, "sl", 100, currentPrice, pctChange)
		}
	}
}

// executeSell sells sellPct% of the position, records the result, and
// notifies the admin. sellPct should be 50 or 100.
func (s *Seller) executeSell(ctx context.Context, p storage.Position, reason string, sellPct int, currentPrice, pctChange float64) {
	if s.buyer == nil {
		log.Printf("[SELLER] no buyer configured — cannot execute sell for position %d", p.ID)
		return
	}

	// Estimate how many tokens we hold. This is approximate — a production
	// system should read the actual token balance from the RPC node.
	// Here: tokenQty = buyAmountUSD / entryPriceUSD * (sellPct/100)
	tokenQtyUSD := p.BuyAmountUSD * float64(sellPct) / 100.0

	// For the TP partial, PnL on the sold portion:
	realizedPnL := tokenQtyUSD * (pctChange / 100.0)

	txHash, err := s.executeSellTx(ctx, p.TokenAddress, tokenQtyUSD, currentPrice)
	if err != nil {
		log.Printf("[SELLER] executeSellTx %s: %v", p.TokenAddress, err)
		s.notify(fmt.Sprintf(
			"SELL FAILED: %s\nReason: %s (%.1f%%)\nError: %v",
			p.TokenSymbol, strings.ToUpper(reason), pctChange, err,
		))
		return
	}

	var dbErr error
	if sellPct == 50 {
		dbErr = s.store.MarkTPPartial(p.ID, txHash, realizedPnL)
	} else {
		dbErr = s.store.ClosePosition(p.ID, reason, txHash, realizedPnL)
	}
	if dbErr != nil {
		log.Printf("[SELLER] update position %d: %v", p.ID, dbErr)
	}

	emoji := "🟢"
	if reason == "sl" {
		emoji = "🔴"
	}
	action := "TAKE PROFIT"
	if reason == "sl" {
		action = "STOP LOSS"
	}

	s.notify(fmt.Sprintf(
		"%s %s TRIGGERED\n\n"+
			"Token: %s\n"+
			"Change: %+.1f%%\n"+
			"Sold: %d%% of position\n"+
			"Realized PnL: $%.2f\n"+
			"Tx: %s",
		emoji, action,
		p.TokenSymbol,
		pctChange,
		sellPct,
		realizedPnL,
		txHash,
	))

	log.Printf("[SELLER] %s executed for %s: %.1f%% change, tx %s", action, p.TokenSymbol, pctChange, txHash)
}

// executeSellTx gets a Jupiter sell quote and executes it. tokenQtyUSD is
// the dollar value to sell; we derive token lamports from current price.
func (s *Seller) executeSellTx(ctx context.Context, tokenAddress string, tokenQtyUSD, currentPrice float64) (string, error) {
	if currentPrice <= 0 {
		return "", fmt.Errorf("invalid current price")
	}

	// Convert USD → token units (raw, 9 decimals assumed for most Solana tokens)
	tokenUnits := uint64((tokenQtyUSD / currentPrice) * 1e9)
	if tokenUnits == 0 {
		return "", fmt.Errorf("computed zero token units for $%.4f", tokenQtyUSD)
	}

	rawQuote, err := s.fetchSellQuote(ctx, tokenAddress, tokenUnits)
	if err != nil {
		return "", fmt.Errorf("sell quote: %w", err)
	}

	swapTxBase64, err := s.buyer.getSwapTransaction(ctx, rawQuote)
	if err != nil {
		return "", fmt.Errorf("sell swap tx: %w", err)
	}

	sig, err := s.buyer.signAndBroadcast(ctx, swapTxBase64)
	if err != nil {
		return "", fmt.Errorf("sell broadcast: %w", err)
	}
	return sig, nil
}

func (s *Seller) fetchSellQuote(ctx context.Context, tokenAddress string, tokenUnits uint64) (json.RawMessage, error) {
	url := jupiterSellQuoteURL(tokenAddress, tokenUnits)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("jupiter sell quote HTTP %d", resp.StatusCode)
	}

	buf := new(bytes.Buffer)
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		return nil, err
	}
	if !json.Valid(buf.Bytes()) {
		return nil, fmt.Errorf("invalid JSON from jupiter sell quote")
	}
	return json.RawMessage(buf.Bytes()), nil
}

// fetchPriceUSD queries Jupiter's price API for the current USD price of
// tokenAddress. Returns 0 on failure rather than erroring — callers should
// treat 0 as "price unavailable" and skip the position check for this tick.
func (s *Seller) fetchPriceUSD(ctx context.Context, tokenAddress string) (float64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, jupiterPriceURL(tokenAddress), nil)
	if err != nil {
		return 0, err
	}

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("jupiter price HTTP %d", resp.StatusCode)
	}

	var result struct {
		Data map[string]struct {
			Price float64 `json:"price"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return 0, fmt.Errorf("decode price: %w", err)
	}

	if entry, ok := result.Data[tokenAddress]; ok {
		return entry.Price, nil
	}
	return 0, fmt.Errorf("token %s not found in price response", tokenAddress)
}
