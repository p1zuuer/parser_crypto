package storage

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestInitDB_Fresh(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "bot_db_test_*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	dbPath := filepath.Join(tmpDir, "bot.db")
	s, err := InitDB(dbPath)
	if err != nil {
		t.Fatalf("InitDB failed on fresh database: %v", err)
	}
	defer s.Close()
	if s == nil {
		t.Fatalf("expected storage instance, got nil")
	}

	// Sniper settings row should exist with defaults.
	st, err := s.GetSniperSettings()
	if err != nil {
		t.Fatalf("GetSniperSettings failed: %v", err)
	}
	if st.MinWallets != 3 || st.MinVolumeUSD != 1500 || st.WindowSeconds != 120 {
		t.Fatalf("unexpected default settings: %+v", st)
	}

	// Verify we can save and read a cluster record.
	if err := s.SaveCluster("0x123", "TEST", "eth", 5, 1000.0, 60, "0xabc"); err != nil {
		t.Fatalf("SaveCluster failed: %v", err)
	}
	clusters, err := s.GetRecentClusters(10)
	if err != nil {
		t.Fatalf("GetRecentClusters failed: %v", err)
	}
	if len(clusters) != 1 {
		t.Fatalf("expected 1 cluster record, got %d", len(clusters))
	}
	if clusters[0].TokenSymbol != "TEST" {
		t.Fatalf("expected token symbol TEST, got %s", clusters[0].TokenSymbol)
	}
}

func TestSeedWallets(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "bot_db_test_seed_*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	s, err := InitDB(filepath.Join(tmpDir, "bot.db"))
	if err != nil {
		t.Fatalf("InitDB failed: %v", err)
	}
	defer s.Close()

	if err := s.SeedWallets(); err != nil {
		t.Fatalf("SeedWallets failed: %v", err)
	}
	wallets, err := s.GetSmartWallets()
	if err != nil {
		t.Fatalf("GetSmartWallets failed: %v", err)
	}
	if len(wallets) < 20 {
		t.Fatalf("expected at least 20 seeded wallets, got %d", len(wallets))
	}

	// Re-running SeedWallets must not duplicate or error.
	if err := s.SeedWallets(); err != nil {
		t.Fatalf("second SeedWallets call failed: %v", err)
	}
	wallets2, err := s.GetSmartWallets()
	if err != nil {
		t.Fatalf("GetSmartWallets after re-seed failed: %v", err)
	}
	if len(wallets2) != len(wallets) {
		t.Fatalf("expected seed to be idempotent, got %d then %d", len(wallets), len(wallets2))
	}
}

func TestPruneOldData(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "bot_db_test_prune_*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	s, err := InitDB(filepath.Join(tmpDir, "bot.db"))
	if err != nil {
		t.Fatalf("InitDB failed: %v", err)
	}
	defer s.Close()

	if err := s.SaveCluster("0xold", "OLD", "eth", 2, 500, 60, "0xabc"); err != nil {
		t.Fatalf("SaveCluster failed: %v", err)
	}
	// PruneOldData with a zero-length retention window should remove everything.
	n, err := s.PruneOldData(-1 * time.Hour)
	if err != nil {
		t.Fatalf("PruneOldData failed: %v", err)
	}
	if n < 1 {
		t.Fatalf("expected at least 1 row pruned, got %d", n)
	}
}
