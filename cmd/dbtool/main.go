package main

import (
	"context"
	"encoding/json"
	"flag"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

func main() {
	dbPathFlag := flag.String("path", "", "path to SQLite database file")
	migrateFlag := flag.Bool("migrate", false, "apply schema migrations")
	checkFlag := flag.Bool("check", false, "run read-only integrity check")
	backupToFlag := flag.String("backup-to", "", "destination path for SQLite backup")
	schemaVersionFlag := flag.Bool("schema-version", false, "print schema compatibility information as JSON")
	activeAuthCountFlag := flag.Bool("active-auth-count", false, "print active authentication attempt count as JSON")
	gateStatusFlag := flag.Bool("gate-status", false, "print deployment mutation gate status as JSON")
	gateAcquireFlag := flag.Bool("gate-acquire", false, "acquire deployment mutation gate lease")
	gateReleaseFlag := flag.Bool("gate-release", false, "release deployment mutation gate lease")
	gateRenewFlag := flag.Bool("gate-renew", false, "renew deployment mutation gate lease")
	gateCheckFlag := flag.Bool("gate-check", false, "check mutation gate is open and active auth count is 0")
	schemaCompatFlag := flag.Bool("schema-compat", false, "verify schema compatibility with minimum version")
	sessionCheckFlag := flag.Bool("session-check", false, "verify valid durable session exists in database")
	readonlyFlag := flag.Bool("readonly", false, "open SQLite database in read-only mode")

	connectionIDFlag := flag.String("connection-id", "", "connection ID for session check")
	generationFlag := flag.Int64("generation", 0, "generation for session check")
	ownerFlag := flag.String("owner", "", "lease owner identifier")
	leaseTokenFlag := flag.String("lease-token", "", "lease token for release or renewal")
	leaseDurationFlag := flag.Duration("lease-duration", 2*time.Minute, "duration of lease")
	reasonFlag := flag.String("reason", "deploy", "reason for mutation gate lease")
	minVersionFlag := flag.Int("min-version", 9, "minimum required schema version for schema-compat check")
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
	if len(dbPath) >= 3 && dbPath[0] == '/' && ((dbPath[1] >= 'a' && dbPath[1] <= 'z') || (dbPath[1] >= 'A' && dbPath[1] <= 'Z')) && dbPath[2] == '/' {
		dbPath = string(dbPath[1]) + ":" + dbPath[2:]
	}

	ctx := context.Background()

	actionCount := 0
	for _, selected := range []bool{
		*migrateFlag, *checkFlag, *backupToFlag != "", *schemaVersionFlag,
		*activeAuthCountFlag, *gateStatusFlag, *gateAcquireFlag, *gateReleaseFlag,
		*gateRenewFlag, *gateCheckFlag, *schemaCompatFlag, *sessionCheckFlag,
	} {
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
	isReadOnly := *readonlyFlag || *checkFlag || *schemaVersionFlag || *activeAuthCountFlag || *gateStatusFlag || *gateCheckFlag || *schemaCompatFlag || *sessionCheckFlag
	store, err := storage.OpenWithOptions(ctx, dbPath, storage.OpenOptions{
		RunMigrations: false,
		ReadOnly:      isReadOnly,
	})
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
		return
	}

	if *gateStatusFlag {
		gate, err := store.GetDeploymentGate(ctx)
		if err != nil {
			logger.Error("get gate status failed", "error", err)
			os.Exit(1)
		}
		if err := encoder.Encode(gate); err != nil {
			logger.Error("encode gate status", "error", err)
			os.Exit(1)
		}
		return
	}

	if *gateAcquireFlag {
		owner := *ownerFlag
		if owner == "" {
			owner = "deployer-" + strconv.FormatInt(time.Now().Unix(), 10)
		}
		gate, err := store.AcquireMutationGate(ctx, owner, *leaseDurationFlag, *reasonFlag)
		if err != nil {
			logger.Error("acquire mutation gate failed", "error", err)
			os.Exit(1)
		}
		if err := encoder.Encode(gate); err != nil {
			logger.Error("encode gate acquire", "error", err)
			os.Exit(1)
		}
		return
	}

	if *gateReleaseFlag {
		owner := *ownerFlag
		if owner == "" {
			logger.Error("owner is required to release mutation gate")
			os.Exit(1)
		}
		if err := store.ReleaseMutationGate(ctx, owner, *leaseTokenFlag); err != nil {
			logger.Error("release mutation gate failed", "error", err)
			os.Exit(1)
		}
		if err := encoder.Encode(map[string]any{"status": "released", "gateState": "OPEN"}); err != nil {
			logger.Error("encode gate release", "error", err)
			os.Exit(1)
		}
		return
	}

	if *gateRenewFlag {
		owner := *ownerFlag
		if owner == "" || *leaseTokenFlag == "" {
			logger.Error("owner and lease-token are required to renew mutation gate")
			os.Exit(1)
		}
		if err := store.RenewMutationGate(ctx, owner, *leaseTokenFlag, *leaseDurationFlag); err != nil {
			logger.Error("renew mutation gate failed", "error", err)
			os.Exit(1)
		}
		if err := encoder.Encode(map[string]any{"status": "renewed"}); err != nil {
			logger.Error("encode gate renew", "error", err)
			os.Exit(1)
		}
		return
	}

	if *gateCheckFlag {
		gate, err := store.GetDeploymentGate(ctx)
		if err != nil {
			logger.Error("get gate status failed", "error", err)
			os.Exit(1)
		}
		if err := encoder.Encode(gate); err != nil {
			logger.Error("encode gate check", "error", err)
			os.Exit(1)
		}
		if gate.ActiveAuthCount > 0 {
			logger.Error("active auth session in progress", "count", gate.ActiveAuthCount)
			os.Exit(1)
		}
		if gate.GateState == "LOCKED" && !gate.IsStale && (*ownerFlag == "" || gate.Owner != *ownerFlag) {
			logger.Error("mutation gate is locked", "owner", gate.Owner, "expiresAt", gate.LeaseExpiresAt)
			os.Exit(1)
		}
		return
	}

	if *schemaCompatFlag {
		report, err := store.SchemaVersion(ctx)
		if err != nil {
			logger.Error("schema check failed", "error", err)
			os.Exit(1)
		}
		compat := report.Version >= *minVersionFlag
		res := map[string]any{
			"compatible":      compat,
			"schemaVersion":   report.Version,
			"requiredVersion": *minVersionFlag,
		}
		if err := encoder.Encode(res); err != nil {
			logger.Error("encode schema compat", "error", err)
			os.Exit(1)
		}
		if !compat {
			logger.Error("schema is incompatible", "current", report.Version, "required", *minVersionFlag)
			os.Exit(1)
		}
		return
	}

	if *sessionCheckFlag {
		connID := *connectionIDFlag
		gen := *generationFlag
		if connID == "" || gen <= 0 {
			conn, err := store.Connection(ctx)
			if err != nil {
				logger.Error("failed to get active connection for session check", "error", err)
				os.Exit(1)
			}
			connID = conn.ID
			gen = conn.Generation
		}
		if connID == "" || gen <= 0 {
			logger.Error("no active connection found for session check")
			os.Exit(1)
		}
		session, err := store.Session(ctx, connID, gen)
		if err != nil {
			logger.Error("durable session not found", "connection_id", connID, "generation", gen, "error", err)
			os.Exit(1)
		}
		if len(session.Envelope) == 0 {
			logger.Error("durable session envelope is empty", "connection_id", connID, "generation", gen)
			os.Exit(1)
		}
		report := map[string]any{
			"status":        "ok",
			"connection_id": session.ConnectionID,
			"generation":    session.Generation,
			"has_envelope":  true,
		}
		if err := encoder.Encode(report); err != nil {
			logger.Error("encode session check report", "error", err)
			os.Exit(1)
		}
		return
	}
}
