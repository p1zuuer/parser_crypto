// Package trading implements live token execution via Jupiter aggregator API
// and github.com/gagliardetto/solana-go v0.4.x.
package trading

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	bin "github.com/gagliardetto/binary"
	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
)

// AutoBuyer executes a market buy of tokenAddress for approximately
// amountUSD worth of the quote asset.
type AutoBuyer interface {
	BuyToken(ctx context.Context, tokenAddress string, amountUSD float64) error
}

const wrappedSOLMint = "So11111111111111111111111111111111111111112"
const jupiterQuoteURL = "https://quote-api.jup.ag/v6/quote"
const jupiterSwapURL = "https://quote-api.jup.ag/v6/swap"
const lamportsPerSOL = 1_000_000_000
const solUSDFallbackPrice = 150.0

// JupiterBuyer implements AutoBuyer via Jupiter v6 + solana-go v0.4.x.
type JupiterBuyer struct {
	wallet     solana.PrivateKey
	rpcClient  *rpc.Client
	httpClient *http.Client
}

// NewJupiterBuyer parses a base58-encoded Solana private key and returns a
// ready-to-use JupiterBuyer.
func NewJupiterBuyer(base58PrivateKey, rpcURL string) (*JupiterBuyer, error) {
	if base58PrivateKey == "" {
		return nil, fmt.Errorf("trading: SOL_PRIVATE_KEY is empty")
	}
	wallet, err := solana.PrivateKeyFromBase58(base58PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("trading: parse private key: %w", err)
	}
	if rpcURL == "" {
		rpcURL = rpc.MainNetBeta_RPC
	}
	return &JupiterBuyer{
		wallet:     wallet,
		rpcClient:  rpc.New(rpcURL),
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}, nil
}

// swapResponse is the subset of Jupiter's /swap response we need.
type swapResponse struct {
	SwapTransaction string `json:"swapTransaction"`
}

// BuyToken satisfies the AutoBuyer interface.
func (b *JupiterBuyer) BuyToken(ctx context.Context, tokenAddress string, amountUSD float64) error {
	_, err := b.BuyTokenWithResult(ctx, tokenAddress, amountUSD)
	return err
}

// BuyTokenWithResult quotes, builds, signs, and broadcasts a Jupiter swap,
// returning the transaction signature on success.
func (b *JupiterBuyer) BuyTokenWithResult(ctx context.Context, tokenAddress string, amountUSD float64) (string, error) {
	if b == nil {
		return "", fmt.Errorf("trading: buyer not configured")
	}
	lamports := usdToLamports(amountUSD, solUSDFallbackPrice)
	if lamports <= 0 {
		return "", fmt.Errorf("trading: computed zero lamports for $%.2f", amountUSD)
	}
	rawQuote, err := b.getQuote(ctx, tokenAddress, lamports)
	if err != nil {
		return "", fmt.Errorf("trading: get quote: %w", err)
	}
	swapTxBase64, err := b.getSwapTransaction(ctx, rawQuote)
	if err != nil {
		return "", fmt.Errorf("trading: build swap tx: %w", err)
	}
	sig, err := b.signAndBroadcast(ctx, swapTxBase64)
	if err != nil {
		return "", fmt.Errorf("trading: sign/broadcast: %w", err)
	}
	return sig, nil
}

// getQuote calls GET /v6/quote and returns the raw JSON body.
// Jupiter's /swap endpoint expects the complete unmodified quote, so we
// deliberately avoid re-marshalling a partially-typed struct.
func (b *JupiterBuyer) getQuote(ctx context.Context, tokenAddress string, lamports int64) (json.RawMessage, error) {
	url := fmt.Sprintf(
		"%s?inputMint=%s&outputMint=%s&amount=%d&slippageBps=200",
		jupiterQuoteURL, wrappedSOLMint, tokenAddress, lamports,
	)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := b.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("jupiter quote HTTP %d", resp.StatusCode)
	}
	buf := new(bytes.Buffer)
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		return nil, err
	}
	if !json.Valid(buf.Bytes()) {
		return nil, fmt.Errorf("jupiter quote: invalid JSON")
	}
	return json.RawMessage(buf.Bytes()), nil
}

// getSwapTransaction calls POST /v6/swap with the raw quote and our public
// key, returning the base64-encoded unsigned transaction.
func (b *JupiterBuyer) getSwapTransaction(ctx context.Context, rawQuote json.RawMessage) (string, error) {
	body := map[string]interface{}{
		"quoteResponse":             rawQuote,
		"userPublicKey":             b.wallet.PublicKey().String(),
		"wrapAndUnwrapSol":          true,
		"dynamicComputeUnitLimit":   true,
		"prioritizationFeeLamports": "auto",
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, jupiterSwapURL, bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := b.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("jupiter swap HTTP %d", resp.StatusCode)
	}
	var sw swapResponse
	if err := json.NewDecoder(resp.Body).Decode(&sw); err != nil {
		return "", fmt.Errorf("decode swap response: %w", err)
	}
	if sw.SwapTransaction == "" {
		return "", fmt.Errorf("empty swapTransaction in response")
	}
	return sw.SwapTransaction, nil
}

// signAndBroadcast decodes the base64 transaction from Jupiter, signs it
// with our wallet, and broadcasts it via RPC. Uses solana-go v0.4.x API:
// UnmarshalWithDecoder (bin.NewBinDecoder) instead of the newer
// TransactionFromBase64 / TransactionFromBytes helpers that don't exist
// in this version.
func (b *JupiterBuyer) signAndBroadcast(ctx context.Context, txBase64 string) (string, error) {
	txBytes, err := base64.StdEncoding.DecodeString(txBase64)
	if err != nil {
		return "", fmt.Errorf("base64 decode tx: %w", err)
	}

	tx := &solana.Transaction{}
	decoder := bin.NewBinDecoder(txBytes)
	if err := tx.UnmarshalWithDecoder(decoder); err != nil {
		return "", fmt.Errorf("unmarshal tx: %w", err)
	}

	// Sign — the getter is called once per required signer public key.
	_, err = tx.Sign(func(key solana.PublicKey) *solana.PrivateKey {
		if key.Equals(b.wallet.PublicKey()) {
			return &b.wallet
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("sign tx: %w", err)
	}

	// SendTransactionWithOpts in v0.4.x takes (ctx, tx, skipPreflight bool, commitment string)
	// — NOT a TransactionOpts struct like in newer versions.
	sig, err := b.rpcClient.SendTransaction(ctx, tx)
	if err != nil {
		return "", fmt.Errorf("broadcast tx: %w", err)
	}
	return sig.String(), nil
}

// usdToLamports converts a USD trade size into lamports using solPriceUSD.
func usdToLamports(amountUSD, solPriceUSD float64) int64 {
	if solPriceUSD <= 0 {
		return 0
	}
	return int64((amountUSD / solPriceUSD) * lamportsPerSOL)
}

// NoopBuyer is a no-op AutoBuyer used when auto-buy is disabled.
type NoopBuyer struct{}

func (NoopBuyer) BuyToken(ctx context.Context, tokenAddress string, amountUSD float64) error {
	return fmt.Errorf("trading: auto-buy disabled")
}
