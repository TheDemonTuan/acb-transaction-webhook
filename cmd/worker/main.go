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
	"sync"
	"syscall"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/acb"
	"github.com/thedemontuan/acb-transaction-webhook/internal/bark"
	"github.com/thedemontuan/acb-transaction-webhook/internal/config"
	"github.com/thedemontuan/acb-transaction-webhook/internal/lock"
	"github.com/thedemontuan/acb-transaction-webhook/internal/monitor"
	"github.com/thedemontuan/acb-transaction-webhook/internal/notification"
	"github.com/thedemontuan/acb-transaction-webhook/internal/security"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
	"github.com/thedemontuan/acb-transaction-webhook/internal/webhook"
	"github.com/thedemontuan/acb-transaction-webhook/internal/workerrpc"
)

type workerService struct {
	bankMonitor           *monitor.Monitor
	dispatcher            *notification.Dispatcher
	verifierSessionLoader *monitor.SessionLoader
	verifierClient        *acb.Client
	store                 *storage.Store
}

func (w *workerService) RequestSync(ctx context.Context) error {
	if w.bankMonitor == nil {
		return fmt.Errorf("bank monitor not initialized")
	}
	return w.bankMonitor.RequestSync(ctx)
}

func (w *workerService) EnsureHistory(ctx context.Context, fromDay, toDay string) (int, error) {
	if w.bankMonitor == nil {
		return 0, fmt.Errorf("bank monitor not initialized")
	}
	return w.bankMonitor.EnsureHistory(ctx, fromDay, toDay)
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
	if w.verifierSessionLoader == nil || w.verifierClient == nil {
		return fmt.Errorf("session verifier not configured")
	}
	if generation <= 0 {
		return fmt.Errorf("invalid generation %d", generation)
	}
	if w.store != nil {
		conn, err := w.store.Connection(ctx)
		if err == nil && conn.Generation > generation {
			return fmt.Errorf("stale session verification generation: requested %d, current is %d", generation, conn.Generation)
		}
	}
	verifier := monitor.NewSessionVerifier(w.verifierSessionLoader, w.verifierClient, w.bankMonitor.UpstreamGate())
	return verifier.VerifySession(ctx, account, generation, password)
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
		if *livenessCheck {
			resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%s/healthz", port))
			if err == nil && resp.StatusCode == http.StatusOK {
				return
			}
			os.Exit(1)
		}
		if *readinessCheck {
			resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%s/readyz", port))
			if err == nil && resp.StatusCode == http.StatusOK {
				return
			}
			os.Exit(1)
		}
		resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%s/readyz", port))
		if err == nil && resp.StatusCode == http.StatusOK {
			return
		}
		resp, err = client.Get(fmt.Sprintf("http://127.0.0.1:%s/healthz", port))
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

	if cfg.Production && cfg.WorkerInternalToken == "" {
		logger.Error("WORKER_INTERNAL_TOKEN or WORKER_INTERNAL_TOKEN_FILE is required in production")
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

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
	if barkCfg.Configured() {
		if err := bark.ValidateConfig(barkCfg); err != nil {
			logger.Error("invalid Bark configuration", "error", err)
			os.Exit(1)
		}
		barkSender := bark.NewSender(barkCfg, nil, cfg.PublicOrigin)
		notificationRegistry.Register(notification.ProviderBark, barkSender)
		logger.Info("Bark notification provider registered in worker")
	}

	dispatcher := notification.NewDispatcher(store, notificationRegistry)
	go dispatcher.Start(ctx)

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
		_, _ = store.AppendJournalEvent(context.Background(), "ep1", "poll.completed", p.ID, payload)
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
	go bankMonitor.Run(ctx)
	logger.Info("ACB bank polling monitor started in worker")

	// 5. Setup Private RPC Server
	ws := &workerService{
		bankMonitor:           bankMonitor,
		dispatcher:            dispatcher,
		verifierSessionLoader: verifierSessionLoader,
		verifierClient:        verifierClient,
		store:                 store,
	}

	rpcServer, err := workerrpc.NewServer(ws, cfg.WorkerInternalToken)
	if err != nil {
		logger.Error("create worker RPC server failed", "error", err)
		os.Exit(1)
	}

	rpcServer.SetReadyChecker(func(ctx context.Context) error {
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

	httpServer := &http.Server{
		Addr:              rpcAddr,
		Handler:           rpcServer.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	go func() {
		logger.Info("worker private RPC server listening", "addr", rpcAddr)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("worker RPC server failed", "error", err)
		}
	}()

	<-ctx.Done()
	logger.Info("shutting down worker...")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(shutdownCtx)
	logger.Info("worker stopped successfully")
}
