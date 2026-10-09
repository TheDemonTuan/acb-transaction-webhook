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
	connection := historicalConnectionFixture(t, store, ctx, "***1234")

	destination := filepath.Join(t.TempDir(), "backups", "gateway.db")
	if err := store.Backup(ctx, destination); err != nil {
		t.Fatal(err)
	}
	backup, err := sql.Open("sqlite", "file:"+filepath.ToSlash(destination)+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer backup.Close()
	var masked, bank string
	if err := backup.QueryRowContext(ctx, `SELECT account_masked,bank_code FROM connections WHERE id=?`, connection.ID).Scan(&masked, &bank); err != nil {
		t.Fatal(err)
	}
	if masked != connection.AccountMasked || bank != "ACB" {
		t.Fatalf("backup changed historical connection: masked=%q bank=%q", masked, bank)
	}
	if err := store.Backup(ctx, destination); err == nil {
		t.Fatal("expected existing destination to be rejected")
	}
}
