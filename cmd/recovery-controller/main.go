package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/authbrowser"
	"github.com/thedemontuan/acb-transaction-webhook/internal/authrecovery"
	"github.com/thedemontuan/acb-transaction-webhook/internal/authsession"
	"github.com/thedemontuan/acb-transaction-webhook/internal/captchasolver"
	"github.com/thedemontuan/acb-transaction-webhook/internal/challenge"
	"github.com/thedemontuan/acb-transaction-webhook/internal/security"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
	"github.com/thedemontuan/acb-transaction-webhook/internal/telegramauth"
	"github.com/thedemontuan/acb-transaction-webhook/internal/workerrpc"
)

func main() {
	readiness := flag.Bool("readiness-check", false, "check local recovery readiness")
	preflight := flag.Bool("check-config", false, "check configured integrations without login or polling")
	flag.Parse()
	if flag.NArg() != 0 || *readiness && *preflight {
		fmt.Fprintln(os.Stderr, "RECOVERY_ARGUMENT_INVALID")
		os.Exit(2)
	}
	if *readiness {
		if err := checkReadiness(context.Background()); err != nil {
			fmt.Fprintln(os.Stderr, "RECOVERY_NOT_READY")
			os.Exit(1)
		}
		return
	}
	cfg, err := authrecovery.LoadConfig()
	if err != nil {
		slog.Error("recovery configuration invalid")
		os.Exit(2)
	}
	if !cfg.Enabled {
		fmt.Println("AUTH_RECOVERY_DISABLED")
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *preflight {
		if err := checkConfig(ctx, cfg); err != nil {
			slog.Error("recovery preflight failed", "reason", err.Error())
			os.Exit(1)
		}
		fmt.Println("PASS recovery configured integrations; vision is synthetic only")
		return
	}
	if err := authrecovery.RunSingleton(ctx, cfg, func(ctx context.Context) error { return run(ctx, cfg) }); err != nil {
		if errors.Is(err, authrecovery.ErrSingletonUnavailable) {
			slog.Error("RECOVERY_SINGLETON_UNAVAILABLE")
		} else {
			slog.Error("RECOVERY_RUNTIME_UNAVAILABLE")
		}
		os.Exit(1)
	}
}
func solverFor(cfg authrecovery.Config) (*captchasolver.NineRouter, error) {
	if !cfg.AICaptchaEnabled {
		return nil, nil
	}
	return captchasolver.NewNineRouter(captchasolver.Config{BaseURL: cfg.NineRouterBaseURL, Model: cfg.NineRouterCaptchaModel, APIKey: cfg.NineRouterAPIKey, Production: cfg.Production})
}
func checkConfig(ctx context.Context, cfg authrecovery.Config) error {
	bot, err := telegramauth.NewClient(cfg.TelegramBotToken, telegramauth.ClientOptions{})
	if err != nil {
		return errors.New("TELEGRAM_CONFIG_INVALID")
	}
	if _, err := bot.CheckConfig(ctx); err != nil {
		return err
	}
	if err := bot.CheckOperator(ctx, cfg.TelegramChatID, cfg.TelegramUserID); err != nil {
		return err
	}
	solver, err := solverFor(cfg)
	if err != nil {
		return errors.New("AI_CONFIG_INVALID")
	}
	if solver != nil {
		if err := solver.CheckConfig(ctx); err != nil {
			return errors.New("AI_VISION_PREFLIGHT_UNAVAILABLE")
		}
	}
	return nil
}
func run(ctx context.Context, cfg authrecovery.Config) error {
	store, err := storage.OpenRuntime(ctx, cfg.DatabasePath)
	if err != nil {
		return errors.New("RECOVERY_STORAGE_UNAVAILABLE")
	}
	defer store.Close()
	schema, err := store.SchemaVersion(ctx)
	if err != nil || schema.Version < 12 {
		return errors.New("RECOVERY_SCHEMA_REQUIRED")
	}
	var checksum string
	if err := store.DB().QueryRowContext(ctx, `SELECT checksum FROM schema_migrations WHERE version=12`).Scan(&checksum); err != nil || checksum != "2026-10-02-v12-auth-recovery" {
		return errors.New("RECOVERY_SCHEMA_INCOMPATIBLE")
	}
	keyring, err := security.LoadKeyring(cfg.MasterKeyFile)
	if err != nil {
		return errors.New("RECOVERY_KEY_UNAVAILABLE")
	}
	browser := authbrowser.NewClient(cfg.AuthBrowserURL, cfg.AuthBrowserInternalToken)
	worker := workerrpc.NewClient(cfg.WorkerRPCURL, cfg.WorkerInternalToken)
	bot, err := telegramauth.NewClient(cfg.TelegramBotToken, telegramauth.ClientOptions{})
	if err != nil {
		return errors.New("TELEGRAM_CONFIG_INVALID")
	}
	solver, err := solverFor(cfg)
	if err != nil {
		return errors.New("AI_CONFIG_INVALID")
	}
	broker := &challenge.Broker{Store: store, Browser: browser, Sender: bot, Config: challenge.Config{ChatID: cfg.TelegramChatID, CaptchaTTL: cfg.CaptchaTTL, OTPTTL: cfg.OTPTTL}}
	handler, err := telegramauth.NewHandler(telegramauth.HandlerOptions{Store: store, Client: bot, ChatID: cfg.TelegramChatID, UserID: cfg.TelegramUserID, PublicOrigin: cfg.PublicOrigin, ReplyBroker: broker, Browser: browser, Scheduler: worker})
	if err != nil {
		return errors.New("TELEGRAM_HANDLER_CONFIG_INVALID")
	}
	options := authrecovery.CoordinatorOptions{Config: cfg, Store: store, Browser: browser, Broker: broker, Finalizer: authsession.NewFinalizer(authsession.Options{Store: store, Browser: browser, Keyring: keyring, Verifier: worker, Scheduler: worker}), Telegram: bot, Notices: handler, WorkerReady: worker.Ready}
	if solver != nil {
		options.Solver = solver
	}
	coordinator, err := authrecovery.NewCoordinator(options)
	if err != nil {
		return errors.New("RECOVERY_COORDINATOR_INVALID")
	}
	handler.AIDegraded = coordinator.AIDegraded
	var initialized atomic.Bool
	initialized.Store(true)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Runtime-Role", "recovery-controller")
		healthCtx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if !initialized.Load() || store.Health(healthCtx) != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "not_ready"})
			return
		}
		transport := bot.Readiness()
		state := "ready"
		if !transport.Ready {
			state = "degraded"
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ready", "role": "recovery-controller", "telegram": state, "ai": coordinator.AIDegraded()})
	})
	health := &http.Server{Addr: ":8182", Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	serveErr := make(chan error, 1)
	go func() { serveErr <- health.ListenAndServe() }()
	runtimeCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	workersDone := make(chan struct{}, 2)
	go func() {
		defer func() { workersDone <- struct{}{} }()
		if err := bot.RunPolling(runtimeCtx, store, handler); err != nil && runtimeCtx.Err() == nil {
			slog.Warn("Telegram transport stopped; recovery requires operator repair")
		}
	}()
	go func() { defer func() { workersDone <- struct{}{} }(); _ = coordinator.Run(runtimeCtx) }()
	var runtimeErr error
	select {
	case <-ctx.Done():
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			runtimeErr = errors.New("RECOVERY_HEALTH_UNAVAILABLE")
		}
	}
	initialized.Store(false)
	cancel()
	shutdown, cancelShutdown := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancelShutdown()
	_ = health.Shutdown(shutdown)
	for range 2 {
		select {
		case <-workersDone:
		case <-shutdown.Done():
			return errors.New("RECOVERY_SHUTDOWN_TIMEOUT")
		}
	}
	return runtimeErr
}
func checkReadiness(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:8182/readyz", nil)
	if err != nil {
		return err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return errors.New("RECOVERY_NOT_READY")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return errors.New("RECOVERY_NOT_READY")
	}
	var payload struct {
		Status string `json:"status"`
		Role   string `json:"role"`
	}
	if json.NewDecoder(io.LimitReader(response.Body, 8<<10)).Decode(&payload) != nil || payload.Status != "ready" || payload.Role != "recovery-controller" {
		return errors.New("RECOVERY_NOT_READY")
	}
	return nil
}
