// Package storage provides SQLite-backed persistence for users, watchlists,
// and cluster alert history.
package storage

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// ── Types ──────────────────────────────────────────────────────────────────────

// User represents a registered bot user with their preferences.
type User struct {
	UserID      int64     `db:"user_id"`
	Username    string    `db:"username"`
	Language    string    `db:"language"`
	IsVIP       bool      `db:"is_vip"`
	MinVolume   int       `db:"min_volume"`
	EthEnabled  bool      `db:"eth_enabled"`
	SolEnabled  bool      `db:"sol_enabled"`
	BaseEnabled bool      `db:"base_enabled"`
	BscEnabled  bool      `db:"bsc_enabled"`
	CreatedAt   time.Time `db:"created_at"`
}

// WatchlistEntry is a single wallet tracked for a user.
type WatchlistEntry struct {
	ID            int64     `db:"id"`
	UserID        int64     `db:"user_id"`
	WalletAddress string    `db:"wallet_address"`
	Note          string    `db:"note"`
	CreatedAt     time.Time `db:"created_at"`
}

// ClusterRecord is a single detected smart-money accumulation event.
type ClusterRecord struct {
	ID                int64     `json:"id"               db:"id"`
	TokenAddress      string    `json:"TokenAddress"     db:"token_address"`
	TokenSymbol       string    `json:"TokenSymbol"      db:"token_symbol"`
	Chain             string    `json:"Chain"            db:"chain"`
	BuyCount          int       `json:"BuyCount"         db:"buy_count"`
	TotalVolumeUSD    float64   `json:"TotalVolumeUSD"   db:"total_volume_usd"`
	TimeWindowSeconds int       `json:"TimeWindowSeconds" db:"time_window_seconds"`
	WalletAddress     string    `json:"WalletAddress"    db:"wallet_address"`
	CreatedAt         time.Time `json:"CreatedAt"        db:"created_at"`
}

// Storage wraps the SQLite database connection.
type Storage struct {
	db *sql.DB
}

// ── Schema ─────────────────────────────────────────────────────────────────────

const schema = `
CREATE TABLE IF NOT EXISTS users (
	user_id      INTEGER PRIMARY KEY,
	username     TEXT    NOT NULL DEFAULT '',
	language     TEXT    NOT NULL DEFAULT 'en',
	is_vip       BOOLEAN NOT NULL DEFAULT 0,
	min_volume   INTEGER NOT NULL DEFAULT 10000,
	eth_enabled  BOOLEAN NOT NULL DEFAULT 1,
	sol_enabled  BOOLEAN NOT NULL DEFAULT 1,
	base_enabled BOOLEAN NOT NULL DEFAULT 1,
	bsc_enabled  BOOLEAN NOT NULL DEFAULT 1,
	created_at   DATETIME NOT NULL
);

CREATE TABLE IF NOT EXISTS user_watchlists (
	id             INTEGER  PRIMARY KEY AUTOINCREMENT,
	user_id        INTEGER  NOT NULL REFERENCES users(user_id) ON DELETE CASCADE,
	wallet_address TEXT     NOT NULL,
	note           TEXT     NOT NULL DEFAULT '',
	created_at     DATETIME NOT NULL,
	UNIQUE(user_id, wallet_address)
);

CREATE TABLE IF NOT EXISTS alert_counts (
	user_id    INTEGER NOT NULL,
	date_str   TEXT    NOT NULL,
	count      INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY(user_id, date_str)
);

CREATE INDEX IF NOT EXISTS idx_watchlists_user   ON user_watchlists(user_id);
CREATE INDEX IF NOT EXISTS idx_watchlists_wallet ON user_watchlists(wallet_address);
CREATE INDEX IF NOT EXISTS idx_clusters_chain    ON clusters(chain);
CREATE INDEX IF NOT EXISTS idx_clusters_created  ON clusters(created_at);
`

// ── Init ───────────────────────────────────────────────────────────────────────

// InitDB opens (or creates) the SQLite database at dbPath, migrates the
// schema, and returns a ready-to-use Storage.
func InitDB(dbPath string) (*Storage, error) {
	if dbPath == "" {
		dbPath = "./data/bot.db"
	}
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return nil, fmt.Errorf("storage: mkdir %s: %w", filepath.Dir(dbPath), err)
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("storage: open db: %w", err)
	}
	// SQLite is single-writer; one connection avoids SQLITE_BUSY churn.
	db.SetMaxOpenConns(1)

	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("storage: ping db: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("storage: apply schema: %w", err)
	}

	return &Storage{db: db}, nil
}

// ── User Methods ───────────────────────────────────────────────────────────────

// GetOrCreateUser returns the user from DB, creating them with sensible defaults
// if they don't exist yet. It also updates username and language on every call.
func (s *Storage) GetOrCreateUser(userID int64, username, lang string) (*User, error) {
	if lang == "" {
		lang = "en"
	}

	u := &User{}
	row := s.db.QueryRow(
		`SELECT user_id, username, language, is_vip, min_volume,
		        eth_enabled, sol_enabled, base_enabled, bsc_enabled, created_at
		 FROM users WHERE user_id = ?`, userID)

	var createdStr string
	err := row.Scan(
		&u.UserID, &u.Username, &u.Language, &u.IsVIP, &u.MinVolume,
		&u.EthEnabled, &u.SolEnabled, &u.BaseEnabled, &u.BscEnabled, &createdStr,
	)
	if err == sql.ErrNoRows {
		now := time.Now().UTC()
		_, err = s.db.Exec(
			`INSERT INTO users
			 (user_id, username, language, is_vip, min_volume,
			  eth_enabled, sol_enabled, base_enabled, bsc_enabled, created_at)
			 VALUES (?, ?, ?, 0, 10000, 1, 1, 1, 1, ?)`,
			userID, username, lang, now,
		)
		if err != nil {
			return nil, fmt.Errorf("storage: insert user %d: %w", userID, err)
		}
		return &User{
			UserID: userID, Username: username, Language: lang,
			MinVolume: 10000, EthEnabled: true, SolEnabled: true,
			BaseEnabled: true, BscEnabled: true, CreatedAt: now,
		}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("storage: query user %d: %w", userID, err)
	}
	u.CreatedAt = parseTime(createdStr)

	// Keep username/language fresh.
	if u.Username != username || u.Language != lang {
		u.Username = username
		u.Language = lang
		if _, err := s.db.Exec(
			`UPDATE users SET username = ?, language = ? WHERE user_id = ?`,
			username, lang, userID,
		); err != nil {
			return nil, fmt.Errorf("storage: update user %d: %w", userID, err)
		}
	}
	return u, nil
}

// UpdateUserSettings persists user alert preferences.
func (s *Storage) UpdateUserSettings(userID int64, minVolume int, eth, sol, base, bsc bool) error {
	_, err := s.db.Exec(
		`UPDATE users
		 SET min_volume = ?, eth_enabled = ?, sol_enabled = ?,
		     base_enabled = ?, bsc_enabled = ?
		 WHERE user_id = ?`,
		minVolume, eth, sol, base, bsc, userID,
	)
	if err != nil {
		return fmt.Errorf("storage: update settings for %d: %w", userID, err)
	}
	return nil
}

// SetUserVIP upgrades (or downgrades) the VIP status of a user.
func (s *Storage) SetUserVIP(userID int64, vip bool) error {
	_, err := s.db.Exec(`UPDATE users SET is_vip = ? WHERE user_id = ?`, vip, userID)
	return err
}

// SetUserLanguage updates only the language preference.
func (s *Storage) SetUserLanguage(userID int64, lang string) error {
	if lang == "" {
		lang = "en"
	}
	_, err := s.db.Exec(`UPDATE users SET language = ? WHERE user_id = ?`, lang, userID)
	return err
}

// CheckAndIncrementFreeAlert checks if a free user has reached their daily limit (e.g. 5) and increments if not.
// Returns true if the alert can be sent, false if daily limit reached.
func (s *Storage) CheckAndIncrementFreeAlert(userID int64, maxAlerts int) (bool, error) {
	dateStr := time.Now().UTC().Format("2006-01-02")
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	var count int
	err = tx.QueryRow(`SELECT count FROM alert_counts WHERE user_id = ? AND date_str = ?`, userID, dateStr).Scan(&count)
	if err == sql.ErrNoRows {
		count = 0
	} else if err != nil {
		return false, err
	}

	if count >= maxAlerts {
		return false, nil
	}

	_, err = tx.Exec(
		`INSERT INTO alert_counts (user_id, date_str, count) VALUES (?, ?, 1)
		 ON CONFLICT(user_id, date_str) DO UPDATE SET count = count + 1`,
		userID, dateStr,
	)
	if err != nil {
		return false, err
	}

	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// GetAllUsers returns all users (used by the alert broadcaster).
func (s *Storage) GetAllUsers() ([]User, error) {
	rows, err := s.db.Query(
		`SELECT user_id, username, language, is_vip, min_volume,
		        eth_enabled, sol_enabled, base_enabled, bsc_enabled, created_at
		 FROM users`,
	)
	if err != nil {
		return nil, fmt.Errorf("storage: query all users: %w", err)
	}
	defer rows.Close()

	var users []User
	for rows.Next() {
		var u User
		var createdStr string
		if err := rows.Scan(
			&u.UserID, &u.Username, &u.Language, &u.IsVIP, &u.MinVolume,
			&u.EthEnabled, &u.SolEnabled, &u.BaseEnabled, &u.BscEnabled, &createdStr,
		); err != nil {
			return nil, fmt.Errorf("storage: scan user: %w", err)
		}
		u.CreatedAt = parseTime(createdStr)
		users = append(users, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: rows err users: %w", err)
	}
	return users, nil
}

// ── Watchlist Methods ──────────────────────────────────────────────────────────

// AddWatchlistWallet saves a wallet address to a user's watchlist.
// Duplicate entries (same user_id + wallet_address) are silently ignored.
func (s *Storage) AddWatchlistWallet(userID int64, walletAddress, note string) error {
	_, err := s.db.Exec(
		`INSERT OR IGNORE INTO user_watchlists (user_id, wallet_address, note, created_at)
		 VALUES (?, ?, ?, ?)`,
		userID, walletAddress, note, time.Now().UTC(),
	)
	if err != nil {
		return fmt.Errorf("storage: add watchlist wallet: %w", err)
	}
	return nil
}

// GetWatchlist returns all watchlist entries for a user.
func (s *Storage) GetWatchlist(userID int64) ([]WatchlistEntry, error) {
	rows, err := s.db.Query(
		`SELECT id, user_id, wallet_address, note, created_at
		 FROM user_watchlists WHERE user_id = ? ORDER BY id DESC`,
		userID,
	)
	if err != nil {
		return nil, fmt.Errorf("storage: get watchlist: %w", err)
	}
	defer rows.Close()

	var entries []WatchlistEntry
	for rows.Next() {
		var e WatchlistEntry
		var createdStr string
		if err := rows.Scan(&e.ID, &e.UserID, &e.WalletAddress, &e.Note, &createdStr); err != nil {
			return nil, fmt.Errorf("storage: scan watchlist entry: %w", err)
		}
		e.CreatedAt = parseTime(createdStr)
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: rows err watchlist: %w", err)
	}
	return entries, nil
}

// RemoveWatchlistWallet deletes a watchlist entry by its row ID (must belong to userID).
func (s *Storage) RemoveWatchlistWallet(userID, entryID int64) error {
	_, err := s.db.Exec(
		`DELETE FROM user_watchlists WHERE id = ? AND user_id = ?`,
		entryID, userID,
	)
	return err
}

// GetWatchlistUsersByWallet returns user IDs that are watching a specific wallet.
// Used by the broadcaster to send personalised watchlist pings.
func (s *Storage) GetWatchlistUsersByWallet(walletAddress string) ([]int64, error) {
	rows, err := s.db.Query(
		`SELECT DISTINCT user_id FROM user_watchlists WHERE wallet_address = ?`,
		walletAddress,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return ids, nil
}

// ── Cluster Methods ────────────────────────────────────────────────────────────

// SaveCluster persists a detected cluster event.
func (s *Storage) SaveCluster(tokenAddress, tokenSymbol, chain string,
	buyCount int, totalVolumeUSD float64, timeWindowSeconds int, walletAddress string) error {

	_, err := s.db.Exec(
		`INSERT INTO clusters
		 (token_address, token_symbol, chain, buy_count, total_volume_usd,
		  time_window_seconds, wallet_address, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		tokenAddress, tokenSymbol, chain, buyCount, totalVolumeUSD,
		timeWindowSeconds, walletAddress, time.Now().UTC(),
	)
	if err != nil {
		return fmt.Errorf("storage: save cluster: %w", err)
	}
	return nil
}

// GetRecentClusters returns the most recent clusters, newest first.
func (s *Storage) GetRecentClusters(limit int) ([]ClusterRecord, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.Query(
		`SELECT id, token_address, token_symbol, chain, buy_count,
		        total_volume_usd, time_window_seconds, wallet_address, created_at
		 FROM clusters ORDER BY id DESC LIMIT ?`, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("storage: query clusters: %w", err)
	}
	defer rows.Close()

	var records []ClusterRecord
	for rows.Next() {
		var c ClusterRecord
		var walletAddr sql.NullString
		var createdStr string

		if err := rows.Scan(
			&c.ID, &c.TokenAddress, &c.TokenSymbol, &c.Chain, &c.BuyCount,
			&c.TotalVolumeUSD, &c.TimeWindowSeconds, &walletAddr, &createdStr,
		); err != nil {
			return nil, fmt.Errorf("storage: scan cluster: %w", err)
		}
		if walletAddr.Valid {
			c.WalletAddress = walletAddr.String
		} else {
			c.WalletAddress = c.TokenAddress
		}
		c.CreatedAt = parseTime(createdStr)
		records = append(records, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: rows err clusters: %w", err)
	}
	return records, nil
}

// Stats24h returns aggregate stats for the last 24 hours.
type Stats24h struct {
	TotalClusters  int
	TotalVolumeUSD float64
	TopToken       string
	TopChain       string
}

// GetStats24h computes cluster statistics for the last 24 hours.
func (s *Storage) GetStats24h() (*Stats24h, error) {
	since := time.Now().UTC().Add(-24 * time.Hour)
	stats := &Stats24h{}

	row := s.db.QueryRow(
		`SELECT COUNT(*), COALESCE(SUM(total_volume_usd), 0) FROM clusters WHERE created_at >= ?`, since)
	if err := row.Scan(&stats.TotalClusters, &stats.TotalVolumeUSD); err != nil {
		return nil, fmt.Errorf("storage: stats24h aggregate: %w", err)
	}

	// Top token by volume.
	row = s.db.QueryRow(
		`SELECT token_symbol FROM clusters WHERE created_at >= ?
		 GROUP BY token_symbol ORDER BY SUM(total_volume_usd) DESC LIMIT 1`, since)
	_ = row.Scan(&stats.TopToken) // ok if no rows

	// Top chain by count.
	row = s.db.QueryRow(
		`SELECT chain FROM clusters WHERE created_at >= ?
		 GROUP BY chain ORDER BY COUNT(*) DESC LIMIT 1`, since)
	_ = row.Scan(&stats.TopChain)

	return stats, nil
}

// ── Wallet Heat Score (Bonus Feature #1) ──────────────────────────────────────

// WalletHeat represents how active a wallet has been across detected clusters.
type WalletHeat struct {
	WalletAddress  string
	ClusterCount   int
	TotalVolumeUSD float64
}

// GetTopWallets returns the wallets that appear in the most clusters within
// the last N hours — the "hottest" smart-money addresses right now.
func (s *Storage) GetTopWallets(hours int, limit int) ([]WalletHeat, error) {
	if hours <= 0 {
		hours = 24
	}
	if limit <= 0 {
		limit = 5
	}
	since := time.Now().UTC().Add(-time.Duration(hours) * time.Hour)

	rows, err := s.db.Query(
		`SELECT wallet_address, COUNT(*) AS cluster_count,
		        SUM(total_volume_usd) AS total_vol
		 FROM clusters
		 WHERE wallet_address IS NOT NULL
		   AND wallet_address != ''
		   AND created_at >= ?
		 GROUP BY wallet_address
		 ORDER BY cluster_count DESC, total_vol DESC
		 LIMIT ?`, since, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("storage: top wallets: %w", err)
	}
	defer rows.Close()

	var result []WalletHeat
	for rows.Next() {
		var h WalletHeat
		if err := rows.Scan(&h.WalletAddress, &h.ClusterCount, &h.TotalVolumeUSD); err != nil {
			return nil, err
		}
		result = append(result, h)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// parseTime tries common SQLite date layouts and returns a zero time on failure.
func parseTime(s string) time.Time {
	layouts := []string{
		time.RFC3339Nano,
		"2006-01-02 15:04:05.999999999-07:00",
		"2006-01-02 15:04:05",
		"2006-01-02T15:04:05Z",
	}
	for _, l := range layouts {
		if t, err := time.Parse(l, s); err == nil {
			return t
		}
	}
	return time.Time{}
}
