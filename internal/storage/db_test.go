package storage

import (
	"os"
	"path/filepath"
	"testing"
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
	if s == nil {
		t.Fatalf("expected storage instance, got nil")
	}

	// Verify we can save and read a cluster record
	err = s.SaveCluster("0x123", "TEST", "eth", 5, 1000.0, 60, "0xabc")
	if err != nil {
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
