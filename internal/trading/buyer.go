// Package trading implements live token execution. The current backend
// (JupiterBuyer) routes swaps through Jupiter's aggregator API and signs/
// broadcasts the resulting transaction with a local Solana keypair.
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
// amountUSD worth of the quote asset. Implementations are responsible for
// slippage protection, fee handling, and honouring ctx's deadline.
type AutoBuyer interface {
	BuyToken(ctx context.Context, tokenAddress string, amountUSD float64) error
}

// wrappedSOLMint is Jupiter/Solana's canonical wrapped-SOL mint address —
// the standard "inputMint" for SOL-denominated swaps.
const wrappedSOLMint = "So11111111111111111111111111111111111111112"

const jupiterQuoteURL = "https://quote-api.jup.ag/v6/quote"
const jupiterSwapURL = "https://quote-api.jup.ag/v6/swap"

// lamportsPerSOL is the fixed Solana unit conversion (1 SOL = 1e9 lamports).
const lamportsPerSOL = 1_000_000_000

// solUSDFallbackPrice is used only if a live price cannot be fetched. Keeping
// this current matters: an out-of-date price directly changes trade size.
// Prefer wiring a real price feed (Pyth/Birdeye/Jupiter price API) in
// production; this constant exists purely so auto-buy never hard-fails
// because a price oracle call failed.
const solUSDFallbackPrice = 150.0

// JupiterBuyer implements AutoBuyer via Jupiter's swap aggregator API,
// signing the returned transaction locally with a solana-go keypair before
// broadcasting it through the configured RPC endpoint.
type JupiterBuyer struct {
	wallet     solana.PrivateKey
	rpcClient  *rpc.Client
	httpClient *http.Client
}

// NewJupiterBuyer parses a base58-encoded Solana private key and constructs
// a ready-to-use JupiterBuyer. Returns an error if the key is malformed.
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
	SwapTransaction string `json:"swapTransaction"` // base64-encoded serialized tx
}

// BuyToken quotes, builds, signs, and broadcasts a Jupiter swap from wrapped
// SOL into tokenAddress for approximately amountUSD worth of SOL. Satisfies
// the AutoBuyer interface (result discarded — use BuyTokenWithResult to
// retrieve the transaction signature).
func (b *JupiterBuyer) BuyToken(ctx context.Context, tokenAddress string, amountUSD float64) error {
	_, err := b.BuyTokenWithResult(ctx, tokenAddress, amountUSD)
	return err
}

// BuyTokenWithResult quotes, builds, signs, and broadcasts a Jupiter swap,
// returning the transaction signature (hash) on success.
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

// getQuote calls GET /v6/quote for a wSOL → tokenAddress swap and returns
// the raw JSON body untouched — Jupiter's /swap endpoint expects the
// complete, unmodified quote object forwarded verbatim, so we deliberately
// avoid re-marshalling a partially-typed struct.
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
		return nil, fmt.Errorf("jupiter quote returned invalid JSON")
	}
	return json.RawMessage(buf.Bytes()), nil
}

// getSwapTransaction calls POST /v6/swap with the raw quote and our public
// key, returning the base64-encoded unsigned (fee-payer-only) transaction.
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

// signAndBroadcast deserializes the base64 transaction Jupiter returned,
// signs it with our wallet, and submits it via the configured RPC endpoint.
func (b *JupiterBuyer) signAndBroadcast(ctx context.Context, txBase64 string) (string, error) {
	rawTx, err := base64.StdEncoding.DecodeString(txBase64)
	if err != nil {
		return "", fmt.Errorf("decode base64 tx: %w", err)
	}

	tx, err := solana.TransactionFromDecoder(bin.NewBinDecoder(rawTx))
	if err != nil {
		return "", fmt.Errorf("decode tx: %w", err)
	}

	_, err = tx.Sign(func(key solana.PublicKey) *solana.PrivateKey {
		if key.Equals(b.wallet.PublicKey()) {
			return &b.wallet
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("sign tx: %w", err)
	}

	sig, err := b.rpcClient.SendTransactionWithOpts(ctx, tx, rpc.TransactionOpts{
		SkipPreflight:       false,
		PreflightCommitment: rpc.CommitmentConfirmed,
	})
	if err != nil {
		return "", fmt.Errorf("broadcast tx: %w", err)
	}
	return sig.String(), nil
}

// usdToLamports converts a USD trade size into lamports of wrapped SOL using
// solPriceUSD as the conversion rate.
func usdToLamports(amountUSD, solPriceUSD float64) int64 {
	if solPriceUSD <= 0 {
		return 0
	}
	sol := amountUSD / solPriceUSD
	return int64(sol * lamportsPerSOL)
}

// NoopBuyer is a placeholder AutoBuyer that performs no on-chain action.
// Used automatically when SOL_PRIVATE_KEY is unset or AUTO_BUY_ENABLED is
// false, so the rest of the codebase never has to nil-check the buyer.
type NoopBuyer struct{}

// BuyToken always returns an error indicating auto-buy is disabled.
func (NoopBuyer) BuyToken(ctx context.Context, tokenAddress string, amountUSD float64) error {
	return fmt.Errorf("trading: auto-buy disabled (no SOL_PRIVATE_KEY / AUTO_BUY_ENABLED=false)")
}
