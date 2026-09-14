package httpapi

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/thedemontuan/acb-transaction-webhook/internal/auth"
	"github.com/thedemontuan/acb-transaction-webhook/internal/authbrowser"
	"github.com/thedemontuan/acb-transaction-webhook/internal/bark"
	"github.com/thedemontuan/acb-transaction-webhook/internal/config"
	"github.com/thedemontuan/acb-transaction-webhook/internal/eventhub"
	"github.com/thedemontuan/acb-transaction-webhook/internal/httpui"
	"github.com/thedemontuan/acb-transaction-webhook/internal/monitor"
	"github.com/thedemontuan/acb-transaction-webhook/internal/notification"
	"github.com/thedemontuan/acb-transaction-webhook/internal/security"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
	"github.com/thedemontuan/acb-transaction-webhook/internal/telemetry"
	"github.com/thedemontuan/acb-transaction-webhook/internal/ttsclient"
	"github.com/thedemontuan/acb-transaction-webhook/internal/workerrpc"
)

type SyncRequester interface {
	RequestSync(context.Context) error
}

type HistoryEnsurer interface {
	EnsureHistory(ctx context.Context, fromDay, toDay string) (int, error)
}

type HistoryJobManager interface {
	CreateHistoryJob(ctx context.Context, fromDay, toDay string) (storage.HistorySyncJob, error)
	CancelHistoryJob(ctx context.Context, jobID string) error
}

type MonitorNotifier interface {
	NotifySettingsChanged(ctx context.Context) error
}

type MonitorNotifierFunc func(ctx context.Context) error

func (f MonitorNotifierFunc) NotifySettingsChanged(ctx context.Context) error {
	return f(ctx)
}

type AuthVerifier interface {
	VerifySession(context.Context, string, int64, []byte) error
}

type WorkerProber interface {
	Ready(ctx context.Context) error
}

type NotificationChannelTester interface {
	TestNotificationChannel(ctx context.Context, channelID string) (workerrpc.TestNotificationResponse, error)
}

type WakeDispatcherFunc func(ctx context.Context) error

type Server struct {
	syncRequester     SyncRequester
	historyEnsurer    HistoryEnsurer
	historyJobManager HistoryJobManager
	monitorNotifier   MonitorNotifier
	authVerifier      AuthVerifier
	workerProber      WorkerProber
	channelTester     NotificationChannelTester
	cfg               config.Config
	store           *storage.Store
	auth            *auth.Middleware
	browser         *authbrowser.Client
	browserVNCURL   string
	keyring         *security.Keyring
	eventHub        *eventhub.Hub
	ttsClient       *ttsclient.Client
	barkSender      *bark.Sender
	notifRegistry   *notification.Registry
	wakeFn          WakeDispatcherFunc
	instanceNonce   string
	testCooldownMu  sync.Mutex
	lastTestPerCh   map[string]time.Time
	started         time.Time
	handler         http.Handler
}

func New(cfg config.Config, store *storage.Store) *Server {
	var keyring *security.Keyring
	if cfg.MasterKeyFile != "" {
		keyring, _ = security.LoadKeyring(cfg.MasterKeyFile)
	}
	var verifier auth.Verifier
	if cfg.Production {
		verifier = auth.NewCloudflareVerifier(cfg)
	}
	var ttsClientInstance *ttsclient.Client
	if cfg.TTSGatewayURL != "" {
		ttsClientInstance = ttsclient.New(cfg.TTSGatewayURL, cfg.TTSInternalToken)
	}
	nonceBytes := make([]byte, 16)
	_, _ = rand.Read(nonceBytes)
	instanceNonce := hex.EncodeToString(nonceBytes)

	s := &Server{
		cfg:           cfg,
		store:         store,
		auth:          auth.New(cfg, verifier),
		browser:       authbrowser.NewClient(cfg.AuthBrowserURL),
		browserVNCURL: cfg.AuthBrowserVNCURL,
		keyring:       keyring,
		eventHub:      eventhub.New(),
		ttsClient:     ttsClientInstance,
		instanceNonce: instanceNonce,
		started:       time.Now().UTC(),
	}
	r := chi.NewRouter()
	r.Use(requestID, s.platformHeaders, securityHeaders, recoverer)
	r.Get("/healthz", s.health)
	r.Get("/health", s.health)
	r.Get("/readyz", s.ready)
	r.Get("/ready", s.ready)
	r.Get("/internal/deployz", s.deployReady)
	r.Route("/api/v1", func(api chi.Router) {
		api.Use(s.auth.Require(auth.Owner, auth.Operator, auth.Viewer))
		api.Get("/status", s.status)
		api.Get("/csrf", auth.CSRF)
		api.Get("/connection", s.connection)
		api.Get("/webhooks", s.endpoints)
		api.Get("/transactions", s.transactions)
		api.Get("/transactions/{id}", s.transactionDetail)
		api.With(s.auth.Require(auth.Owner, auth.Operator), s.requireMutationAllowed).Post("/transactions/ensure-history", s.ensureHistory)
		api.Get("/transactions/history-sync-jobs/{id}", s.getHistoryJob)
		api.Get("/history-sync-jobs/{id}", s.getHistoryJob)
		api.With(s.auth.Require(auth.Owner, auth.Operator), s.requireMutationAllowed).Delete("/transactions/history-sync-jobs/{id}", s.cancelHistoryJob)
		api.With(s.auth.Require(auth.Owner, auth.Operator), s.requireMutationAllowed).Delete("/history-sync-jobs/{id}", s.cancelHistoryJob)
		api.With(s.auth.Require(auth.Owner, auth.Operator), s.requireMutationAllowed).Post("/transactions/history-sync-jobs/{id}/cancel", s.cancelHistoryJob)
		api.With(s.auth.Require(auth.Owner, auth.Operator), s.requireMutationAllowed).Post("/history-sync-jobs/{id}/cancel", s.cancelHistoryJob)
		api.Get("/deliveries", s.deliveries)
		api.Get("/poll-runs", s.pollRuns)
		api.Get("/audit", s.auditLogs)
		api.Get("/events", s.eventsStream)
		api.Get("/events/stream", s.eventsStream)
		api.Get("/realtime/status", s.realtimeStatus)
		api.Get("/monitor/settings", s.getMonitorSettings)
		api.With(s.auth.Require(auth.Owner, auth.Operator), s.requireMutationAllowed).Post("/monitor/settings", s.updateMonitorSettings)
		api.With(s.auth.Require(auth.Owner, auth.Operator), s.requireMutationAllowed).Put("/monitor/settings", s.updateMonitorSettings)
		api.Get("/payment-qr", s.getPaymentQR)
		api.Get("/payment-qr/image", s.getPaymentQRImage)
		api.With(s.auth.Require(auth.Owner, auth.Operator), s.requireMutationAllowed).Post("/payment-qr", s.savePaymentQR)
		api.With(s.auth.Require(auth.Owner, auth.Operator), s.requireMutationAllowed).Post("/payment-qr/upload", s.uploadPaymentQR)
		api.With(s.auth.Require(auth.Owner, auth.Operator), s.requireMutationAllowed).Post("/payment-qr/generate", s.generatePaymentQR)
		api.With(s.auth.Require(auth.Owner, auth.Operator), s.requireMutationAllowed).Delete("/payment-qr", s.deletePaymentQR)

		api.Get("/voice/settings", s.getVoiceSettings)
		api.With(s.auth.Require(auth.Owner, auth.Operator), s.requireMutationAllowed).Put("/voice/settings", s.updateVoiceSettings)
		api.Get("/voice/status", s.voiceStatus)
		api.Post("/voice/test", s.testVoiceAudio)
		api.Post("/voice/transactions/{id}", s.synthesizeTransactionAudio)
		api.Post("/voice/transactions/{id}/replay", s.replayTransactionAudio)
		api.Post("/voice/transactions/summary", s.synthesizeSummaryAudio)

		api.With(s.auth.Require(auth.Owner), s.requireMutationAllowed).Post("/connection/configure", s.configure)
		api.With(s.auth.Require(auth.Owner, auth.Operator), s.requireMutationAllowed).Post("/connection/{action:pause|resume|sync}", s.connectionAction)
		api.With(s.auth.Require(auth.Owner), s.requireMutationAllowed).Post("/connection/auth/start", s.startAuth)
		api.With(s.auth.Require(auth.Owner)).Get("/connection/auth/current", s.currentAuth)
		api.With(s.auth.Require(auth.Owner), s.requireMutationAllowed).Post("/connection/auth/cancel", s.cancelAuth)
		api.With(s.auth.Require(auth.Owner)).Get("/connection/auth/{attemptID}/status", s.authStatus)
		api.With(s.auth.Require(auth.Owner)).Handle("/connection/auth/{attemptID}/screen/*", http.HandlerFunc(s.browserScreen))

		api.With(s.auth.Require(auth.Owner), s.requireMutationAllowed).Post("/webhooks", s.createEndpoint)
		api.With(s.auth.Require(auth.Owner), s.requireMutationAllowed).Post("/webhooks/{id}/{action:enable|disable}", s.endpointAction)

		api.Get("/notification-providers", s.notificationProviders)
		api.Get("/notification-channels", s.notificationChannels)
		api.With(s.auth.Require(auth.Owner), s.requireMutationAllowed).Post("/notification-channels", s.createNotificationChannel)
		api.With(s.auth.Require(auth.Owner), s.requireMutationAllowed).Put("/notification-channels/{id}", s.updateNotificationChannel)
		api.With(s.auth.Require(auth.Owner), s.requireMutationAllowed).Post("/notification-channels/{id}/{action:enable|disable}", s.toggleNotificationChannel)
		api.With(s.auth.Require(auth.Owner), s.requireMutationAllowed).Post("/notification-channels/{id}/rotate-secret", s.rotateChannelSecret)
		api.With(s.auth.Require(auth.Owner)).Post("/notification-channels/{id}/test", s.testNotificationChannel)
		api.With(s.auth.Require(auth.Owner)).Post("/deliveries/{id}/replay", s.replayDelivery)
	})
	ui, err := fs.Sub(httpui.Files, "dist")
	if err == nil {
		r.Mount("/", spa(ui))
	}
	s.handler = r
	return s
}
func (s *Server) WithAuthVerifier(verifier AuthVerifier) *Server {
	s.authVerifier = verifier
	return s
}

func (s *Server) WithEventHub(hub *eventhub.Hub) *Server {
	s.eventHub = hub
	return s
}

func (s *Server) EventHub() *eventhub.Hub {
	return s.eventHub
}

func (s *Server) WithSyncRequester(requester SyncRequester) *Server {
	s.syncRequester = requester
	return s
}

func (s *Server) WithHistoryJobManager(mgr HistoryJobManager) *Server {
	s.historyJobManager = mgr
	return s
}

func (s *Server) WithHistoryEnsurer(ensurer HistoryEnsurer) *Server {
	s.historyEnsurer = ensurer
	if ensurer != nil && s.historyJobManager == nil {
		s.historyJobManager = &legacyHistoryEnsurerAdapter{ensurer: ensurer}
	}
	return s
}

func (s *Server) WithBarkSender(sender *bark.Sender) *Server {
	s.barkSender = sender
	return s
}

func (s *Server) WithNotificationRegistry(reg *notification.Registry) *Server {
	s.notifRegistry = reg
	return s
}

func (s *Server) WithNotificationTester(tester NotificationChannelTester) *Server {
	s.channelTester = tester
	return s
}

func (s *Server) WithWakeDispatcher(wake WakeDispatcherFunc) *Server {
	s.wakeFn = wake
	return s
}

func (s *Server) WithWorkerProber(wp WorkerProber) *Server {
	s.workerProber = wp
	return s
}

func (s *Server) WithMonitorNotifier(notifier MonitorNotifier) *Server {
	s.monitorNotifier = notifier
	return s
}

func (s *Server) WithTTSClient(client *ttsclient.Client) *Server {
	s.ttsClient = client
	return s
}

func (s *Server) Handler() http.Handler { return s.handler }
func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	if err := s.store.Health(r.Context()); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not_ready"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func (s *Server) deployReady(w http.ResponseWriter, r *http.Request) {
	reqToken := strings.TrimSpace(r.Header.Get("X-Worker-Internal-Token"))
	if reqToken == "" {
		if authHdr := r.Header.Get("Authorization"); strings.HasPrefix(authHdr, "Bearer ") {
			reqToken = strings.TrimSpace(strings.TrimPrefix(authHdr, "Bearer "))
		}
	}

	expectedToken := strings.TrimSpace(s.cfg.WorkerInternalToken)
	if expectedToken == "" || subtle.ConstantTimeCompare([]byte(reqToken), []byte(expectedToken)) != 1 {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	ctx := r.Context()
	resp := map[string]any{
		"release": s.cfg.ReleaseCommit,
		"slot":    s.cfg.Slot,
		"nonce":   s.instanceNonce,
	}

	status := "ready"

	// 1. Storage check
	if err := s.store.Health(ctx); err != nil {
		resp["storage"] = "unhealthy: " + err.Error()
		status = "not_ready"
	} else {
		resp["storage"] = "ready"
	}

	// 2. Schema check
	schemaReport, err := s.store.SchemaVersion(ctx)
	if err != nil {
		resp["schema"] = "incompatible: " + err.Error()
		status = "not_ready"
	} else {
		resp["schema"] = "compatible"
		resp["schemaVersion"] = schemaReport.Version
	}

	// 3. Worker check (if configured)
	if s.cfg.WorkerRPCURL != "" {
		if s.workerProber != nil {
			if err := s.workerProber.Ready(ctx); err != nil {
				resp["worker"] = "unreachable: " + err.Error()
				status = "not_ready"
			} else {
				resp["worker"] = "ready"
			}
		} else {
			resp["worker"] = "not_configured"
			status = "not_ready"
		}
	} else {
		resp["worker"] = "monolith"
	}

	// 4. Auth-browser check (if configured)
	if s.cfg.AuthBrowserURL != "" {
		client := &http.Client{Timeout: 1500 * time.Millisecond}
		res, err := client.Get(strings.TrimRight(s.cfg.AuthBrowserURL, "/") + "/healthz")
		if err != nil || res.StatusCode != http.StatusOK {
			resp["authBrowser"] = "unreachable"
		} else {
			resp["authBrowser"] = "ready"
			_ = res.Body.Close()
		}
	} else {
		resp["authBrowser"] = "disabled"
	}

	// 5. TTS gateway check (if configured)
	if s.cfg.TTSGatewayURL != "" {
		client := &http.Client{Timeout: 1500 * time.Millisecond}
		res, err := client.Get(strings.TrimRight(s.cfg.TTSGatewayURL, "/") + "/healthz")
		if err != nil || res.StatusCode != http.StatusOK {
			resp["tts"] = "unreachable"
			if status == "ready" {
				status = "degraded"
			}
		} else {
			resp["tts"] = "ready"
			_ = res.Body.Close()
		}
	} else {
		resp["tts"] = "disabled"
	}

	// 6. Mutation gate state
	gate, gateErr := s.store.GetDeploymentGate(ctx)
	if gateErr == nil && gate != nil {
		resp["mutationGate"] = gate.GateState
	} else {
		resp["mutationGate"] = "OPEN"
	}

	resp["status"] = status
	code := http.StatusOK
	if status == "not_ready" {
		code = http.StatusServiceUnavailable
	}
	writeJSON(w, code, resp)
}

func (s *Server) requireMutationAllowed(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := s.store.CheckMutationAllowed(r.Context()); err != nil {
			if errors.Is(err, storage.ErrMutationGateLocked) {
				w.Header().Set("Retry-After", "5")
				w.Header().Set("X-Mutation-Gate", "LOCKED")
				writeJSON(w, http.StatusServiceUnavailable, map[string]any{
					"error": "deployment in progress, mutations temporarily locked",
					"code":  "MUTATION_GATE_LOCKED",
				})
				return
			}
			writeError(w, http.StatusInternalServerError, "failed to check mutation gate: "+err.Error())
			return
		}
		next.ServeHTTP(w, r)
	})
}
func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	connection, err := s.store.Connection(r.Context())
	acb := map[string]any{"state": "UNCONFIGURED", "coverage": "NOT_STARTED", "lastSuccessfulPollAt": nil}
	if err == nil {
		acb["state"] = connection.State
		acb["accountMasked"] = connection.AccountMasked
		acb["generation"] = connection.Generation
		acb["coverage"] = "PENDING_ACB_POC"
		if poll, pollErr := s.store.LastSuccessfulPoll(r.Context(), connection.ID); pollErr == nil {
			acb["lastSuccessfulPollAt"] = poll.FinishedAt
		}

		monSettings, errSettings := s.store.GetMonitorSettings(r.Context())
		if errSettings == nil {
			resolved := storage.ResolveSchedule(time.Now(), &monSettings)
			acb["scheduleMode"] = resolved.Mode
			acb["scheduleWindow"] = resolved.ActiveWindow
			acb["scheduleMinSeconds"] = int(resolved.MinInterval.Seconds())
			acb["scheduleMaxSeconds"] = int(resolved.MaxInterval.Seconds())
		}
	} else if !errors.Is(err, storage.ErrNotFound) {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "storage_error"})
		return
	}
	summary, err := s.store.NotificationSummary(r.Context())
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "storage_error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"service":       "HEALTHY",
		"version":       "2.0.0-dev",
		"uptimeSeconds": int(time.Since(s.started).Seconds()),
		"acb":           acb,
		"storage":       map[string]string{"status": "READY"},
		"webhooks":      summary.ByProvider[notification.ProviderWebhook],
		"notifications": summary,
	})
}
func (s *Server) realtimeStatus(w http.ResponseWriter, r *http.Request) {
	if s.eventHub != nil {
		telemetry.Default.SetConnectedClients(int64(s.eventHub.SubscriberCount()))
	}
	writeJSON(w, http.StatusOK, telemetry.Default.Report())
}
func (s *Server) connection(w http.ResponseWriter, r *http.Request) {
	c, err := s.store.Connection(r.Context())
	if errors.Is(err, storage.ErrNotFound) {
		writeJSON(w, http.StatusOK, map[string]any{"configured": false})
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "storage_error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"configured": true, "connection": c})
}
func (s *Server) configure(w http.ResponseWriter, r *http.Request) {
	var in struct {
		AccountMasked string `json:"accountMasked"`
	}
	if !decode(w, r, &in) {
		return
	}
	c, err := s.store.ConfigureConnection(r.Context(), in.AccountMasked)
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	audit(s.store, r, "connection.configure", c.ID)
	s.publishStateEvent("connection.changed", c.ID, c)
	writeJSON(w, http.StatusCreated, c)
}
func (s *Server) connectionAction(w http.ResponseWriter, r *http.Request) {
	if chi.URLParam(r, "action") == "sync" {
		c, err := s.store.Connection(r.Context())
		if err != nil {
			writeError(w, http.StatusNotFound, "connection_not_found")
			return
		}
		if c.State != "MONITORING" {
			writeJSON(w, http.StatusConflict, map[string]string{"code": "SYNC_UNAVAILABLE", "error": "Chỉ có thể đồng bộ khi ACB đang theo dõi giao dịch. Phiên hiện tại được giữ nguyên."})
			return
		}
		if s.syncRequester == nil {
			writeError(w, http.StatusServiceUnavailable, "Bộ đồng bộ ACB chưa sẵn sàng.")
			return
		}
		if err := s.syncRequester.RequestSync(r.Context()); err != nil {
			if errors.Is(err, monitor.ErrSyncUnavailable) {
				writeJSON(w, http.StatusConflict, map[string]string{"code": "SYNC_UNAVAILABLE", "error": "Trạng thái ACB đã thay đổi. Vui lòng tải lại trước khi đồng bộ."})
			} else {
				writeError(w, http.StatusServiceUnavailable, "Không thể tiếp nhận yêu cầu đồng bộ ACB.")
			}
			return
		}
		audit(s.store, r, "connection.sync", c.ID)
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "ACCEPTED"})
		return
	}
	c, err := s.store.TransitionConnection(r.Context(), chi.URLParam(r, "action"))
	if errors.Is(err, storage.ErrNotFound) {
		writeError(w, http.StatusNotFound, "connection_not_found")
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	audit(s.store, r, "connection."+chi.URLParam(r, "action"), c.ID)
	s.publishStateEvent("connection.changed", c.ID, c)
	writeJSON(w, http.StatusAccepted, c)
}
func isTerminalAuthStatus(status string) bool {
	return status == "FAILED" || status == "EXPIRED" || status == "CANCELLED" || status == "VERIFIED"
}

func (s *Server) currentAuth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	identity, _ := auth.FromContext(r.Context())
	_, _ = s.store.ExpireStaleAuthAttempts(r.Context())

	activeAttempt, found, err := s.store.ActiveAuthAttemptForOwner(r.Context(), identity.Email)
	if err != nil || !found {
		writeJSON(w, http.StatusOK, map[string]any{"attempt": nil})
		return
	}

	session, err := s.browser.Status(r.Context(), activeAttempt.ID)
	if err != nil {
		if authbrowser.IsHTTPStatus(err, http.StatusNotFound) {
			_ = s.store.FinishAuthAttempt(r.Context(), activeAttempt.ID, "FAILED")
			writeJSON(w, http.StatusOK, map[string]any{"attempt": nil})
			return
		}
		screenURL := "/api/v1/connection/auth/" + activeAttempt.ID + "/screen/vnc.html?autoconnect=true&resize=remote&path=api/v1/connection/auth/" + activeAttempt.ID + "/screen/websockify"
		writeJSON(w, http.StatusOK, map[string]any{
			"attempt": map[string]any{
				"attemptId":          activeAttempt.ID,
				"status":             activeAttempt.Status,
				"screenUrl":          screenURL,
				"expiresAt":          activeAttempt.ExpiresAt,
				"browserUnavailable": true,
			},
		})
		return
	}

	if isTerminalAuthStatus(session.Status) {
		_ = s.store.FinishAuthAttempt(r.Context(), activeAttempt.ID, session.Status)
		writeJSON(w, http.StatusOK, map[string]any{"attempt": nil})
		return
	}

	screenURL := "/api/v1/connection/auth/" + activeAttempt.ID + "/screen/vnc.html?autoconnect=true&resize=remote&path=api/v1/connection/auth/" + activeAttempt.ID + "/screen/websockify"
	writeJSON(w, http.StatusOK, map[string]any{
		"attempt": map[string]any{
			"attemptId": activeAttempt.ID,
			"status":    activeAttempt.Status,
			"screenUrl": screenURL,
			"expiresAt": activeAttempt.ExpiresAt,
		},
	})
}

func (s *Server) startAuth(w http.ResponseWriter, r *http.Request) {
	identity, _ := auth.FromContext(r.Context())
	_, _ = s.store.ExpireStaleAuthAttempts(r.Context())

	// If there is already an active attempt for this owner, resume it if browser is still alive
	activeAttempt, found, err := s.store.ActiveAuthAttemptForOwner(r.Context(), identity.Email)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Lỗi kiểm tra phiên đăng nhập ACB: "+err.Error())
		return
	}
	if found {
		session, err := s.browser.Status(r.Context(), activeAttempt.ID)
		if err == nil {
			if !isTerminalAuthStatus(session.Status) {
				session.ScreenURL = "/api/v1/connection/auth/" + activeAttempt.ID + "/screen/vnc.html?autoconnect=true&resize=remote&path=api/v1/connection/auth/" + activeAttempt.ID + "/screen/websockify"
				audit(s.store, r, "auth.resume", activeAttempt.ID)
				s.publishStateEvent("auth.changed", activeAttempt.ID, map[string]any{"attemptId": activeAttempt.ID, "status": activeAttempt.Status})
				writeJSON(w, http.StatusOK, session)
				return
			}
			_ = s.store.FinishAuthAttempt(r.Context(), activeAttempt.ID, session.Status)
		} else if authbrowser.IsHTTPStatus(err, http.StatusNotFound) {
			_ = s.store.FinishAuthAttempt(r.Context(), activeAttempt.ID, "FAILED")
		} else {
			slog.Warn("transient error querying auth browser status for active attempt", "attempt_id", activeAttempt.ID, "error", err)
			writeError(w, http.StatusServiceUnavailable, "Không thể kết nối đến trình duyệt ACB. Vui lòng thử lại sau giây lát.")
			return
		}
	}

	attempt, err := s.store.StartAuthAttempt(r.Context(), identity.Email, 15*time.Minute)
	if err != nil {
		if errors.Is(err, storage.ErrAuthAttemptActive) || strings.Contains(err.Error(), "database is locked") || strings.Contains(err.Error(), "SQLITE_BUSY") {
			writeError(w, http.StatusConflict, "Một phiên đăng nhập ACB đang được thực hiện bởi quản trị viên khác.")
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	session, err := s.browser.Start(r.Context(), attempt.ID)
	if err != nil {
		slog.Warn("failed to start ACB browser session", "attempt_id", attempt.ID, "error", err)
		_ = s.store.FinishAuthAttempt(r.Context(), attempt.ID, "FAILED")
		writeError(w, http.StatusServiceUnavailable, "Không thể khởi động trình duyệt ACB. Vui lòng thử lại.")
		return
	}
	if err := s.store.MarkAuthAttemptInProgress(r.Context(), attempt.ID); err != nil {
		_ = s.browser.Cancel(r.Context(), attempt.ID)
		_ = s.store.FinishAuthAttempt(r.Context(), attempt.ID, "FAILED")
		writeJSON(w, http.StatusConflict, map[string]string{"code": "AUTH_SESSION_SUPERSEDED", "error": "Phiên đăng nhập ACB đã được thay thế. Vui lòng mở phiên mới."})
		return
	}
	session.ScreenURL = "/api/v1/connection/auth/" + attempt.ID + "/screen/vnc.html?autoconnect=true&resize=remote&path=api/v1/connection/auth/" + attempt.ID + "/screen/websockify"
	audit(s.store, r, "auth.start", attempt.ID)
	s.publishStateEvent("auth.changed", attempt.ID, map[string]any{"attemptId": attempt.ID, "status": "IN_PROGRESS"})
	writeJSON(w, http.StatusCreated, session)
}
func (s *Server) cancelAuth(w http.ResponseWriter, r *http.Request) {
	var in struct {
		AttemptID string `json:"attemptId"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.AttemptID == "" {
		writeError(w, http.StatusBadRequest, "attemptId is required")
		return
	}
	identity, _ := auth.FromContext(r.Context())
	attempt, err := s.store.AuthAttemptStatusForOwner(r.Context(), in.AttemptID, identity.Email)
	if err != nil {
		writeError(w, http.StatusNotFound, "ACB browser session not found")
		return
	}
	if attempt.Status == "CANCELLED" {
		writeJSON(w, http.StatusOK, map[string]string{"status": "CANCELLED"})
		return
	}
	if attempt.Status != "STARTING" && attempt.Status != "IN_PROGRESS" {
		writeError(w, http.StatusBadRequest, "auth attempt cannot be cancelled")
		return
	}
	if err := s.browser.Cancel(r.Context(), in.AttemptID); err != nil && !authbrowser.IsHTTPStatus(err, http.StatusNotFound) {
		slog.Warn("failed to cancel ACB browser session upstream", "attempt_id", in.AttemptID, "error", err)
		writeError(w, http.StatusBadGateway, "Không thể kết nối dịch vụ trình duyệt ACB. Vui lòng thử lại.")
		return
	}
	err = s.store.FinishAuthAttempt(r.Context(), in.AttemptID, "CANCELLED")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	audit(s.store, r, "auth.cancel", in.AttemptID)
	s.publishStateEvent("auth.changed", in.AttemptID, map[string]any{"attemptId": in.AttemptID, "status": "CANCELLED"})
	writeJSON(w, http.StatusOK, map[string]string{"status": "CANCELLED"})
}
func (s *Server) authStatus(w http.ResponseWriter, r *http.Request) {
	identity, _ := auth.FromContext(r.Context())
	attemptID := chi.URLParam(r, "attemptID")
	attempt, err := s.store.AuthAttemptStatusForOwner(r.Context(), attemptID, identity.Email)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"code": "AUTH_SESSION_NOT_FOUND", "error": "Không tìm thấy phiên đăng nhập ACB. Vui lòng mở phiên mới."})
		return
	}

	switch attempt.Status {
	case "VERIFIED":
		conn, err := s.store.Connection(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, "storage_error")
			return
		}
		if conn.Generation != attempt.Generation || conn.State != "MONITORING" {
			writeJSON(w, http.StatusConflict, map[string]string{"code": "AUTH_SESSION_SUPERSEDED", "error": "Phiên đăng nhập ACB đã được thay thế. Vui lòng tải lại trạng thái kết nối."})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "MONITORING"})
		return
	case "CANCELLED":
		writeJSON(w, http.StatusOK, map[string]string{"status": "CANCELLED"})
		return
	case "EXPIRED":
		writeJSON(w, http.StatusOK, map[string]string{
			"status": "EXPIRED",
			"error":  "Phiên đăng nhập ACB đã hết hạn. Vui lòng mở phiên mới.",
		})
		return
	case "FAILED":
		writeJSON(w, http.StatusOK, map[string]string{
			"status": "FAILED",
			"error":  "Phiên trình duyệt ACB đã kết thúc. Vui lòng mở phiên mới.",
		})
		return
	}

	expiresAt, parseErr := time.Parse(time.RFC3339Nano, attempt.ExpiresAt)
	if parseErr != nil {
		expiresAt, parseErr = time.Parse(time.RFC3339, attempt.ExpiresAt)
	}
	if parseErr == nil && !expiresAt.IsZero() && !time.Now().UTC().Before(expiresAt) {
		_ = s.browser.Cancel(r.Context(), attemptID)
		_ = s.store.FinishAuthAttempt(r.Context(), attemptID, "EXPIRED")
		writeJSON(w, http.StatusOK, map[string]string{
			"status": "EXPIRED",
			"error":  "Phiên đăng nhập ACB đã hết hạn. Vui lòng mở phiên mới.",
		})
		return
	}

	conn, err := s.store.Connection(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if conn.Generation != attempt.Generation {
		_ = s.browser.Cancel(r.Context(), attemptID)
		_ = s.store.FinishAuthAttempt(r.Context(), attemptID, "FAILED")
		writeJSON(w, http.StatusConflict, map[string]string{"code": "AUTH_SESSION_SUPERSEDED", "error": "Phiên đăng nhập ACB đã được thay thế. Vui lòng mở phiên mới."})
		return
	}

	session, err := s.browser.Status(r.Context(), attemptID)
	if err != nil {
		if authbrowser.IsHTTPStatus(err, http.StatusNotFound) {
			slog.Warn("ACB browser session missing or ended upstream", "attempt_id", attemptID, "error", err)
			_ = s.store.FinishAuthAttempt(r.Context(), attemptID, "FAILED")
			writeJSON(w, http.StatusOK, map[string]string{
				"status": "FAILED",
				"error":  "Phiên trình duyệt ACB đã kết thúc. Vui lòng mở phiên mới.",
			})
			return
		}
		writeJSON(w, http.StatusBadGateway, map[string]string{"code": "AUTH_SESSION_UNAVAILABLE", "error": "Không thể kết nối dịch vụ trình duyệt ACB. Vui lòng thử lại."})
		return
	}

	latestConn, err := s.store.Connection(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if latestConn.Generation != attempt.Generation {
		_ = s.browser.Cancel(r.Context(), attemptID)
		_ = s.store.FinishAuthAttempt(r.Context(), attemptID, "FAILED")
		writeJSON(w, http.StatusConflict, map[string]string{"code": "AUTH_SESSION_SUPERSEDED", "error": "Phiên đăng nhập ACB đã được thay thế. Vui lòng mở phiên mới."})
		return
	}

	if session.Status == "FAILED" || session.Status == "EXPIRED" {
		_ = s.store.FinishAuthAttempt(r.Context(), attemptID, session.Status)
		message := "Không thể khởi động trình duyệt ACB. Vui lòng mở phiên mới."
		if session.Status == "EXPIRED" {
			message = "Phiên đăng nhập ACB đã hết hạn. Vui lòng mở phiên mới."
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": session.Status, "error": message})
		return
	}

	if session.Status == "CANCELLED" {
		_ = s.store.FinishAuthAttempt(r.Context(), attemptID, "CANCELLED")
		writeJSON(w, http.StatusOK, map[string]string{"status": "CANCELLED"})
		return
	}

	if session.Status == "VERIFIED" {
		if s.keyring == nil {
			writeError(w, http.StatusServiceUnavailable, "session encryption is unavailable")
			return
		}
		handoff, err := s.browser.Handoff(r.Context(), attemptID)
		if err != nil {
			writeError(w, http.StatusBadGateway, "ACB browser session handoff failed")
			return
		}
		envelope, err := s.keyring.Encrypt([]byte(handoff), []byte("acb-session:"+attempt.ConnectionID))
		if err != nil {
			writeError(w, http.StatusInternalServerError, "session encryption failed")
			return
		}
		encrypted, err := json.Marshal(envelope)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "session encoding failed")
			return
		}
		if s.authVerifier == nil {
			writeError(w, http.StatusServiceUnavailable, "ACB session verification is unavailable")
			return
		}
		preVerifyConn, err := s.store.Connection(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if preVerifyConn.Generation != attempt.Generation {
			_ = s.browser.Cancel(r.Context(), attemptID)
			_ = s.store.FinishAuthAttempt(r.Context(), attemptID, "FAILED")
			writeJSON(w, http.StatusConflict, map[string]string{"code": "AUTH_SESSION_SUPERSEDED", "error": "Phiên đăng nhập ACB đã được thay thế. Vui lòng mở phiên mới."})
			return
		}
		if err := s.authVerifier.VerifySession(r.Context(), attempt.ConnectionID, attempt.Generation, encrypted); err != nil {
			slog.Warn("ACB HTTP session verification pending or failed", "attempt_id", attemptID, "generation", attempt.Generation, "error", err)
			writeJSON(w, http.StatusOK, map[string]string{
				"status": "VERIFYING",
				"error":  "Đã đăng nhập ACB thành công. Vui lòng bấm vào tài khoản thanh toán trên màn hình để kết nối lịch sử giao dịch.",
			})
			return
		}
		completedConn, err := s.store.CompleteAuthSession(r.Context(), attemptID, encrypted)
		if err != nil {
			_ = s.browser.Cancel(r.Context(), attemptID)
			_ = s.store.FinishAuthAttempt(r.Context(), attemptID, "FAILED")
			writeJSON(w, http.StatusConflict, map[string]string{"code": "AUTH_SESSION_SUPERSEDED", "error": "Phiên đăng nhập ACB đã được thay thế. Vui lòng mở phiên mới."})
			return
		}
		if err := s.browser.Complete(r.Context(), attemptID); err != nil {
			slog.Warn("complete ACB browser handoff", "attempt_id", attemptID, "error", err)
		}
		audit(s.store, r, "auth.verified", completedConn.ID)
		s.publishStateEvent("connection.changed", completedConn.ID, completedConn)
		s.publishStateEvent("auth.changed", attemptID, map[string]any{"attemptId": attemptID, "status": "MONITORING"})
		writeJSON(w, http.StatusOK, map[string]string{"status": "MONITORING"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": session.Status})
}

func (s *Server) browserScreen(w http.ResponseWriter, r *http.Request) {
	identity, _ := auth.FromContext(r.Context())
	attemptID := chi.URLParam(r, "attemptID")
	if _, err := s.store.AuthAttemptForOwner(r.Context(), attemptID, identity.Email); err != nil {
		writeError(w, http.StatusNotFound, "ACB browser session not found")
		return
	}
	browserURL, err := url.Parse(s.browserVNCURL)
	if err != nil || browserURL.Scheme == "" || browserURL.Host == "" {
		writeError(w, http.StatusServiceUnavailable, "ACB browser screen unavailable")
		return
	}
	proxy := httputil.NewSingleHostReverseProxy(browserURL)
	originalDirector := proxy.Director
	proxy.Director = func(request *http.Request) {
		originalDirector(request)
		request.URL.Path = "/" + strings.TrimPrefix(chi.URLParam(r, "*"), "/")
		request.Host = browserURL.Host
	}
	proxy.ModifyResponse = func(response *http.Response) error {
		if chi.URLParam(r, "*") == "vnc.html" && response.StatusCode == http.StatusOK {
			w.Header().Del("Content-Security-Policy")
			response.Header.Set("Content-Security-Policy", defaultContentSecurityPolicy+"; img-src 'self' data:")
		}
		return nil
	}
	proxy.ErrorHandler = func(rw http.ResponseWriter, _ *http.Request, _ error) {
		writeError(rw, http.StatusBadGateway, "ACB browser screen unavailable")
	}
	proxy.ServeHTTP(w, r)
}

func pageParams(r *http.Request) (int, string, error) {
	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 100 {
			return 0, "", errors.New("limit must be between 1 and 100")
		}
		limit = parsed
	}
	return limit, r.URL.Query().Get("cursor"), nil
}

func (s *Server) transactions(w http.ResponseWriter, r *http.Request) {
	limit, cursor, err := pageParams(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if len(q) > 200 {
		q = q[:200]
	}
	from := strings.TrimSpace(r.URL.Query().Get("from"))
	to := strings.TrimSpace(r.URL.Query().Get("to"))
	direction := strings.TrimSpace(r.URL.Query().Get("direction"))
	if direction != "credit" && direction != "debit" {
		direction = "all"
	}
	if from != "" {
		if _, err := time.Parse("2006-01-02", from); err != nil {
			writeError(w, http.StatusBadRequest, "invalid from date: expected YYYY-MM-DD")
			return
		}
	}
	if to != "" {
		if _, err := time.Parse("2006-01-02", to); err != nil {
			writeError(w, http.StatusBadRequest, "invalid to date: expected YYYY-MM-DD")
			return
		}
	}
	if from != "" && to != "" && from > to {
		writeError(w, http.StatusBadRequest, "from date must not be after to date")
		return
	}

	filter := storage.TransactionFilter{
		From:      from,
		To:        to,
		Direction: direction,
		Query:     q,
		Limit:     limit,
		Cursor:    cursor,
	}
	page, err := s.store.ListTransactionsFiltered(r.Context(), filter)
	if err != nil {
		status := http.StatusInternalServerError
		if cursor != "" {
			status = http.StatusBadRequest
		}
		writeError(w, status, "query transactions failed")
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (s *Server) transactionDetail(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "transaction id is required")
		return
	}
	txn, err := s.store.GetTransactionByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "transaction not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to get transaction")
		return
	}
	writeJSON(w, http.StatusOK, txn)
}

type ensureHistoryRequest struct {
	From string `json:"from"`
	To   string `json:"to"`
}

func (s *Server) ensureHistory(w http.ResponseWriter, r *http.Request) {
	var req ensureHistoryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeStandardError(w, r, http.StatusBadRequest, "INVALID_RANGE", "Dữ liệu yêu cầu không hợp lệ.")
		return
	}
	req.From = strings.TrimSpace(req.From)
	req.To = strings.TrimSpace(req.To)
	if req.From == "" || req.To == "" {
		writeStandardError(w, r, http.StatusBadRequest, "INVALID_RANGE", "Ngày bắt đầu và kết thúc là bắt buộc (YYYY-MM-DD).")
		return
	}
	fromT, err := time.Parse("2006-01-02", req.From)
	if err != nil {
		writeStandardError(w, r, http.StatusBadRequest, "INVALID_RANGE", "Định dạng ngày bắt đầu không hợp lệ (YYYY-MM-DD).")
		return
	}
	toT, err := time.Parse("2006-01-02", req.To)
	if err != nil {
		writeStandardError(w, r, http.StatusBadRequest, "INVALID_RANGE", "Định dạng ngày kết thúc không hợp lệ (YYYY-MM-DD).")
		return
	}
	if fromT.After(toT) {
		writeStandardError(w, r, http.StatusBadRequest, "INVALID_RANGE", "Ngày bắt đầu không được sau ngày kết thúc.")
		return
	}
	if toT.Sub(fromT) > 31*24*time.Hour {
		writeStandardError(w, r, http.StatusBadRequest, "INVALID_RANGE", "Khoảng thời gian vượt quá giới hạn tối đa 31 ngày.")
		return
	}

	conn, err := s.store.Connection(r.Context())
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeStandardError(w, r, http.StatusBadRequest, "CONNECTION_NOT_READY", "Chưa cấu hình tài khoản ACB.")
			return
		}
		writeStandardError(w, r, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "Không thể kiểm tra kết nối tài khoản ACB.")
		return
	}
	if conn.State != "MONITORING" {
		writeStandardError(w, r, http.StatusBadRequest, "CONNECTION_NOT_READY", "Tài khoản ACB chưa sẵn sàng hoặc chưa đăng nhập.")
		return
	}

	// Check if the requested range is already complete in database
	covered, err := s.store.CheckRangeCoverage(r.Context(), conn.ID, req.From, req.To)
	if err == nil && covered {
		writeJSON(w, http.StatusOK, map[string]any{
			"status":   "COMPLETED",
			"coverage": "COMPLETE",
			"synced":   false,
			"job":      nil,
		})
		return
	}

	mgr := s.getHistoryJobManager()
	if mgr == nil {
		writeStandardError(w, r, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "Dịch vụ đồng bộ lịch sử tạm thời không khả dụng.")
		return
	}

	job, err := mgr.CreateHistoryJob(r.Context(), req.From, req.To)
	if err != nil {
		slog.Warn("failed to create history job", "from", req.From, "to", req.To, "error", err)
		writeStandardError(w, r, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "Không thể khởi tạo tiến trình đồng bộ: "+err.Error())
		return
	}

	audit(s.store, r, "history_sync.create", job.ID)
	s.publishStateEvent("history_sync.queued", job.ID, job)

	writeJSON(w, http.StatusAccepted, map[string]any{
		"id":       job.ID,
		"status":   job.Status,
		"coverage": "PENDING",
		"synced":   false,
		"job":      job,
	})
}

func (s *Server) getHistoryJob(w http.ResponseWriter, r *http.Request) {
	jobID := chi.URLParam(r, "id")
	if jobID == "" {
		writeStandardError(w, r, http.StatusBadRequest, "INVALID_REQUEST", "Mã công việc không được để trống.")
		return
	}
	if jobID == "latest" {
		s.getLatestHistoryJob(w, r)
		return
	}

	conn, err := s.store.Connection(r.Context())
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		writeStandardError(w, r, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "Lỗi kiểm tra kết nối.")
		return
	}

	job, err := s.store.GetHistorySyncJob(r.Context(), jobID)
	if err != nil {
		if errors.Is(err, storage.ErrJobNotFound) {
			writeStandardError(w, r, http.StatusNotFound, "JOB_NOT_FOUND", "Không tìm thấy tiến trình đồng bộ.")
			return
		}
		writeStandardError(w, r, http.StatusInternalServerError, "INTERNAL_ERROR", "Lỗi truy vấn tiến trình đồng bộ.")
		return
	}

	// Validate ownership/context: singleton connection
	if (conn.ID != "" && job.ConnectionID != "" && job.ConnectionID != conn.ID) || (conn.ID == "" && job.ConnectionID != "") {
		writeStandardError(w, r, http.StatusNotFound, "JOB_NOT_FOUND", "Không tìm thấy tiến trình đồng bộ.")
		return
	}

	writeJSON(w, http.StatusOK, job)
}

func (s *Server) getLatestHistoryJob(w http.ResponseWriter, r *http.Request) {
	conn, err := s.store.Connection(r.Context())
	if err != nil || conn.ID == "" {
		writeStandardError(w, r, http.StatusNotFound, "JOB_NOT_FOUND", "Chưa có kết nối ACB.")
		return
	}

	job, found, err := s.store.GetLatestHistorySyncJob(r.Context(), conn.ID)
	if err != nil || !found {
		writeStandardError(w, r, http.StatusNotFound, "JOB_NOT_FOUND", "Chưa có tiến trình đồng bộ nào.")
		return
	}

	writeJSON(w, http.StatusOK, job)
}

func (s *Server) cancelHistoryJob(w http.ResponseWriter, r *http.Request) {
	jobID := chi.URLParam(r, "id")
	if jobID == "" {
		writeStandardError(w, r, http.StatusBadRequest, "INVALID_REQUEST", "Mã công việc không được để trống.")
		return
	}

	conn, err := s.store.Connection(r.Context())
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		writeStandardError(w, r, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "Lỗi kiểm tra kết nối.")
		return
	}

	job, err := s.store.GetHistorySyncJob(r.Context(), jobID)
	if err != nil {
		if errors.Is(err, storage.ErrJobNotFound) {
			writeStandardError(w, r, http.StatusNotFound, "JOB_NOT_FOUND", "Không tìm thấy tiến trình đồng bộ.")
			return
		}
		writeStandardError(w, r, http.StatusInternalServerError, "INTERNAL_ERROR", "Lỗi truy vấn tiến trình đồng bộ.")
		return
	}

	if (conn.ID != "" && job.ConnectionID != "" && job.ConnectionID != conn.ID) || (conn.ID == "" && job.ConnectionID != "") {
		writeStandardError(w, r, http.StatusNotFound, "JOB_NOT_FOUND", "Không tìm thấy tiến trình đồng bộ.")
		return
	}

	if job.Status == storage.HistoryJobStatusCompleted || job.Status == storage.HistoryJobStatusFailed {
		writeStandardError(w, r, http.StatusConflict, "JOB_TERMINAL", "Tiến trình đồng bộ đã ở trạng thái kết thúc, không thể hủy.")
		return
	}

	mgr := s.getHistoryJobManager()
	if mgr == nil {
		writeStandardError(w, r, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "Dịch vụ đồng bộ lịch sử tạm thời không khả dụng.")
		return
	}

	if err := mgr.CancelHistoryJob(r.Context(), jobID); err != nil {
		if errors.Is(err, storage.ErrJobNotFound) {
			writeStandardError(w, r, http.StatusNotFound, "JOB_NOT_FOUND", "Không tìm thấy tiến trình đồng bộ.")
			return
		}
		if errors.Is(err, storage.ErrJobTerminal) {
			writeStandardError(w, r, http.StatusConflict, "JOB_TERMINAL", "Tiến trình đồng bộ đã ở trạng thái kết thúc, không thể hủy.")
			return
		}
		writeStandardError(w, r, http.StatusInternalServerError, "INTERNAL_ERROR", "Không thể hủy tiến trình đồng bộ: "+err.Error())
		return
	}

	audit(s.store, r, "history_sync.cancel", jobID)
	s.publishStateEvent("history_sync.canceled", jobID, map[string]any{"id": jobID, "status": "CANCELED"})

	job.Status = storage.HistoryJobStatusCanceled
	writeJSON(w, http.StatusAccepted, job)
}

func (s *Server) getHistoryJobManager() HistoryJobManager {
	if s.historyJobManager != nil {
		return s.historyJobManager
	}
	if s.store != nil {
		return &defaultStoreHistoryJobManager{store: s.store}
	}
	return nil
}

type defaultStoreHistoryJobManager struct {
	store *storage.Store
}

func (m *defaultStoreHistoryJobManager) CreateHistoryJob(ctx context.Context, fromDay, toDay string) (storage.HistorySyncJob, error) {
	conn, err := m.store.Connection(ctx)
	if err != nil {
		return storage.HistorySyncJob{}, fmt.Errorf("lookup connection: %w", err)
	}
	if conn.State != "MONITORING" {
		return storage.HistorySyncJob{}, errors.New("bank connection is not in MONITORING state")
	}
	job, _, err := m.store.CreateOrGetHistorySyncJob(ctx, conn.ID, conn.Generation, fromDay, toDay)
	return job, err
}

func (m *defaultStoreHistoryJobManager) CancelHistoryJob(ctx context.Context, jobID string) error {
	return m.store.CancelHistorySyncJob(ctx, jobID)
}

type legacyHistoryEnsurerAdapter struct {
	ensurer HistoryEnsurer
}

func (a *legacyHistoryEnsurerAdapter) CreateHistoryJob(ctx context.Context, fromDay, toDay string) (storage.HistorySyncJob, error) {
	rowsSeen, err := a.ensurer.EnsureHistory(ctx, fromDay, toDay)
	if err != nil {
		return storage.HistorySyncJob{}, err
	}
	return storage.HistorySyncJob{
		ID:        "syncjob_legacy",
		Status:    storage.HistoryJobStatusCompleted,
		RangeFrom: fromDay,
		RangeTo:   toDay,
		RowsSeen:  rowsSeen,
		PagesDone: 1,
	}, nil
}

func (a *legacyHistoryEnsurerAdapter) CancelHistoryJob(ctx context.Context, jobID string) error {
	return nil
}

func (s *Server) getMonitorSettings(w http.ResponseWriter, r *http.Request) {
	settings, err := s.store.GetMonitorSettings(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load monitor settings")
		return
	}
	resolved := storage.ResolveSchedule(time.Now(), &settings)
	writeJSON(w, http.StatusOK, map[string]any{
		"settings": settings,
		"current": map[string]any{
			"mode":             resolved.Mode,
			"minSeconds":       int(resolved.MinInterval.Seconds()),
			"maxSeconds":       int(resolved.MaxInterval.Seconds()),
			"activeWindow":     resolved.ActiveWindow,
			"nextTransitionAt": resolved.NextTransition.Format(time.RFC3339),
			"nextMode":         resolved.NextMode,
		},
	})
}

func (s *Server) updateMonitorSettings(w http.ResponseWriter, r *http.Request) {
	var input storage.MonitorSettings
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	saved, err := s.store.SaveMonitorSettings(r.Context(), input)
	if err != nil {
		if errors.Is(err, storage.ErrSettingsConflict) {
			writeError(w, http.StatusConflict, "settings conflict: revision mismatch, please reload and retry")
			return
		}
		writeError(w, http.StatusBadRequest, "invalid settings: "+err.Error())
		return
	}

	audit(s.store, r, "monitor.settings.update", "singleton")

	var wakeWarning string
	if s.monitorNotifier != nil {
		if err := s.monitorNotifier.NotifySettingsChanged(r.Context()); err != nil {
			slog.Warn("monitor settings wake degraded", "error", err)
			wakeWarning = "worker wake degraded; settings saved"
		}
	}

	resolved := storage.ResolveSchedule(time.Now(), &saved)
	respData := map[string]any{
		"settings": saved,
		"current": map[string]any{
			"mode":             resolved.Mode,
			"minSeconds":       int(resolved.MinInterval.Seconds()),
			"maxSeconds":       int(resolved.MaxInterval.Seconds()),
			"activeWindow":     resolved.ActiveWindow,
			"nextTransitionAt": resolved.NextTransition.Format(time.RFC3339),
			"nextMode":         resolved.NextMode,
		},
	}
	if wakeWarning != "" {
		respData["warning"] = wakeWarning
	}
	writeJSON(w, http.StatusOK, respData)
}

func (s *Server) getPaymentQR(w http.ResponseWriter, r *http.Request) {
	qr, err := s.store.GetPaymentQR(r.Context(), "")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to get payment qr")
		return
	}
	if qr == nil {
		writeJSON(w, http.StatusOK, map[string]any{"configured": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"configured": true,
		"qr":         qr,
		"hasImage":   qr.ImagePath != "",
		"imageURL":   "/api/v1/payment-qr/image?v=" + strconv.FormatInt(qr.Revision, 10),
	})
}

func (s *Server) getPaymentQRImage(w http.ResponseWriter, r *http.Request) {
	qr, err := s.store.GetPaymentQR(r.Context(), "")
	if err != nil || qr == nil || qr.ImagePath == "" {
		writeError(w, http.StatusNotFound, "payment qr image not found")
		return
	}

	cleanedPath := filepath.Clean(qr.ImagePath)
	dataDir := filepath.Dir(s.cfg.DatabasePath)
	qrDir := filepath.Clean(filepath.Join(dataDir, "qr"))
	rel, err := filepath.Rel(qrDir, cleanedPath)
	if err != nil || strings.HasPrefix(rel, "..") {
		writeError(w, http.StatusForbidden, "invalid image path")
		return
	}

	data, err := os.ReadFile(cleanedPath)
	if err != nil {
		writeError(w, http.StatusNotFound, "qr image file missing")
		return
	}

	contentType := qr.ImageContentType
	if contentType == "" {
		contentType = "image/png"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("ETag", fmt.Sprintf(`"%s"`, qr.ImageHash))
	w.Header().Set("Cache-Control", "private, max-age=3600")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (s *Server) savePaymentQR(w http.ResponseWriter, r *http.Request) {
	var input struct {
		AccountNumber string `json:"accountNumber"`
		AccountName   string `json:"accountName"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	input.AccountNumber = strings.TrimSpace(input.AccountNumber)
	input.AccountName = strings.TrimSpace(input.AccountName)
	if input.AccountNumber == "" || input.AccountName == "" {
		writeError(w, http.StatusBadRequest, "accountNumber and accountName are required")
		return
	}

	existing, _ := s.store.GetPaymentQR(r.Context(), "")
	qr := storage.PaymentQR{
		AccountNumber: input.AccountNumber,
		AccountName:   input.AccountName,
		Provider:      "UPLOAD",
	}
	if existing != nil {
		qr.ImagePath = existing.ImagePath
		qr.ImageHash = existing.ImageHash
		qr.ImageContentType = existing.ImageContentType
		qr.Provider = existing.Provider
		qr.Revision = existing.Revision
	}
	saved, err := s.store.SavePaymentQR(r.Context(), qr)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	audit(s.store, r, "payment_qr.save", saved.ID)
	writeJSON(w, http.StatusOK, saved)
}

func (s *Server) uploadPaymentQR(w http.ResponseWriter, r *http.Request) {
	// Limit upload size to 2MB
	r.Body = http.MaxBytesReader(w, r.Body, 2*1024*1024)
	if err := r.ParseMultipartForm(2 * 1024 * 1024); err != nil {
		writeError(w, http.StatusBadRequest, "image too large (max 2MB)")
		return
	}

	file, header, err := r.FormFile("image")
	if err != nil {
		writeError(w, http.StatusBadRequest, "image file is required")
		return
	}
	defer file.Close()

	accountNumber := strings.TrimSpace(r.FormValue("accountNumber"))
	accountName := strings.TrimSpace(r.FormValue("accountName"))

	imgBytes, err := io.ReadAll(file)
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read image file")
		return
	}

	// Sniff MIME type - reject SVG/HTML/executable
	contentType := http.DetectContentType(imgBytes)
	if !strings.HasPrefix(contentType, "image/png") &&
		!strings.HasPrefix(contentType, "image/jpeg") &&
		!strings.HasPrefix(contentType, "image/webp") {
		writeError(w, http.StatusBadRequest, "invalid image format: only PNG, JPEG and WebP allowed")
		return
	}

	// Save image file
	dataDir := filepath.Dir(s.cfg.DatabasePath)
	qrDir := filepath.Join(dataDir, "qr")
	_ = os.MkdirAll(qrDir, 0o750)

	ext := ".png"
	if strings.Contains(contentType, "jpeg") {
		ext = ".jpg"
	} else if strings.Contains(contentType, "webp") {
		ext = ".webp"
	}

	imgHash := storage.HashBytes(imgBytes)
	filePath := filepath.Join(qrDir, fmt.Sprintf("qr_%s%s", imgHash[:16], ext))
	tempPath := filepath.Join(qrDir, fmt.Sprintf(".tmp_upload_%d", time.Now().UnixNano()))
	if err := os.WriteFile(tempPath, imgBytes, 0o640); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to store image file")
		return
	}
	if err := os.Rename(tempPath, filePath); err != nil {
		_ = os.Remove(tempPath)
		writeError(w, http.StatusInternalServerError, "failed to move image file")
		return
	}

	existing, _ := s.store.GetPaymentQR(r.Context(), "")
	qr := storage.PaymentQR{
		AccountNumber:    accountNumber,
		AccountName:      accountName,
		ImagePath:        filePath,
		ImageHash:        imgHash,
		ImageContentType: contentType,
		Provider:         "UPLOAD",
	}
	if existing != nil {
		if qr.AccountNumber == "" {
			qr.AccountNumber = existing.AccountNumber
		}
		if qr.AccountName == "" {
			qr.AccountName = existing.AccountName
		}
	}
	if qr.AccountNumber == "" || qr.AccountName == "" {
		if existing == nil || existing.ImagePath != filePath {
			_ = os.Remove(filePath)
		}
		writeError(w, http.StatusBadRequest, "accountNumber and accountName are required")
		return
	}

	saved, err := s.store.SavePaymentQR(r.Context(), qr)
	if err != nil {
		if existing == nil || existing.ImagePath != filePath {
			_ = os.Remove(filePath)
		}
		writeError(w, http.StatusInternalServerError, "failed to save payment qr: "+err.Error())
		return
	}

	audit(s.store, r, "payment_qr.upload", saved.ID)
	writeJSON(w, http.StatusOK, map[string]any{
		"status":   "OK",
		"qr":       saved,
		"imageURL": "/api/v1/payment-qr/image?v=" + strconv.FormatInt(saved.Revision, 10),
	})
	_ = header
}

func (s *Server) generatePaymentQR(w http.ResponseWriter, r *http.Request) {
	var input struct {
		AccountNumber string `json:"accountNumber"`
		AccountName   string `json:"accountName"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	input.AccountNumber = strings.TrimSpace(input.AccountNumber)
	input.AccountName = strings.TrimSpace(input.AccountName)
	if input.AccountNumber == "" || input.AccountName == "" {
		writeError(w, http.StatusBadRequest, "accountNumber and accountName are required")
		return
	}

	// Generate VietQR using VietQR standard quick link (ACB BIN: 970416)
	vietQRURL := fmt.Sprintf("https://img.vietqr.io/image/970416-%s-compact.png?accountName=%s",
		url.PathEscape(input.AccountNumber), url.QueryEscape(input.AccountName))

	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, vietQRURL, nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create request")
		return
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		writeError(w, http.StatusBadGateway, "failed to generate QR from VietQR provider")
		return
	}
	defer resp.Body.Close()

	imgBytes, err := io.ReadAll(http.MaxBytesReader(w, resp.Body, 2*1024*1024))
	if err != nil {
		writeError(w, http.StatusBadGateway, "failed to read generated QR data")
		return
	}

	// Validate content type
	contentType := http.DetectContentType(imgBytes)
	if !strings.HasPrefix(contentType, "image/png") &&
		!strings.HasPrefix(contentType, "image/jpeg") &&
		!strings.HasPrefix(contentType, "image/webp") {
		writeError(w, http.StatusBadGateway, "invalid image received from VietQR provider")
		return
	}

	dataDir := filepath.Dir(s.cfg.DatabasePath)
	qrDir := filepath.Join(dataDir, "qr")
	_ = os.MkdirAll(qrDir, 0o750)

	imgHash := storage.HashBytes(imgBytes)
	filePath := filepath.Join(qrDir, fmt.Sprintf("qr_gen_%s.png", imgHash[:16]))
	tempPath := filepath.Join(qrDir, fmt.Sprintf(".tmp_gen_%d", time.Now().UnixNano()))
	if err := os.WriteFile(tempPath, imgBytes, 0o640); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to save generated QR image")
		return
	}
	if err := os.Rename(tempPath, filePath); err != nil {
		_ = os.Remove(tempPath)
		writeError(w, http.StatusInternalServerError, "failed to move generated QR image")
		return
	}

	qr := storage.PaymentQR{
		AccountNumber:    input.AccountNumber,
		AccountName:      input.AccountName,
		ImagePath:        filePath,
		ImageHash:        imgHash,
		ImageContentType: contentType,
		Provider:         "VIETQR",
	}
	saved, err := s.store.SavePaymentQR(r.Context(), qr)
	if err != nil {
		_ = os.Remove(filePath)
		writeError(w, http.StatusInternalServerError, "failed to save qr: "+err.Error())
		return
	}

	audit(s.store, r, "payment_qr.generate", saved.ID)
	writeJSON(w, http.StatusOK, map[string]any{
		"status":   "OK",
		"qr":       saved,
		"imageURL": "/api/v1/payment-qr/image?v=" + strconv.FormatInt(saved.Revision, 10),
	})
}

func (s *Server) deletePaymentQR(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeletePaymentQR(r.Context(), ""); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to delete payment qr")
		return
	}
	audit(s.store, r, "payment_qr.delete", "singleton")
	writeJSON(w, http.StatusOK, map[string]string{"status": "DELETED"})
}
func (s *Server) deliveries(w http.ResponseWriter, r *http.Request) {
	limit, cursor, err := pageParams(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	page, err := s.store.ListDeliveriesPage(r.Context(), limit, cursor)
	if err != nil {
		status := http.StatusInternalServerError
		if cursor != "" {
			status = http.StatusBadRequest
		}
		writeError(w, status, "pagination request failed")
		return
	}
	writeJSON(w, http.StatusOK, page)
}
func (s *Server) pollRuns(w http.ResponseWriter, r *http.Request) {
	limit, cursor, err := pageParams(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	page, err := s.store.ListPollRunsPage(r.Context(), limit, cursor)
	if err != nil {
		status := http.StatusInternalServerError
		if cursor != "" {
			status = http.StatusBadRequest
		}
		writeError(w, status, "pagination request failed")
		return
	}
	writeJSON(w, http.StatusOK, page)
}
func (s *Server) auditLogs(w http.ResponseWriter, r *http.Request) {
	limit, cursor, err := pageParams(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	page, err := s.store.ListAuditLogsPage(r.Context(), limit, cursor)
	if err != nil {
		status := http.StatusInternalServerError
		if cursor != "" {
			status = http.StatusBadRequest
		}
		writeError(w, status, "pagination request failed")
		return
	}
	writeJSON(w, http.StatusOK, page)
}
func (s *Server) endpoints(w http.ResponseWriter, r *http.Request) {
	items, err := s.store.Endpoints(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "storage_error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}
func (s *Server) createEndpoint(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name string `json:"name"`
		URL  string `json:"url"`
	}
	if !decode(w, r, &in) {
		return
	}
	if _, err := security.ValidateWebhookURL(in.URL); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	e, err := s.store.CreateEndpointWithSecret(r.Context(), in.Name, in.URL)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	audit(s.store, r, "webhook.create", e.ID)
	s.publishStateEvent("webhook.changed", e.ID, map[string]any{"id": e.ID, "status": e.Status})
	writeJSON(w, http.StatusCreated, e)
}
func (s *Server) endpointAction(w http.ResponseWriter, r *http.Request) {
	status := "ACTIVE"
	if chi.URLParam(r, "action") == "disable" {
		status = "DISABLED"
	}
	err := s.store.SetEndpointStatus(r.Context(), chi.URLParam(r, "id"), status)
	if errors.Is(err, storage.ErrNotFound) {
		writeError(w, http.StatusNotFound, "endpoint_not_found")
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	audit(s.store, r, "webhook."+strings.ToLower(status), chi.URLParam(r, "id"))
	s.publishStateEvent("webhook.changed", chi.URLParam(r, "id"), map[string]any{"id": chi.URLParam(r, "id"), "status": status})
	writeJSON(w, http.StatusOK, map[string]string{"status": status})
}
func audit(store *storage.Store, r *http.Request, action, target string) {
	identity, _ := auth.FromContext(r.Context())
	_ = store.Audit(r.Context(), identity.Subject, string(identity.Role), action, target, requestIDFromContext(r.Context()))
}
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		writeError(w, http.StatusUnsupportedMediaType, "content_type_must_be_json")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json")
		return false
	}
	return true
}
func requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, 12)
		_, _ = rand.Read(b)
		id := hex.EncodeToString(b)
		w.Header().Set("X-Request-Id", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey{}, id)))
	})
}

type requestIDKey struct{}

func requestIDFromContext(ctx context.Context) string {
	v, _ := ctx.Value(requestIDKey{}).(string)
	return v
}

const defaultContentSecurityPolicy = "default-src 'self'; base-uri 'none'; frame-ancestors 'self'; form-action 'self'; object-src 'none'; connect-src 'self'"

func (s *Server) platformHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.Slot != "" {
			w.Header().Set("X-Platform-Slot", s.cfg.Slot)
		}
		if s.cfg.ReleaseCommit != "" {
			w.Header().Set("X-Release-Commit", s.cfg.ReleaseCommit)
		}
		next.ServeHTTP(w, r)
	})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "SAMEORIGIN")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=()")
		w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
		w.Header().Set("Content-Security-Policy", defaultContentSecurityPolicy)
		if r.TLS != nil {
			w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		next.ServeHTTP(w, r)
	})
}
func recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recover() != nil {
				writeError(w, http.StatusInternalServerError, "internal_error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
func writeError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]string{"error": code, "code": code})
}

func writeStandardError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	reqID := ""
	if r != nil {
		reqID = r.Header.Get("X-Request-Id")
		if reqID == "" {
			reqID = requestIDFromContext(r.Context())
		}
	}
	resp := map[string]any{
		"error": message,
		"code":  code,
	}
	if reqID != "" {
		resp["requestId"] = reqID
	}
	writeJSON(w, status, resp)
}
func spa(files fs.FS) http.Handler {
	static := http.FileServer(http.FS(files))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.NotFound(w, r)
			return
		}
		requested := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if requested != "." && requested != "" {
			if f, err := files.Open(requested); err == nil {
				_ = f.Close()
				if strings.HasPrefix(requested, "assets/") {
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				}
				static.ServeHTTP(w, r)
				return
			}
		}
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		w.Header().Set("Pragma", "no-cache")
		w.Header().Set("Expires", "0")
		r.URL.Path = "/"
		static.ServeHTTP(w, r)
	})
}
