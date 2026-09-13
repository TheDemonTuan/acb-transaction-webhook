package main

import (
	"context"
	"flag"
	"fmt"
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

	// If migrateFlag is set or no flags are set, run migration
	if *migrateFlag || (!*checkFlag && *backupToFlag == "") {
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

	if *checkFlag {
		report, err := store.CheckIntegrity(ctx)
		if err != nil {
			logger.Error("database integrity check failed", "error", err)
			os.Exit(1)
		}
		logger.Info("database integrity report",
			"integrityOk", report.IntegrityOK,
			"integrityMessage", report.IntegrityMessage,
			"migrationsApplied", report.MigrationsApplied,
			"connectionsCount", report.ConnectionsCount,
			"connectionState", report.ConnectionState,
			"generation", report.Generation,
			"transactionsCount", report.TransactionsCount,
			"eventsCount", report.EventsCount,
		)
		if !report.IntegrityOK {
			os.Exit(1)
		}
		fmt.Println("INTEGRITY_OK")
	}
}
