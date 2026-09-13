package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/acb"
	"github.com/thedemontuan/acb-transaction-webhook/internal/bark"
	"github.com/thedemontuan/acb-transaction-webhook/internal/config"
	"github.com/thedemontuan/acb-transaction-webhook/internal/eventhub"
	"github.com/thedemontuan/acb-transaction-webhook/internal/httpapi"
	"github.com/thedemontuan/acb-transaction-webhook/internal/lock"
	"github.com/thedemontuan/acb-transaction-webhook/internal/monitor"
	"github.com/thedemontuan/acb-transaction-webhook/internal/notification"
	"github.com/thedemontuan/acb-transaction-webhook/internal/security"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
	"github.com/thedemontuan/acb-transaction-webhook/internal/webhook"
	"github.com/thedemontuan/acb-transaction-webhook/internal/workerrpc"
)

type gatewayFlags struct {
	healthcheck    bool
	deploycheck    bool
	checkIntegrity bool
	migrateOnly    bool
	backupTo       string
}

func parseGatewayFlags(args []string, output io.Writer) (gatewayFlags, error) {
	fs := flag.NewFlagSet("gateway", flag.ContinueOnError)
	fs.SetOutput(output)
	healthcheck := fs.Bool("healthcheck", false, "verify server health via HTTP")
	deploycheck := fs.Bool("deploycheck", false, "verify server deployment readiness via /internal/deployz")
	checkIntegrity := fs.Bool("check", false, "run read-only database integrity and inventory check")
	migrateOnly := fs.Bool("migrate-only", false, "apply database migrations and exit (deprecated: use dbtool --migrate)")
	backupTo := fs.String("backup-to", "", "create a SQLite backup and exit")

	if err := fs.Parse(args); err != nil {
		return gatewayFlags{}, err
	}
	if *migrateOnly {
		return gatewayFlags{}, errors.New("gateway --migrate-only is disabled: database migrations must be performed using 'dbtool --migrate'")
	}
	return gatewayFlags{
		healthcheck:    *healthcheck,
		deploycheck:    *deploycheck,
		checkIntegrity: *checkIntegrity,
		migrateOnly:    *migrateOnly,
		backupTo:       *backupTo,
	}, nil
}

func main() {
	flags, err := parseGatewayFlags(os.Args[1:], os.Stderr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	if flags.healthcheck {
		client := &http.Client{Timeout: 3 * time.Second}
		ports := []string{"8090", "8080"}
		if addr := os.Getenv("LISTEN_ADDR"); addr != "" {
			if _, p, err := net.SplitHostPort(addr); err == nil {
				ports = append([]string{p}, ports...)
			}
		}
		for _, p := range ports {
			resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%s/healthz", p))
			if err == nil && resp.StatusCode == http.StatusOK {
				return
			}
		}
		os.Exit(1)
	}
	if flags.deploycheck {
		client := &http.Client{Timeout: 5 * time.Second}
		ports := []string{"8090", "8080"}
		if addr := os.Getenv("LISTEN_ADDR"); addr != "" {
			if _, p, err := net.SplitHostPort(addr); err == nil {
				ports = append([]string{p}, ports...)
			}
		}
		token := os.Getenv("WORKER_INTERNAL_TOKEN")
		if token == "" {
			if tokenFile := os.Getenv("WORKER_INTERNAL_TOKEN_FILE"); tokenFile != "" {
				if b, err := os.ReadFile(tokenFile); err == nil {
					token = strings.TrimSpace(string(b))
				}
			}
		}
		for _, p := range ports {
			req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%s/internal/deployz", p), nil)
			if err != nil {
				continue
			}
			if token != "" {
				req.Header.Set("X-Worker-Internal-Token", token)
			}
			resp, err := client.Do(req)
			if err == nil && resp != nil {
				if resp.StatusCode == http.StatusOK {
					var body map[string]any
					if err := json.NewDecoder(resp.Body).Decode(&body); err == nil {
						resp.Body.Close()
						if body["storage"] == "ready" && body["schema"] == "compatible" {
							fmt.Println("DEPLOYZ_READY")
							return
						}
					} else {
						resp.Body.Close()
					}
				} else {
					resp.Body.Close()
				}
			}
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
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	if err := os.MkdirAll(filepath.Dir(cfg.DatabasePath), 0o750); err != nil {
		logger.Error("create data directory", "error", err)
		os.Exit(1)
	}
	var fileLock *lock.FileLock
	// When Worker is running as a dedicated service, Gateway operates in concurrent HTTP-only mode
	// and does not hold a singleton lock on gateway.lock.
	if cfg.WorkerRPCURL == "" {
		lockPath := filepath.Join(filepath.Dir(cfg.DatabasePath), "gateway.lock")
		if flags.checkIntegrity || flags.backupTo != "" {
			fileLock, err = lock.AcquireShared(lockPath)
		} else {
			fileLock, err = lock.Acquire(lockPath)
		}
		if err != nil {
			logger.Error("gateway lock unavailable", "error", err)
			os.Exit(1)
		}
		defer fileLock.Close()
	}
	store, err := storage.OpenRuntime(ctx, cfg.DatabasePath)
	if err != nil {
		logger.Error("open storage", "error", err)
		os.Exit(1)
	}
	defer store.Close()
	if n, err := store.ExpireStaleAuthAttempts(ctx); err == nil && n > 0 {
		logger.Info("reaped stale auth attempts on startup", "count", n)
	}

	if flags.backupTo != "" {
		if err := store.Backup(ctx, flags.backupTo); err != nil {
			logger.Error("database backup failed", "error", err)
			os.Exit(1)
		}
		logger.Info("database backup created", "destination", flags.backupTo)
		return
	}

	if flags.checkIntegrity {
		report, err := store.CheckIntegrity(ctx)
		if err != nil {
			logger.Error("database check failed", "error", err)
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
			"deliveriesPending", report.Deliveries.Pending,
			"deliveriesInFlight", report.Deliveries.InFlight,
			"deliveriesDelivered", report.Deliveries.Delivered,
			"deliveriesDeadLetter", report.Deliveries.DeadLetter,
			"endpointsCount", report.EndpointsCount,
			"activeEndpoints", report.ActiveEndpoints,
			"quarantinedCount", report.QuarantinedCount,
			"orphanTransactions", report.OrphanTransactions,
		)
		if !report.IntegrityOK {
			os.Exit(1)
		}
		return
	}

	var keyring *security.Keyring
	if cfg.MasterKeyFile != "" {
		keyring, err = security.LoadKeyring(cfg.MasterKeyFile)
		if err != nil {
			logger.Error("load session encryption key", "error", err)
			os.Exit(1)
		}
		store.WithKeyring(keyring)
	}

	hub := eventhub.New()

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
		logger.Info("Bark notification provider registered")
	}

	var server *httpapi.Server
	if cfg.WorkerRPCURL != "" {
		logger.Info("starting gateway in HTTP-only mode with worker RPC", "workerRPCURL", cfg.WorkerRPCURL)
		workerClient := workerrpc.NewClient(cfg.WorkerRPCURL, cfg.WorkerInternalToken)
		server = httpapi.New(cfg, store).
			WithSyncRequester(workerClient).
			WithHistoryEnsurer(workerClient).
			WithMonitorNotifier(workerClient).
			WithEventHub(hub).
			WithBarkSender(barkSender).
			WithNotificationRegistry(notificationRegistry).
			WithWakeDispatcher(workerClient.WakeDispatcher).
			WithAuthVerifier(workerClient).
			WithWorkerProber(workerClient)
		go server.RunJournalWatcher(ctx, 200*time.Millisecond)
	} else {
		dispatcher := notification.NewDispatcher(store, notificationRegistry)
		go dispatcher.Start(ctx)

		acbClient, err := acb.NewClient("https://online.acb.com.vn", nil)
		if err != nil {
			logger.Error("create ACB client", "error", err)
			os.Exit(1)
		}
		bankMonitor := monitor.New(store, acbClient, cfg.PollMinInterval, cfg.PollMaxInterval)
		bankMonitor.WithEventNotifier(func(events []storage.EventNotification) {
			for _, ev := range events {
				hub.Publish(eventhub.Event{
					Seq:         ev.JournalSeq,
					Epoch:       ev.Epoch,
					EventType:   ev.EventType,
					AggregateID: ev.TransactionID,
					Payload:     ev.Payload,
					CreatedAt:   ev.CreatedAt,
				})
			}
			dispatcher.Wake()
		})
		var pollStatusMu sync.Mutex
		var lastPollStatus string
		bankMonitor.WithPollNotifier(func(p storage.PollRun, insertedCount int) {
			pollStatusMu.Lock()
			statusChanged := p.Status != lastPollStatus
			lastPollStatus = p.Status
			pollStatusMu.Unlock()

			// Only push to SSE when there are actually new transactions or when poll status changed.
			// Suppress routine duplicate polls to avoid noisy repetitive SSE events.
			if insertedCount == 0 && !statusChanged && p.Status == "SUCCEEDED" {
				return
			}

			payload, err := storage.PollCompletedPayload(p, insertedCount)
			if err != nil {
				return
			}
			seq, err := store.AppendJournalEvent(context.Background(), "ep1", "poll.completed", p.ID, payload)
			if err != nil {
				return
			}
			hub.Publish(eventhub.Event{
				Seq:         seq,
				Epoch:       "ep1",
				EventType:   "poll.completed",
				AggregateID: p.ID,
				Payload:     payload,
				CreatedAt:   time.Now().UTC().Format(time.RFC3339Nano),
			})
		})
		var sessionLoader *monitor.SessionLoader
		if keyring != nil {
			sessionLoader = monitor.NewSessionLoader(store, keyring, acbClient)
			bankMonitor.WithSessionLoader(sessionLoader)
		}
		go bankMonitor.Run(ctx)

		server = httpapi.New(cfg, store).
			WithSyncRequester(bankMonitor).
			WithHistoryEnsurer(bankMonitor).
			WithMonitorNotifier(httpapi.MonitorNotifierFunc(func(ctx context.Context) error {
				bankMonitor.NotifySettingsChanged()
				return nil
			})).
			WithEventHub(hub).
			WithBarkSender(barkSender).
			WithNotificationRegistry(notificationRegistry).
			WithWakeDispatcher(func(ctx context.Context) error {
				dispatcher.Wake()
				return nil
			})

		if keyring != nil {
			verifierClient, verifierErr := acb.NewClient("https://online.acb.com.vn", nil)
			if verifierErr != nil {
				logger.Error("create ACB session verifier client", "error", verifierErr)
				os.Exit(1)
			}
			verifierLoader := monitor.NewSessionLoader(store, keyring, verifierClient)
			server.WithAuthVerifier(monitor.NewSessionVerifier(verifierLoader, verifierClient, bankMonitor.UpstreamGate()))
		}
	}
	go server.RunJournalRetention(ctx, 24*time.Hour)
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_, _ = store.ExpireStaleAuthAttempts(ctx)
			}
		}
	}()

	primaryAddr := cfg.Address
	addresses := []string{primaryAddr}
	if strings.HasSuffix(primaryAddr, ":8090") {
		addresses = append(addresses, strings.TrimSuffix(primaryAddr, ":8090")+":8080")
	} else if strings.HasSuffix(primaryAddr, ":8080") {
		addresses = append(addresses, strings.TrimSuffix(primaryAddr, ":8080")+":8090")
	}

	handler := server.Handler()
	var servers []*http.Server
	errCh := make(chan error, len(addresses))

	for _, addr := range addresses {
		srv := &http.Server{
			Addr:              addr,
			Handler:           handler,
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       15 * time.Second,
			WriteTimeout:      30 * time.Second,
			IdleTimeout:       60 * time.Second,
			MaxHeaderBytes:    1 << 20,
		}
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			logger.Warn("could not listen on address", "address", addr, "error", err)
			continue
		}
		servers = append(servers, srv)
		logger.Info("gateway listening", "address", addr)
		go func(s *http.Server, l net.Listener) {
			errCh <- s.Serve(l)
		}(srv, ln)
	}
	if len(servers) == 0 {
		logger.Error("no listener could be started")
		os.Exit(1)
	}

	select {
	case <-ctx.Done():
		logger.Info("gateway shutting down")
		shutdownCtx, stop := context.WithTimeout(context.Background(), 20*time.Second)
		defer stop()
		for _, srv := range servers {
			_ = srv.Shutdown(shutdownCtx)
		}
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			logger.Error("server failed", "error", err)
			os.Exit(1)
		}
	}
}
