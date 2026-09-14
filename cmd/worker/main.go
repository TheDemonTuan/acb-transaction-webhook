package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/acb"
	"github.com/thedemontuan/acb-transaction-webhook/internal/bark"
	"github.com/thedemontuan/acb-transaction-webhook/internal/config"
	"github.com/thedemontuan/acb-transaction-webhook/internal/lock"
	"github.com/thedemontuan/acb-transaction-webhook/internal/maintenance"
	"github.com/thedemontuan/acb-transaction-webhook/internal/monitor"
	"github.com/thedemontuan/acb-transaction-webhook/internal/notification"
	"github.com/thedemontuan/acb-transaction-webhook/internal/security"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
	"github.com/thedemontuan/acb-transaction-webhook/internal/telemetry"
	"github.com/thedemontuan/acb-transaction-webhook/internal/webhook"
	"github.com/thedemontuan/acb-transaction-webhook/internal/workerrpc"
	"github.com/thedemontuan/acb-transaction-webhook/internal/workerstate"
)

type workerService struct {
	bankMonitor           *monitor.Monitor
	historyRunner         *monitor.HistoryJobRunner
	dispatcher            *notification.Dispatcher
	verifierSessionLoader *monitor.SessionLoader
	verifierClient        *acb.Client
	store                 *storage.Store
	notificationRegistry  *notification.Registry
	barkSender            *bark.Sender
	coordinator           *workerstate.Coordinator
	maintRunner           *maintenance.Runner
}

func (w *workerService) RequestSync(ctx context.Context) error {
	if w.bankMonitor == nil {
		return fmt.Errorf("bank monitor not initialized")
	}
	return w.bankMonitor.RequestSync(ctx)
}

func (w *workerService) CreateHistoryJob(ctx context.Context, fromDay, toDay string) (storage.HistorySyncJob, error) {
	if w.store == nil {
		return storage.HistorySyncJob{}, errors.New("storage not initialized")
	}
	fromT, err := time.Parse("2006-01-02", fromDay)
	if err != nil {
		return storage.HistorySyncJob{}, fmt.Errorf("invalid fromDay: %w", err)
	}
	toT, err := time.Parse("2006-01-02", toDay)
	if err != nil {
		return storage.HistorySyncJob{}, fmt.Errorf("invalid toDay: %w", err)
	}
	if fromT.After(toT) {
		return storage.HistorySyncJob{}, errors.New("fromDay must not be after toDay")
	}
	if toT.Sub(fromT) > 31*24*time.Hour {
		return storage.HistorySyncJob{}, errors.New("range too large (max 31 days)")
	}

	conn, err := w.store.Connection(ctx)
	if err != nil {
		return storage.HistorySyncJob{}, fmt.Errorf("failed to lookup connection: %w", err)
	}
	if conn.State != "MONITORING" {
		return storage.HistorySyncJob{}, errors.New("bank connection is not in MONITORING state")
	}

	job, _, err := w.store.CreateOrGetHistorySyncJob(ctx, conn.ID, conn.Generation, fromDay, toDay)
	if err != nil {
		return storage.HistorySyncJob{}, err
	}
	if w.historyRunner != nil {
		w.historyRunner.Wake()
	}
	return job, nil
}

func (w *workerService) CancelHistoryJob(ctx context.Context, jobID string) error {
	if w.store == nil {
		return errors.New("storage not initialized")
	}
	if err := w.store.CancelHistorySyncJob(ctx, jobID); err != nil {
		return err
	}
	if w.historyRunner != nil {
		w.historyRunner.CancelJob(jobID)
	}
	return nil
}

func (w *workerService) NotifySettingsChanged(ctx context.Context) error {
	if w.bankMonitor == nil {
		return fmt.Errorf("bank monitor not initialized")
	}
	w.bankMonitor.NotifySettingsChanged()
	return nil
}

func (w *workerService) WakeDispatcher(ctx context.Context) error {
	if w.dispatcher == nil {
		return fmt.Errorf("dispatcher not initialized")
	}
	w.dispatcher.Wake()
	return nil
}

func (w *workerService) VerifySession(ctx context.Context, account string, generation int64, password []byte) error {
	if generation <= 0 {
		return fmt.Errorf("invalid generation %d", generation)
	}
	if w.store != nil {
		conn, err := w.store.Connection(ctx)
		if err != nil {
			slog.Error("session verification fail-closed: failed to lookup connection", "account", account, "generation", generation, "error", err)
			return fmt.Errorf("failed to lookup connection for session verification: %w", err)
		}
		if conn.Generation != generation {
			slog.Error("session verification generation mismatch", "account", account, "generation", generation, "current_generation", conn.Generation)
			return fmt.Errorf("stale session verification generation: requested %d, current is %d", generation, conn.Generation)
		}
	}
	if w.verifierSessionLoader == nil || w.verifierClient == nil {
		return fmt.Errorf("session verifier not configured")
	}
	var verifier *monitor.SessionVerifier
	if w.bankMonitor != nil {
		verifier = monitor.NewSessionVerifier(w.verifierSessionLoader, w.verifierClient, w.bankMonitor.Scheduler())
	} else {
		verifier = monitor.NewSessionVerifier(w.verifierSessionLoader, w.verifierClient)
	}
	return verifier.VerifySession(ctx, account, generation, password)
}

func (w *workerService) TestNotificationChannel(ctx context.Context, channelID string) (workerrpc.TestNotificationResponse, error) {
	if w.store == nil {
		return workerrpc.TestNotificationResponse{
			Success:        false,
			Status:         "FAILED",
			SanitizedError: "storage not initialized",
		}, nil
	}
	ch, err := w.store.NotificationChannelByID(ctx, channelID)
	if errors.Is(err, storage.ErrNotFound) {
		return workerrpc.TestNotificationResponse{
			Success:        false,
			Status:         "FAILED",
			SanitizedError: "channel_not_found",
		}, nil
	}
	if err != nil {
		return workerrpc.TestNotificationResponse{
			Success:        false,
			Status:         "FAILED",
			SanitizedError: "storage_error",
		}, nil
	}

	target, err := w.store.DeliveryTargetForDelivery(ctx, storage.Delivery{
		EndpointID:       ch.ID,
		EndpointRevision: ch.Revision,
	})
	if err != nil {
		return workerrpc.TestNotificationResponse{
			Success:        false,
			Status:         "FAILED",
			SanitizedError: "cannot decrypt channel target: " + err.Error(),
		}, nil
	}

	if ch.Provider == "BARK" {
		if w.barkSender == nil {
			return workerrpc.TestNotificationResponse{
				Success:           false,
				Status:            "FAILED",
				ProviderErrorCode: "BARK_NOT_CONFIGURED",
				SanitizedError:    "Bark server URL chưa được cấu hình trên worker",
			}, nil
		}
		res := w.barkSender.SendTestNotification(ctx, target)
		if res.Outcome == notification.OutcomeSuccess {
			return workerrpc.TestNotificationResponse{
				Success:   true,
				Status:    "DELIVERED",
				LatencyMs: int64(res.LatencyMs),
				Message:   "Bark đã chấp nhận thông báo thử — hãy kiểm tra iPhone",
			}, nil
		}
		return workerrpc.TestNotificationResponse{
			Success:           false,
			Status:            "FAILED",
			LatencyMs:         int64(res.LatencyMs),
			ProviderErrorCode: res.ProviderErrorCode,
			SanitizedError:    res.SanitizedError,
		}, nil
	}

	if w.notificationRegistry != nil {
		if sender, ok := w.notificationRegistry.Get(ch.Provider); ok {
			testReq := notification.SendRequest{
				DeliveryID:   "del_test_" + channelID,
				EventID:      "evt_test_ping",
				EventType:    "bank.transaction.credit",
				Target:       target,
				EventPayload: []byte(`{"bank":"ACB","credit":"0","debit":"0","description":"Test Webhook Ping","source":"TEST"}`),
			}
			res := sender.Send(ctx, testReq)
			if res.Outcome == notification.OutcomeSuccess {
				msg := "Webhook endpoint responded with HTTP " + strconv.Itoa(res.StatusCode)
				return workerrpc.TestNotificationResponse{
					Success:   true,
					Status:    "DELIVERED",
					LatencyMs: int64(res.LatencyMs),
					Message:   msg,
				}, nil
			}
			return workerrpc.TestNotificationResponse{
				Success:           false,
				Status:            "FAILED",
				LatencyMs:         int64(res.LatencyMs),
				ProviderErrorCode: res.ProviderErrorCode,
				SanitizedError:    res.SanitizedError,
			}, nil
		}
	}

	return workerrpc.TestNotificationResponse{
		Success:           false,
		Status:            "FAILED",
		ProviderErrorCode: "UNKNOWN_PROVIDER",
		SanitizedError:    "unsupported notification provider: " + ch.Provider,
	}, nil
}

func (w *workerService) Quiesce(ctx context.Context) (workerrpc.QuiesceResponse, error) {
	if w.coordinator == nil {
		return workerrpc.QuiesceResponse{}, errors.New("coordinator not initialized")
	}

	// 1. Mark coordinator QUIESCING -> QUIESCED
	if err := w.coordinator.Quiesce(ctx); err != nil {
		return workerrpc.QuiesceResponse{}, fmt.Errorf("coordinator quiesce: %w", err)
	}

	// 2. Pause scheduler
	if w.bankMonitor != nil && w.bankMonitor.Scheduler() != nil {
		w.bankMonitor.Scheduler().Pause()
	}

	// 3. Pause background history runner and requeue RUNNING jobs
	if w.historyRunner != nil {
		w.historyRunner.Pause()
	}
	if w.store != nil {
		requeueCtx, rCancel := context.WithTimeout(ctx, 3*time.Second)
		_, _ = w.store.RequeueRunningHistorySyncJobs(requeueCtx, "Worker quiesced for upgrade")
		rCancel()
	}

	// 4. Pause notification dispatcher
	if w.dispatcher != nil {
		w.dispatcher.Pause()
	}

	// 5. Pause maintenance runner
	if w.maintRunner != nil {
		w.maintRunner.Pause()
	}

	// 6. Persist freshest session snapshot
	if w.bankMonitor != nil {
		persistCtx, pCancel := context.WithTimeout(ctx, 3*time.Second)
		if err := w.bankMonitor.PersistSession(persistCtx); err != nil {
			slog.Warn("persist session snapshot on quiesce", "error", err)
		}
		pCancel()
	}

	// 7. Report generation and latest checkpoint
	var gen int64
	var checkpointStr, coverageTo, scanID string
	if w.store != nil {
		if conn, err := w.store.Connection(ctx); err == nil {
			gen = conn.Generation
			if cp, cpErr := w.store.GetCheckpoint(ctx, conn.ID); cpErr == nil && cp != nil {
				checkpointStr = cp.UpdatedAt
				coverageTo = cp.CoverageTo
				scanID = cp.ScanID
			}
		}
	}

	return workerrpc.QuiesceResponse{
		Status:     "quiesced",
		Quiesced:   true,
		Generation: gen,
		Checkpoint: checkpointStr,
		CoverageTo: coverageTo,
		ScanID:     scanID,
	}, nil
}

func (w *workerService) Resume(ctx context.Context) error {
	// 1. Resume scheduler
	if w.bankMonitor != nil && w.bankMonitor.Scheduler() != nil {
		w.bankMonitor.Scheduler().Resume()
	}

	// 2. Resume history runner
	if w.historyRunner != nil {
		w.historyRunner.Resume()
	}

	// 3. Resume dispatcher
	if w.dispatcher != nil {
		w.dispatcher.Resume()
	}

	// 4. Resume maintenance runner
	if w.maintRunner != nil {
		w.maintRunner.Resume()
	}

	// 5. Unpause coordinator back to StateReady
	if w.coordinator != nil {
		if err := w.coordinator.Resume(ctx); err != nil {
			return fmt.Errorf("coordinator resume: %w", err)
		}
	}

	return nil
}

func main() {
	healthcheck := flag.Bool("healthcheck", false, "verify worker health via HTTP (readiness first, then liveness)")
	livenessCheck := flag.Bool("liveness-check", false, "verify worker liveness via HTTP /healthz")
	readinessCheck := flag.Bool("readiness-check", false, "verify worker readiness via HTTP /readyz")
	flag.Parse()

	rpcAddr := os.Getenv("WORKER_RPC_ADDR")
	if rpcAddr == "" {
		rpcPort := os.Getenv("WORKER_PORT")
		if rpcPort == "" {
			rpcPort = "8190"
		}
		rpcAddr = "0.0.0.0:" + rpcPort
	}

	if *healthcheck || *livenessCheck || *readinessCheck {
		client := &http.Client{Timeout: 3 * time.Second}
		_, port, err := net.SplitHostPort(rpcAddr)
		if err != nil {
			port = "8190"
		}
		roleQuery := ""
		if expectedRole := os.Getenv("EXPECTED_ROLE"); expectedRole != "" {
			roleQuery = "?role=" + expectedRole
		}
		if *livenessCheck {
			resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%s/healthz%s", port, roleQuery))
			if err == nil && resp.StatusCode == http.StatusOK {
				return
			}
			os.Exit(1)
		}
		if *readinessCheck {
			resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%s/readyz%s", port, roleQuery))
			if err == nil && resp.StatusCode == http.StatusOK {
				return
			}
			os.Exit(1)
		}
		resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%s/readyz%s", port, roleQuery))
		if err == nil && resp.StatusCode == http.StatusOK {
			return
		}
		resp, err = client.Get(fmt.Sprintf("http://127.0.0.1:%s/healthz%s", port, roleQuery))
		if err == nil && resp.StatusCode == http.StatusOK {
			return
		}
		os.Exit(1)
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	cfg, err := config.Load()
	if err != nil {
		logger.Error("invalid configuration", "error", err)
		os.Exit(1)
	}

	if cfg.Production && cfg.RuntimeRole != config.RuntimeRoleWorker {
		logger.Error("worker requires RUNTIME_ROLE=worker in production", "role", cfg.RuntimeRole)
		os.Exit(1)
	}
	if cfg.RuntimeRole != config.RuntimeRoleWorker {
		logger.Error("unsupported runtime role for worker", "role", cfg.RuntimeRole)
		os.Exit(1)
	}
	if cfg.Production && cfg.WorkerInternalToken == "" {
		logger.Error("WORKER_INTERNAL_TOKEN or WORKER_INTERNAL_TOKEN_FILE is required in production")
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	workerCtx, workerCancel := context.WithCancel(context.Background())
	defer workerCancel()

	coordinator := workerstate.NewCoordinator(
		workerstate.WithDrainTimeout(10*time.Second),
		workerstate.WithShutdownTimeout(15*time.Second),
	)

	// 1. Singleton Fencing via flock on the same gateway.lock
	dataDir := filepath.Dir(cfg.DatabasePath)
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		logger.Error("create data directory failed", "error", err)
		os.Exit(1)
	}
	lockPath := filepath.Join(dataDir, "gateway.lock")
	flock, err := lock.Acquire(lockPath)
	if err != nil {
		logger.Error("failed to acquire singleton worker lock (another instance running?)", "lockPath", lockPath, "error", err)
		os.Exit(1)
	}
	defer flock.Close()
	logger.Info("acquired exclusive singleton worker lock", "lockPath", lockPath)

	// 2. Open SQLite without schema migrations (migrations run via dbtool)
	store, err := storage.OpenRuntime(ctx, cfg.DatabasePath)
	if err != nil {
		logger.Error("open runtime storage failed", "error", err)
		os.Exit(1)
	}
	defer store.Close()

	var keyring *security.Keyring
	if cfg.MasterKeyFile != "" {
		keyring, err = security.LoadKeyring(cfg.MasterKeyFile)
		if err != nil {
			logger.Error("load session encryption key", "error", err)
			os.Exit(1)
		}
		store.WithKeyring(keyring)
	}

	// 3. Notification Registry & Dispatcher
	notificationRegistry := notification.NewRegistry()
	notificationRegistry.Register(notification.ProviderWebhook, webhook.NewSender(nil, false))

	barkCfg := bark.Config{
		ServerURL:         cfg.BarkServerURL,
		PublicURL:         cfg.BarkPublicURL,
		BasicAuthUser:     cfg.BarkBasicAuthUser,
		BasicAuthPassword: cfg.BarkBasicAuthPassword,
		Timeout:           cfg.BarkTimeout,
		DefaultGroup:      cfg.BarkDefaultGroup,
		DefaultLevel:      cfg.BarkDefaultLevel,
		DefaultSound:      cfg.BarkDefaultSound,
	}
	var barkSender *bark.Sender
	if barkCfg.Configured() {
		if err := bark.ValidateConfig(barkCfg); err != nil {
			logger.Error("invalid Bark configuration", "error", err)
			os.Exit(1)
		}
		barkSender = bark.NewSender(barkCfg, nil, cfg.PublicOrigin)
		notificationRegistry.Register(notification.ProviderBark, barkSender)
		logger.Info("Bark notification provider registered in worker")
	}

	dispatcher := notification.NewDispatcher(store, notificationRegistry)
	go dispatcher.Start(workerCtx)

	// 4. ACB Bank Monitor
	acbClient, err := acb.NewClient("https://online.acb.com.vn", nil)
	if err != nil {
		logger.Error("create ACB client failed", "error", err)
		os.Exit(1)
	}
	bankMonitor := monitor.New(store, acbClient, cfg.PollMinInterval, cfg.PollMaxInterval)
	bankMonitor.WithEventNotifier(func(events []storage.EventNotification) {
		dispatcher.Wake()
	})
	var pollStatusMu sync.Mutex
	var lastPollStatus string
	bankMonitor.WithPollNotifier(func(p storage.PollRun, insertedCount int) {
		pollStatusMu.Lock()
		statusChanged := p.Status != lastPollStatus
		lastPollStatus = p.Status
		pollStatusMu.Unlock()
		if insertedCount > 0 || statusChanged {
			dispatcher.Wake()
		}
		if insertedCount == 0 && !statusChanged && p.Status == "SUCCEEDED" {
			return
		}
		payload, err := storage.PollCompletedPayload(p, insertedCount)
		if err != nil {
			return
		}
		appendCtx, aCancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, _ = store.AppendJournalEvent(appendCtx, "ep1", "poll.completed", p.ID, payload)
		aCancel()
	})

	var sessionLoader *monitor.SessionLoader
	var verifierClient *acb.Client
	var verifierSessionLoader *monitor.SessionLoader
	if keyring != nil {
		sessionLoader = monitor.NewSessionLoader(store, keyring, acbClient)
		bankMonitor.WithSessionLoader(sessionLoader)

		verifierClient, err = acb.NewClient("https://online.acb.com.vn", nil)
		if err != nil {
			logger.Error("create ACB session verifier client failed", "error", err)
			os.Exit(1)
		}
		verifierSessionLoader = monitor.NewSessionLoader(store, keyring, verifierClient)
	}
	go bankMonitor.Run(workerCtx)
	logger.Info("ACB bank polling monitor started in worker")

	historyRunner := monitor.NewHistoryJobRunner(store, acbClient, bankMonitor.Scheduler(), sessionLoader).
		WithMonitor(bankMonitor)
	go historyRunner.Run(workerCtx)
	logger.Info("ACB durable history job runner started in worker")

	// 5. Singleton Maintenance Runner (hourly retention and stale auth reap)
	maintRunner := maintenance.NewRunner(store,
		maintenance.WithRetentionPeriod(24*time.Hour),
		maintenance.WithRetentionInterval(1*time.Hour),
		maintenance.WithStaleAuthInterval(30*time.Second),
	)
	go maintRunner.Run(workerCtx)
	logger.Info("singleton maintenance runner started in worker")

	// 6. Setup Private RPC Server
	ws := &workerService{
		bankMonitor:           bankMonitor,
		historyRunner:         historyRunner,
		dispatcher:            dispatcher,
		verifierSessionLoader: verifierSessionLoader,
		verifierClient:        verifierClient,
		store:                 store,
		notificationRegistry:  notificationRegistry,
		barkSender:            barkSender,
		coordinator:           coordinator,
		maintRunner:           maintRunner,
	}

	rpcServer, err := workerrpc.NewServer(ws, cfg.WorkerInternalToken)
	if err != nil {
		logger.Error("create worker RPC server failed", "error", err)
		os.Exit(1)
	}
	rpcServer.SetStateProvider(coordinator.State)
	rpcServer.SetDrainHandler(coordinator.Drain)

	schemaRep, _ := store.SchemaVersion(ctx)
	schemaVer := ""
	if schemaRep.Version > 0 {
		schemaVer = fmt.Sprintf("%d", schemaRep.Version)
	}
	var hbMu sync.RWMutex
	lastWorkerHb := time.Now().UTC()
	rpcServer.SetRuntimeInfo(string(cfg.RuntimeRole), cfg.ReleaseCommit, cfg.Slot, schemaVer)
	rpcServer.SetHeartbeatProvider(func() time.Time {
		hbMu.RLock()
		defer hbMu.RUnlock()
		return lastWorkerHb
	})
	rpcServer.SetStaleThreshold(60 * time.Second)

	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				hbMu.Lock()
				lastWorkerHb = time.Now().UTC()
				hbMu.Unlock()
				telemetry.Default.SetWorkerSingleton(
					string(cfg.RuntimeRole),
					string(coordinator.State()),
					flock != nil,
					"NONE",
					lastWorkerHb,
					60*time.Second,
				)
			}
		}
	}()

	rpcServer.SetReadyChecker(func(ctx context.Context) error {
		if !coordinator.IsReady() {
			return fmt.Errorf("worker state is %s", coordinator.State())
		}
		if bankMonitor == nil || dispatcher == nil || store == nil {
			return errors.New("worker services not fully initialized")
		}
		if err := store.Health(ctx); err != nil {
			return fmt.Errorf("storage health check failed: %w", err)
		}
		if _, err := store.SchemaVersion(ctx); err != nil {
			return fmt.Errorf("schema check failed: %w", err)
		}
		if flock == nil {
			return errors.New("singleton worker lock not held")
		}
		return nil
	})

	coordinator.RegisterStopHook(func(stopCtx context.Context) error {
		logger.Info("persisting session snapshot and checkpointing background jobs on shutdown")
		// 1. Session snapshot persistence with fresh bounded context
		persistCtx, pCancel := context.WithTimeout(stopCtx, 3*time.Second)
		if err := bankMonitor.PersistSession(persistCtx); err != nil {
			logger.Warn("persist session snapshot on shutdown", "error", err)
		}
		pCancel()

		// 2. Requeue all RUNNING history sync jobs back to QUEUED
		requeueCtx, rCancel := context.WithTimeout(stopCtx, 3*time.Second)
		if n, err := store.RequeueRunningHistorySyncJobs(requeueCtx, "Graceful worker shutdown"); err != nil {
			logger.Warn("requeue running history jobs on shutdown", "error", err)
		} else if n > 0 {
			logger.Info("requeued running history jobs on shutdown", "count", n)
		}
		rCancel()
		return nil
	})

	if err := coordinator.SetReady(); err != nil {
		logger.Error("failed to mark worker ready", "error", err)
		os.Exit(1)
	}
	logger.Info("singleton worker is READY")

	httpServer := &http.Server{
		Addr:              rpcAddr,
		Handler:           rpcServer.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      40 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	go func() {
		logger.Info("worker private RPC server listening", "addr", rpcAddr)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("worker RPC server failed", "error", err)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	select {
	case sig := <-sigCh:
		logger.Info("received termination signal", "signal", sig)
	case <-coordinator.DrainDone():
		logger.Info("worker drain initiated via RPC")
	case <-ctx.Done():
		logger.Info("worker context done")
	}

	logger.Info("shutting down worker...")
	// Drain if not already drained
	drainCtx, dCancel := context.WithTimeout(context.Background(), 5*time.Second)
	_ = coordinator.Drain(drainCtx)
	dCancel()

	// Stop coordinator (runs session persistence and job requeue)
	shutdownCtx, sCancel := context.WithTimeout(context.Background(), 5*time.Second)
	_ = coordinator.Stop(shutdownCtx)
	sCancel()

	// Stop RPC server
	rpcShutdownCtx, rpcCancel := context.WithTimeout(context.Background(), 3*time.Second)
	_ = httpServer.Shutdown(rpcShutdownCtx)
	rpcCancel()

	// Cancel background workers
	workerCancel()

	logger.Info("worker stopped successfully")
}
