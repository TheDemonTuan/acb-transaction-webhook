package storage

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func TestBackupCreatesConsistentSnapshot(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "live.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.ConfigureConnection(ctx, "***1234"); err != nil {
		t.Fatal(err)
	}

	destination := filepath.Join(t.TempDir(), "backups", "gateway.db")
	if err := store.Backup(ctx, destination); err != nil {
		t.Fatal(err)
	}
	backup, err := sql.Open("sqlite", "file:"+filepath.ToSlash(destination)+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer backup.Close()
	var count int
	if err := backup.QueryRowContext(ctx, `SELECT count(*) FROM connections`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("expected one connection in backup, got %d", count)
	}
	if err := store.Backup(ctx, destination); err == nil {
		t.Fatal("expected existing destination to be rejected")
	}
}
