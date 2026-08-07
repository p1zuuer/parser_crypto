package storage

import (
	"fmt"
	"time"
)

// Position tracks a single auto-buy trade from entry through exit.
type Position struct {
	ID           int64
	TokenAddress string
	TokenSymbol  string
	Chain        string
	EntryTxHash  string
	EntryTimeUTC time.Time
	BuyAmountUSD float64
	// EntryPriceUSD is the USD price per token at time of buy. Populated by
	// the seller goroutine once the entry tx is confirmed on-chain.
	EntryPriceUSD float64
	// TakeProfitPct is the % gain at which we sell 50% of the position.
	// Default: 50 (sell half at +50%).
	TakeProfitPct float64
	// StopLossPct is the % loss at which we sell 100% of the position.
	// Default: 15 (full exit at -15%).
	StopLossPct float64
	// Status: "open", "tp_partial" (TP hit, 50% sold), "closed_tp", "closed_sl", "closed_manual"
	Status      string
	ExitTxHash  string
	ExitTimeUTC *time.Time
	PnLUSD      float64
}

const positionsSchema = `
CREATE TABLE IF NOT EXISTS positions (
	id               INTEGER  PRIMARY KEY AUTOINCREMENT,
	token_address    TEXT     NOT NULL,
	token_symbol     TEXT     NOT NULL DEFAULT '',
	chain            TEXT     NOT NULL DEFAULT 'Solana',
	entry_tx_hash    TEXT     NOT NULL DEFAULT '',
	entry_time_utc   DATETIME NOT NULL,
	buy_amount_usd   REAL     NOT NULL DEFAULT 0,
	entry_price_usd  REAL     NOT NULL DEFAULT 0,
	take_profit_pct  REAL     NOT NULL DEFAULT 50,
	stop_loss_pct    REAL     NOT NULL DEFAULT 15,
	status           TEXT     NOT NULL DEFAULT 'open',
	exit_tx_hash     TEXT     NOT NULL DEFAULT '',
	exit_time_utc    DATETIME,
	pnl_usd          REAL     NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_positions_status ON positions(status);
CREATE INDEX IF NOT EXISTS idx_positions_token  ON positions(token_address);
`

// migratePositions adds the positions table if it doesn't already exist.
// Called from InitDB after the main schema runs.
func (s *Storage) migratePositions() error {
	if _, err := s.db.Exec(positionsSchema); err != nil {
		return fmt.Errorf("storage: migrate positions: %w", err)
	}
	return nil
}

// OpenSimulatedPosition records a paper trade. Identical to OpenPosition
// but forces status = 'simulated' so the seller can track TP/SL with real
// prices while never touching the RPC layer.
func (s *Storage) OpenSimulatedPosition(tokenAddress, tokenSymbol, chain string,
	buyAmountUSD, entryPriceUSD, takeProfitPct, stopLossPct float64) (int64, error) {

	res, err := s.db.Exec(
		`INSERT INTO positions
		 (token_address, token_symbol, chain, entry_tx_hash, entry_time_utc,
		  buy_amount_usd, entry_price_usd, take_profit_pct, stop_loss_pct, status)
		 VALUES (?, ?, ?, 'simulation', ?, ?, ?, ?, ?, 'simulated')`,
		tokenAddress, tokenSymbol, chain, time.Now().UTC(),
		buyAmountUSD, entryPriceUSD, takeProfitPct, stopLossPct,
	)
	if err != nil {
		return 0, fmt.Errorf("storage: open simulated position: %w", err)
	}
	id, _ := res.LastInsertId()
	return id, nil
}

// OpenPosition records a newly executed buy. entryPriceUSD may be 0 if the
// fill price isn't available yet — the seller will populate it later.
func (s *Storage) OpenPosition(tokenAddress, tokenSymbol, chain, entryTxHash string,
	buyAmountUSD, entryPriceUSD, takeProfitPct, stopLossPct float64) (int64, error) {

	res, err := s.db.Exec(
		`INSERT INTO positions
		 (token_address, token_symbol, chain, entry_tx_hash, entry_time_utc,
		  buy_amount_usd, entry_price_usd, take_profit_pct, stop_loss_pct, status)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'open')`,
		tokenAddress, tokenSymbol, chain, entryTxHash, time.Now().UTC(),
		buyAmountUSD, entryPriceUSD, takeProfitPct, stopLossPct,
	)
	if err != nil {
		return 0, fmt.Errorf("storage: open position: %w", err)
	}
	id, _ := res.LastInsertId()
	return id, nil
}

// UpdatePositionPrice sets the confirmed entry price for a position (called
// once the on-chain fill is confirmed and the price is known).
func (s *Storage) UpdatePositionPrice(id int64, entryPriceUSD float64) error {
	_, err := s.db.Exec(
		`UPDATE positions SET entry_price_usd = ? WHERE id = ?`,
		entryPriceUSD, id,
	)
	return err
}

// MarkTPPartial records that the take-profit trigger fired and 50% was sold.
func (s *Storage) MarkTPPartial(id int64, exitTxHash string, pnlUSD float64) error {
	_, err := s.db.Exec(
		`UPDATE positions SET status = 'tp_partial', exit_tx_hash = ?, pnl_usd = ? WHERE id = ?`,
		exitTxHash, pnlUSD, id,
	)
	return err
}

// ClosePosition marks a position as fully closed with a reason and final PnL.
func (s *Storage) ClosePosition(id int64, reason, exitTxHash string, pnlUSD float64) error {
	now := time.Now().UTC()
	_, err := s.db.Exec(
		`UPDATE positions
		 SET status = ?, exit_tx_hash = ?, exit_time_utc = ?, pnl_usd = ?
		 WHERE id = ?`,
		"closed_"+reason, exitTxHash, now, pnlUSD, id,
	)
	return err
}

// GetOpenPositions returns all positions not yet fully closed.
func (s *Storage) GetOpenPositions() ([]Position, error) {
	rows, err := s.db.Query(
		`SELECT id, token_address, token_symbol, chain, entry_tx_hash,
		        entry_time_utc, buy_amount_usd, entry_price_usd,
		        take_profit_pct, stop_loss_pct, status, exit_tx_hash,
		        exit_time_utc, pnl_usd
		 FROM positions
		 WHERE status IN ('open', 'tp_partial', 'simulated')
		 ORDER BY entry_time_utc DESC`,
	)
	if err != nil {
		return nil, fmt.Errorf("storage: get open positions: %w", err)
	}
	defer rows.Close()

	var out []Position
	for rows.Next() {
		var p Position
		var entryStr string
		var exitStr *string
		if err := rows.Scan(
			&p.ID, &p.TokenAddress, &p.TokenSymbol, &p.Chain, &p.EntryTxHash,
			&entryStr, &p.BuyAmountUSD, &p.EntryPriceUSD,
			&p.TakeProfitPct, &p.StopLossPct, &p.Status, &p.ExitTxHash,
			&exitStr, &p.PnLUSD,
		); err != nil {
			return nil, fmt.Errorf("storage: scan position: %w", err)
		}
		p.EntryTimeUTC = parseTime(entryStr)
		if exitStr != nil {
			t := parseTime(*exitStr)
			p.ExitTimeUTC = &t
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetRecentClosedPositions returns the last N closed positions for the daily digest.
func (s *Storage) GetRecentClosedPositions(limit int) ([]Position, error) {
	if limit <= 0 {
		limit = 10
	}
	rows, err := s.db.Query(
		`SELECT id, token_address, token_symbol, chain, entry_tx_hash,
		        entry_time_utc, buy_amount_usd, entry_price_usd,
		        take_profit_pct, stop_loss_pct, status, exit_tx_hash,
		        exit_time_utc, pnl_usd
		 FROM positions
		 WHERE status LIKE 'closed_%'
		 ORDER BY exit_time_utc DESC LIMIT ?`,
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("storage: get closed positions: %w", err)
	}
	defer rows.Close()

	var out []Position
	for rows.Next() {
		var p Position
		var entryStr string
		var exitStr *string
		if err := rows.Scan(
			&p.ID, &p.TokenAddress, &p.TokenSymbol, &p.Chain, &p.EntryTxHash,
			&entryStr, &p.BuyAmountUSD, &p.EntryPriceUSD,
			&p.TakeProfitPct, &p.StopLossPct, &p.Status, &p.ExitTxHash,
			&exitStr, &p.PnLUSD,
		); err != nil {
			return nil, fmt.Errorf("storage: scan closed position: %w", err)
		}
		p.EntryTimeUTC = parseTime(entryStr)
		if exitStr != nil {
			t := parseTime(*exitStr)
			p.ExitTimeUTC = &t
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
