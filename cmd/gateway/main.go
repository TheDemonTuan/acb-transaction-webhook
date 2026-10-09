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
	"syscall"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/bark"
	"github.com/thedemontuan/acb-transaction-webhook/internal/config"
	"github.com/thedemontuan/acb-transaction-webhook/internal/eventhub"
	"github.com/thedemontuan/acb-transaction-webhook/internal/httpapi"
	"github.com/thedemontuan/acb-transaction-webhook/internal/lock"
	"github.com/thedemontuan/acb-transaction-webhook/internal/notification"
	"github.com/thedemontuan/acb-transaction-webhook/internal/payments"
	"github.com/thedemontuan/acb-transaction-webhook/internal/realtimestream"
	"github.com/thedemontuan/acb-transaction-webhook/internal/security"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
	"github.com/thedemontuan/acb-transaction-webhook/internal/telemetry"
	"github.com/thedemontuan/acb-transaction-webhook/internal/webhook"
	"github.com/thedemontuan/acb-transaction-webhook/internal/workerrpc"
)

func realtimeStreamReason(err error) string {
	switch {
	case errors.Is(err, realtimestream.ErrUnauthorized):
		return "unauthorized"
	case errors.Is(err, realtimestream.ErrStreamIdle):
		return "idle_timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case err == nil:
		return "stream_closed"
	default:
		return "transport_error"
	}
}

type gatewayFlags struct {
	healthcheck    bool
	livenessCheck  bool
	readinessCheck bool
	deploycheck    bool
	checkIntegrity bool
	migrateOnly    bool
	backupTo       string
}

func parseGatewayFlags(args []string, output io.Writer) (gatewayFlags, error) {
	fs := flag.NewFlagSet("gateway", flag.ContinueOnError)
	fs.SetOutput(output)
	healthcheck := fs.Bool("healthcheck", false, "verify server health via HTTP")
	livenessCheck := fs.Bool("liveness-check", false, "verify server liveness via HTTP /healthz")
	readinessCheck := fs.Bool("readiness-check", false, "verify server readiness via HTTP /readyz")
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
		livenessCheck:  *livenessCheck,
		readinessCheck: *readinessCheck,
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
	if flags.healthcheck || flags.livenessCheck {
		client := &http.Client{Timeout: 3 * time.Second}
		ports := []string{"8090", "8080"}
		if addr := os.Getenv("LISTEN_ADDR"); addr != "" {
			if _, p, err := net.SplitHostPort(addr); err == nil {
				ports = append([]string{p}, ports...)
			}
		}
		roleQuery := ""
		if expectedRole := os.Getenv("EXPECTED_ROLE"); expectedRole != "" {
			roleQuery = "?role=" + expectedRole
		}
		for _, p := range ports {
			resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%s/healthz%s", p, roleQuery))
			if err == nil && resp.StatusCode == http.StatusOK {
				return
			}
		}
		os.Exit(1)
	}
	if flags.readinessCheck {
		client := &http.Client{Timeout: 3 * time.Second}
		ports := []string{"8090", "8080"}
		if addr := os.Getenv("LISTEN_ADDR"); addr != "" {
			if _, p, err := net.SplitHostPort(addr); err == nil {
				ports = append([]string{p}, ports...)
			}
		}
		roleQuery := ""
		if expectedRole := os.Getenv("EXPECTED_ROLE"); expectedRole != "" {
			roleQuery = "?role=" + expectedRole
		}
		for _, p := range ports {
			resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%s/readyz%s", p, roleQuery))
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
						storageOK := body["storage"] == "ready"
						schemaOK := body["schema"] == "compatible"
						workerOK := body["worker"] == "ready" || body["worker"] == "monolith"
						slotOK := true
						if expectedSlot := os.Getenv("EXPECTED_SLOT"); expectedSlot != "" {
							slotOK = (body["slot"] == expectedSlot)
						}
						commitOK := true
						if expectedCommit := os.Getenv("EXPECTED_RELEASE_COMMIT"); expectedCommit != "" {
							commitOK = (body["release"] == expectedCommit)
						}
						roleOK := true
						if expectedRole := os.Getenv("EXPECTED_ROLE"); expectedRole != "" {
							roleOK = (body["role"] == expectedRole)
						}
						if storageOK && schemaOK && workerOK && slotOK && commitOK && roleOK {
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

	if cfg.Production && cfg.RuntimeRole != config.RuntimeRoleGateway {
		logger.Error("gateway requires RUNTIME_ROLE=gateway in production", "role", cfg.RuntimeRole)
		os.Exit(1)
	}
	if cfg.RuntimeRole != config.RuntimeRoleGateway && cfg.RuntimeRole != config.RuntimeRoleMonolithDev {
		logger.Error("unsupported runtime role for gateway", "role", cfg.RuntimeRole)
		os.Exit(1)
	}
	if cfg.RuntimeRole == config.RuntimeRoleGateway && cfg.WorkerRPCURL == "" {
		logger.Error("gateway role requires WORKER_RPC_URL", "role", cfg.RuntimeRole)
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	if err := os.MkdirAll(filepath.Dir(cfg.DatabasePath), 0o750); err != nil {
		logger.Error("create data directory", "error", err)
		os.Exit(1)
	}
	var fileLock *lock.FileLock
	// In gateway role, Gateway operates in concurrent HTTP-only mode and does not hold a singleton lock on gateway.lock.
	if cfg.RuntimeRole == config.RuntimeRoleMonolithDev {
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
	var store *storage.Store
	if cfg.RuntimeRole == config.RuntimeRoleMonolithDev {
		store, err = storage.Open(ctx, cfg.DatabasePath)
	} else {
		store, err = storage.OpenRuntime(ctx, cfg.DatabasePath)
	}
	if err != nil {
		logger.Error("open storage", "error", err)
		os.Exit(1)
	}
	defer store.Close()

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
			logger.Error("load notification encryption key", "error", err)
			os.Exit(1)
		}
		store.WithKeyring(keyring)
	}

	hub := eventhub.New()

	var server *httpapi.Server
	var monolithPayments *payments.Service
	var monolithDispatcher *notification.Dispatcher
	if cfg.RuntimeRole == config.RuntimeRoleGateway {
		logger.Info("starting gateway in HTTP-only mode with worker RPC", "workerRPCURL", cfg.WorkerRPCURL)
		workerClient := workerrpc.NewClient(cfg.WorkerRPCURL, cfg.WorkerInternalToken)
		server = httpapi.New(cfg, store).
			WithEventHub(hub).
			WithNotificationTester(workerClient).
			WithProviderReader(workerClient).
			WithWakeDispatcher(workerClient.WakeDispatcher).
			WithWorkerProber(workerClient)
		provider := paymentProvider(cfg, logger)
		service := payments.NewService(cfg, store, provider, server.PaymentCommitNotifier()).
			WithReconcileWake(workerClient.WakePaymentReconciler)
		server.WithPayments(service)
		if cfg.WorkerRealtimeEnabled {
			coordinator := httpapi.NewRealtimeCoordinator(server, time.Second)
			server.WithRealtimeSubmit(coordinator.Submit)
			go coordinator.Run(ctx)
			telemetry.Default.SetRealtimeStreamState(true, "connecting", "startup")
			streamClient, streamErr := realtimestream.NewClient(realtimestream.ClientConfig{
				BaseURL: cfg.WorkerRealtimeURL,
				Token:   cfg.WorkerInternalToken,
				OnConnect: func() {
					telemetry.Default.SetRealtimeStreamState(true, "connected", "")
					coordinator.RequestReconcile()
				},
				OnConnectError: func(err error) {
					telemetry.Default.SetRealtimeStreamState(true, "degraded", realtimeStreamReason(err))
				},
				OnDisconnect: func(err error) {
					telemetry.Default.RecordRealtimeDisconnect()
					telemetry.Default.SetRealtimeStreamState(true, "degraded", realtimeStreamReason(err))
				},
				OnReconnect: func() {
					telemetry.Default.RecordRealtimeReconnect()
					telemetry.Default.SetRealtimeStreamState(true, "connecting", "reconnect")
				},
			})
			if streamErr != nil {
				logger.Error("create worker realtime client failed", "error", streamErr)
				os.Exit(1)
			}
			go func() {
				err := streamClient.Run(ctx, coordinator.Submit)
				if ctx.Err() != nil {
					return
				}
				reason := realtimeStreamReason(err)
				telemetry.Default.SetRealtimeStreamState(true, "stopped", reason)
				logger.Error("worker realtime stream stopped", "reason", reason)
			}()
		} else {
			telemetry.Default.SetRealtimeStreamState(false, "disabled", "")
			go server.RunJournalWatcher(ctx, 200*time.Millisecond)
		}
	} else if cfg.RuntimeRole == config.RuntimeRoleMonolithDev {
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
			logger.Info("Bark notification provider registered in monolith-dev")
		}

		logger.Info("starting gateway in development monolith mode")
		dispatcher := notification.NewDispatcher(store, notificationRegistry)
		go dispatcher.Start(ctx)

		server = httpapi.New(cfg, store).
			WithEventHub(hub).
			WithBarkSender(barkSender).
			WithNotificationRegistry(notificationRegistry).
			WithWakeDispatcher(func(ctx context.Context) error {
				dispatcher.Wake()
				return nil
			})
		provider := paymentProvider(cfg, logger)
		service := payments.NewService(cfg, store, provider, server.PaymentCommitNotifier())
		server.WithPayments(service)
		go service.Start(ctx)
		monolithPayments = service
		monolithDispatcher = dispatcher

	}

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
		if monolithPayments != nil {
			if err := monolithPayments.Quiesce(shutdownCtx); err != nil {
				logger.Warn("payment drain did not complete", "error", err)
			}
			if err := monolithDispatcher.Drain(shutdownCtx); err != nil {
				logger.Warn("notification drain did not complete", "error", err)
			}
		}
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			logger.Error("server failed", "error", err)
			os.Exit(1)
		}
	}
}

// Missing development credentials disable payments without preventing history
// and administration from booting. Production configuration requires all keys.
func paymentProvider(cfg config.Config, logger *slog.Logger) payments.Provider {
	if cfg.PayOSClientID == "" || cfg.PayOSAPIKey == "" || cfg.PayOSChecksumKey == "" {
		return nil
	}
	provider, err := payments.NewPayOS(cfg.PayOSClientID, cfg.PayOSAPIKey, cfg.PayOSChecksumKey)
	if err != nil {
		logger.Error("create payment provider failed")
		os.Exit(1)
	}
	return provider
}
