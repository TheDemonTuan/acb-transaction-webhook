package main

import (
	"context"
	"encoding/json"
	"flag"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

func main() {
	dbPathFlag := flag.String("path", "", "path to SQLite database file")
	migrateFlag := flag.Bool("migrate", false, "apply schema migrations")
	checkFlag := flag.Bool("check", false, "run read-only integrity check")
	backupToFlag := flag.String("backup-to", "", "destination path for SQLite backup")
	schemaVersionFlag := flag.Bool("schema-version", false, "print schema compatibility information as JSON")
	activeAuthCountFlag := flag.Bool("active-auth-count", false, "print active authentication attempt count as JSON")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	dbPath := *dbPathFlag
	if dbPath == "" {
		dbPath = os.Getenv("DATABASE_PATH")
	}
	if dbPath == "" {
		dataDir := os.Getenv("DATA_DIR")
		if dataDir == "" {
			dataDir = "./data"
		}
		dbPath = filepath.Join(dataDir, "gateway.db")
	}

	ctx := context.Background()

	actionCount := 0
	for _, selected := range []bool{*migrateFlag, *checkFlag, *backupToFlag != "", *schemaVersionFlag, *activeAuthCountFlag} {
		if selected {
			actionCount++
		}
	}
	if actionCount > 1 {
		logger.Error("select exactly one dbtool action")
		os.Exit(2)
	}

	// If --migrate is set or no action is selected, run migration.
	if *migrateFlag || actionCount == 0 {
		logger.Info("running database migration", "database", dbPath)
		store, err := storage.OpenWithOptions(ctx, dbPath, storage.OpenOptions{RunMigrations: true})
		if err != nil {
			logger.Error("database migration failed", "error", err)
			os.Exit(1)
		}
		_ = store.Close()
		logger.Info("database migration completed successfully", "database", dbPath)
		return
	}

	// For check or backup, open runtime without auto-migration
	store, err := storage.OpenRuntime(ctx, dbPath)
	if err != nil {
		logger.Error("open database failed", "error", err)
		os.Exit(1)
	}
	defer store.Close()

	if *backupToFlag != "" {
		logger.Info("creating database backup", "destination", *backupToFlag)
		if err := store.Backup(ctx, *backupToFlag); err != nil {
			logger.Error("database backup failed", "error", err)
			os.Exit(1)
		}
		logger.Info("database backup created successfully", "destination", *backupToFlag)
		return
	}

	encoder := json.NewEncoder(os.Stdout)
	if *schemaVersionFlag {
		report, err := store.SchemaVersion(ctx)
		if err != nil {
			logger.Error("schema report failed", "error", err)
			os.Exit(1)
		}
		if err := encoder.Encode(report); err != nil {
			logger.Error("encode schema report", "error", err)
			os.Exit(1)
		}
		return
	}

	if *activeAuthCountFlag {
		report, err := store.ActiveAuthAttempts(ctx)
		if err != nil {
			logger.Error("active auth attempt count failed", "error", err)
			os.Exit(1)
		}
		if err := encoder.Encode(report); err != nil {
			logger.Error("encode active auth count", "error", err)
			os.Exit(1)
		}
		return
	}

	if *checkFlag {
		report, err := store.CheckIntegrity(ctx)
		if err != nil {
			logger.Error("database integrity check failed", "error", err)
			os.Exit(1)
		}
		if err := encoder.Encode(report); err != nil {
			logger.Error("encode integrity report", "error", err)
			os.Exit(1)
		}
		if !report.IntegrityOK {
			os.Exit(1)
		}
	}
}
