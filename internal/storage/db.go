package storage

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "github.com/glebarez/go-sqlite"
)

// User represents a stored user in the database.
type User struct {
	UserID          int64      `db:"user_id"`
	Username        string     `db:"username"`
	Language        string     `db:"language"`
	IsSubscribed    bool       `db:"is_subscribed"`
	SubscribedUntil *time.Time `db:"subscribed_until"`
	CreatedAt       time.Time  `db:"created_at"`
}

// Storage wraps the SQLite database connection.
type Storage struct {
	db *sql.DB
}

// InitDB creates tables if they do not exist and returns a Storage instance.
func InitDB(dbPath string) (*Storage, error) {
	if dbPath == "" {
		dbPath = "./data/bot.db"
	}

	// Ensure directory exists
	dir := filepath.Dir(dbPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create database directory: %w", err)
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open sqlite database: %w", err)
	}

	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("failed to ping sqlite database: %w", err)
	}

	query := `
	CREATE TABLE IF NOT EXISTS users (
		user_id INTEGER PRIMARY KEY,
		username TEXT,
		language TEXT NOT NULL DEFAULT 'en',
		is_subscribed BOOLEAN NOT NULL DEFAULT 0,
		subscribed_until DATETIME,
		created_at DATETIME NOT NULL
	);

	CREATE TABLE IF NOT EXISTS clusters (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		token_address TEXT NOT NULL,
		token_symbol TEXT NOT NULL,
		chain TEXT NOT NULL,
		buy_count INTEGER NOT NULL,
		total_volume_usd REAL NOT NULL,
		time_window_seconds INTEGER NOT NULL,
		wallet_address TEXT,
		created_at DATETIME NOT NULL
	);
	`

	if _, err := db.Exec(query); err != nil {
		return nil, fmt.Errorf("failed to create users table: %w", err)
	}

	return &Storage{db: db}, nil
}

// GetAllUsers returns all registered users from the database.
func (s *Storage) GetAllUsers() ([]User, error) {
	rows, err := s.db.Query(`SELECT user_id, username, language, is_subscribed, subscribed_until, created_at FROM users`)
	if err != nil {
		return nil, fmt.Errorf("failed to query all users: %w", err)
	}
	defer rows.Close()

	var users []User
	for rows.Next() {
		var user User
		var subUntil sql.NullTime
		var createdAtStr string

		if err := rows.Scan(&user.UserID, &user.Username, &user.Language, &user.IsSubscribed, &subUntil, &createdAtStr); err != nil {
			return nil, fmt.Errorf("failed to scan user: %w", err)
		}

		if subUntil.Valid {
			user.SubscribedUntil = &subUntil.Time
		}
		user.CreatedAt, _ = time.Parse("2006-01-02 15:04:05.999999999-07:00", createdAtStr)
		if user.CreatedAt.IsZero() {
			user.CreatedAt, _ = time.Parse("2006-01-02 15:04:05", createdAtStr)
		}

		users = append(users, user)
	}

	return users, nil
}

// GetOrCreateUser gets a user by ID, or creates them if they don't exist.
// If the user already exists, updates their username and language if changed.
func (s *Storage) GetOrCreateUser(userID int64, username, lang string) (*User, error) {
	if lang == "" {
		lang = "en"
	}

	// Try to fetch existing user
	var user User
	var subUntil sql.NullTime
	var createdAtStr string

	row := s.db.QueryRow(
		`SELECT user_id, username, language, is_subscribed, subscribed_until, created_at FROM users WHERE user_id = ?`,
		userID,
	)

	err := row.Scan(&user.UserID, &user.Username, &user.Language, &user.IsSubscribed, &subUntil, &createdAtStr)
	if err == sql.ErrNoRows {
		// Create new user
		now := time.Now().UTC()
		_, err := s.db.Exec(
			`INSERT INTO users (user_id, username, language, is_subscribed, created_at) VALUES (?, ?, ?, 0, ?)`,
			userID, username, lang, now,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to insert user: %w", err)
		}

		return &User{
			UserID:       userID,
			Username:     username,
			Language:     lang,
			IsSubscribed: false,
			CreatedAt:    now,
		}, nil
	} else if err != nil {
		return nil, fmt.Errorf("failed to query user: %w", err)
	}

	if subUntil.Valid {
		user.SubscribedUntil = &subUntil.Time
	}
	user.CreatedAt, _ = time.Parse("2006-01-02 15:04:05.999999999-07:00", createdAtStr) // fallback parsing or standard layout
	if user.CreatedAt.IsZero() {
		// try simple layouts
		user.CreatedAt, _ = time.Parse("2006-01-02 15:04:05", createdAtStr)
	}

	// Update username or language if changed
	needUpdate := false
	if user.Username != username {
		user.Username = username
		needUpdate = true
	}
	if user.Language != lang && lang != "" {
		user.Language = lang
		needUpdate = true
	}

	if needUpdate {
		_, err = s.db.Exec(
			`UPDATE users SET username = ?, language = ? WHERE user_id = ?`,
			user.Username, user.Language, userID,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to update user: %w", err)
		}
	}

	return &user, nil
}

// SetUserLanguage sets the language for a specific user.
func (s *Storage) SetUserLanguage(userID int64, lang string) error {
	if lang == "" {
		lang = "en"
	}
	_, err := s.db.Exec(
		`UPDATE users SET language = ? WHERE user_id = ?`,
		lang, userID,
	)
	if err != nil {
		return fmt.Errorf("failed to set user language: %w", err)
	}
	return nil
}

// SetSubscription updates user subscription status and expiry.
func (s *Storage) SetSubscription(userID int64, durationHours int) error {
	now := time.Now().UTC()
	var subUntil time.Time

	// Check existing subscription expiry or use now
	var currentSubUntil sql.NullTime
	err := s.db.QueryRow(`SELECT subscribed_until FROM users WHERE user_id = ?`, userID).Scan(&currentSubUntil)
	if err != nil && err != sql.ErrNoRows {
		return fmt.Errorf("failed to check current subscription: %w", err)
	}

	baseTime := now
	if currentSubUntil.Valid && currentSubUntil.Time.After(now) {
		baseTime = currentSubUntil.Time
	}

	subUntil = baseTime.Add(time.Duration(durationHours) * time.Hour)

	_, err = s.db.Exec(
		`UPDATE users SET is_subscribed = 1, subscribed_until = ? WHERE user_id = ?`,
		subUntil, userID,
	)
	if err != nil {
		return fmt.Errorf("failed to set subscription: %w", err)
	}

	return nil
}

// ClusterRecord represents a stored cluster alert in the database.
type ClusterRecord struct {
	ID                int64     `json:"id" db:"id"`
	TokenAddress      string    `json:"TokenAddress" db:"token_address"`
	TokenSymbol       string    `json:"TokenSymbol" db:"token_symbol"`
	Chain             string    `json:"Chain" db:"chain"`
	BuyCount          int       `json:"BuyCount" db:"buy_count"`
	TotalVolumeUSD    float64   `json:"TotalVolumeUSD" db:"total_volume_usd"`
	TimeWindowSeconds int       `json:"TimeWindowSeconds" db:"time_window_seconds"`
	WalletAddress     string    `json:"WalletAddress" db:"wallet_address"`
	CreatedAt         time.Time `json:"CreatedAt" db:"created_at"`
}

// SaveCluster saves a cluster alert to the database.
func (s *Storage) SaveCluster(tokenAddress, tokenSymbol, chain string, buyCount int, totalVolumeUSD float64, timeWindowSeconds int, walletAddress string) error {
	now := time.Now().UTC()
	_, err := s.db.Exec(
		`INSERT INTO clusters (token_address, token_symbol, chain, buy_count, total_volume_usd, time_window_seconds, wallet_address, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		tokenAddress, tokenSymbol, chain, buyCount, totalVolumeUSD, timeWindowSeconds, walletAddress, now,
	)
	if err != nil {
		return fmt.Errorf("failed to save cluster: %w", err)
	}
	return nil
}

// GetRecentClusters returns recent cluster alerts from the database.
func (s *Storage) GetRecentClusters(limit int) ([]ClusterRecord, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.Query(
		`SELECT id, token_address, token_symbol, chain, buy_count, total_volume_usd, time_window_seconds, wallet_address, created_at FROM clusters ORDER BY id DESC LIMIT ?`,
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to query clusters: %w", err)
	}
	defer rows.Close()

	var clusters []ClusterRecord
	for rows.Next() {
		var c ClusterRecord
		var createdAtStr string
		var walletAddr sql.NullString

		if err := rows.Scan(&c.ID, &c.TokenAddress, &c.TokenSymbol, &c.Chain, &c.BuyCount, &c.TotalVolumeUSD, &c.TimeWindowSeconds, &walletAddr, &createdAtStr); err != nil {
			return nil, fmt.Errorf("failed to scan cluster: %w", err)
		}

		if walletAddr.Valid {
			c.WalletAddress = walletAddr.String
		} else {
			c.WalletAddress = c.TokenAddress
		}

		c.CreatedAt, _ = time.Parse("2006-01-02 15:04:05.999999999-07:00", createdAtStr)
		if c.CreatedAt.IsZero() {
			c.CreatedAt, _ = time.Parse("2006-01-02 15:04:05", createdAtStr)
		}

		clusters = append(clusters, c)
	}

	return clusters, nil
}
