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
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/thedemontuan/acb-transaction-webhook/internal/auth"
	"github.com/thedemontuan/acb-transaction-webhook/internal/bark"
	"github.com/thedemontuan/acb-transaction-webhook/internal/config"
	"github.com/thedemontuan/acb-transaction-webhook/internal/eventhub"
	"github.com/thedemontuan/acb-transaction-webhook/internal/notification"
	"github.com/thedemontuan/acb-transaction-webhook/internal/payments"
	"github.com/thedemontuan/acb-transaction-webhook/internal/security"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
	"github.com/thedemontuan/acb-transaction-webhook/internal/telemetry"
	"github.com/thedemontuan/acb-transaction-webhook/internal/ttsclient"
	"github.com/thedemontuan/acb-transaction-webhook/internal/workerrpc"
)

type WorkerProber interface {
	Ready(ctx context.Context) error
}

type NotificationChannelTester interface {
	TestNotificationChannel(ctx context.Context, channelID string) (workerrpc.TestNotificationResponse, error)
}

type NotificationProviderReader interface {
	NotificationProviderMetadata(ctx context.Context) (workerrpc.NotificationProvidersResponse, error)
}

type ipRateLimiter struct {
	mu      sync.Mutex
	history map[string][]time.Time
}

func newIPRateLimiter() *ipRateLimiter {
	return &ipRateLimiter{
		history: make(map[string][]time.Time),
	}
}

func (l *ipRateLimiter) allow(ip string, limit int, window time.Duration, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.history == nil {
		l.history = make(map[string][]time.Time)
	}
	cutoff := now.Add(-window)
	// Prune every idle client, not just the current IP, to bound the map across
	// long-running public traffic. Each limiter instance uses one fixed window.
	for key, entries := range l.history {
		if len(entries) == 0 || !entries[len(entries)-1].After(cutoff) {
			delete(l.history, key)
		}
	}
	timestamps := l.history[ip]
	valid := timestamps[:0]
	for _, t := range timestamps {
		if t.After(cutoff) {
			valid = append(valid, t)
		}
	}
	if len(valid) >= limit {
		l.history[ip] = valid
		return false
	}
	l.history[ip] = append(valid, now)
	return true
}

type WakeDispatcherFunc func(ctx context.Context) error

type Server struct {
	payments             *payments.Service
	workerProber         WorkerProber
	channelTester        NotificationChannelTester
	providerReader       NotificationProviderReader
	cfg                  config.Config
	store                *storage.Store
	auth                 *auth.Middleware
	eventHub             *eventhub.Hub
	realtimeSubmit       func(eventhub.Event) error
	ttsClient            *ttsclient.Client
	barkSender           *bark.Sender
	notifRegistry        *notification.Registry
	wakeFn               WakeDispatcherFunc
	instanceNonce        string
	testCooldownMu       sync.Mutex
	lastTestPerCh        map[string]time.Time
	started              time.Time
	handler              http.Handler
	paymentCreateLimiter *ipRateLimiter
	paymentGetLimiter    *ipRateLimiter
	paymentIntentLocks   [64]sync.Mutex
}

func New(cfg config.Config, store *storage.Store) *Server {
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
		cfg:                  cfg,
		store:                store,
		auth:                 auth.New(cfg, verifier),
		eventHub:             eventhub.New(),
		ttsClient:            ttsClientInstance,
		instanceNonce:        instanceNonce,
		started:              time.Now().UTC(),
		paymentCreateLimiter: newIPRateLimiter(),
		paymentGetLimiter:    newIPRateLimiter(),
	}
	r := chi.NewRouter()
	r.Use(requestID, s.platformHeaders, securityHeaders, paymentResponseHeaders, recoverer)
	r.Get("/healthz", s.health)
	r.Get("/health", s.health)
	r.Get("/readyz", s.ready)
	r.Get("/ready", s.ready)
	r.Get("/internal/deployz", s.deployReady)
	r.Post("/api/integrations/payos/webhook", s.payOSWebhook)
	r.Route("/api/public/v1", func(api chi.Router) {
		api.Get("/payment-config", s.publicPaymentConfig)
		api.Post("/payments", s.createPublicPayment)
		api.Get("/payments/{id}", s.publicPayment)
		api.Get("/transactions", s.publicTransactions)
		api.Get("/transactions/{id}", s.publicTransactionDetail)
		api.Get("/events", s.publicEventsStream)
		api.Get("/events/stream", s.publicEventsStream)
		api.Post("/voice/transactions/{id}", s.synthesizePublicTransactionAudio)
		api.Get("/voice/transactions/{id}/stream", s.synthesizePublicTransactionAudio)
		api.Post("/voice/transactions/{id}/stream", s.synthesizePublicTransactionAudio)
		api.Post("/voice/test", s.publicTestVoiceAudio)
		api.Get("/voice/test/stream", s.publicTestVoiceAudio)
		api.Post("/voice/test/stream", s.publicTestVoiceAudio)
	})
	r.Route("/api/v1", func(api chi.Router) {
		api.Use(s.auth.Require(auth.Owner, auth.Operator, auth.Viewer))
		api.Get("/payments", s.paymentOrders)
		api.Get("/payments/{id}", s.payment)
		api.With(s.auth.Require(auth.Owner, auth.Operator), s.requirePaymentMutationAllowed).Post("/payments", s.createAdminPayment)
		api.With(s.auth.Require(auth.Owner, auth.Operator), s.requirePaymentMutationAllowed).Post("/payments/{id}/cancel", s.cancelPayment)
		api.With(s.auth.Require(auth.Owner, auth.Operator)).Get("/payment-reviews", s.paymentReviews)
		api.With(s.auth.Require(auth.Owner), s.requirePaymentMutationAllowed).Post("/payment-provider/confirm-webhook", s.confirmPaymentWebhook)
		api.Get("/status", s.status)
		api.Get("/csrf", auth.CSRF)
		api.Get("/telemetry", s.telemetry)
		api.Get("/ops/alerts", s.operationalAlerts)
		api.Get("/webhooks", s.endpoints)
		api.Get("/transactions", s.transactions)
		api.Get("/transactions/{id}", s.transactionDetail)
		api.Get("/deliveries", s.deliveries)
		api.Get("/audit", s.auditLogs)
		api.Get("/events", s.eventsStream)
		api.Get("/events/stream", s.eventsStream)
		api.Get("/realtime/status", s.realtimeStatus)

		api.Get("/voice/settings", s.getVoiceSettings)
		api.With(s.auth.Require(auth.Owner, auth.Operator), s.requireMutationAllowed).Put("/voice/settings", s.updateVoiceSettings)
		api.Get("/voice/status", s.voiceStatus)
		api.Post("/voice/test", s.testVoiceAudio)
		api.Get("/voice/test/stream", s.testVoiceAudio)
		api.Post("/voice/test/stream", s.testVoiceAudio)
		api.Post("/voice/transactions/{id}", s.synthesizeTransactionAudio)
		api.Get("/voice/transactions/{id}/stream", s.synthesizeTransactionAudio)
		api.Post("/voice/transactions/{id}/stream", s.synthesizeTransactionAudio)
		api.Post("/voice/transactions/{id}/replay", s.replayTransactionAudio)
		api.Get("/voice/transactions/{id}/replay/stream", s.replayTransactionAudio)
		api.Post("/voice/transactions/{id}/replay/stream", s.replayTransactionAudio)
		api.Post("/voice/transactions/summary", s.synthesizeSummaryAudio)
		api.Get("/voice/transactions/summary/stream", s.synthesizeSummaryAudio)
		api.Post("/voice/transactions/summary/stream", s.synthesizeSummaryAudio)

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
	s.handler = r
	return s
}
func (s *Server) WithEventHub(hub *eventhub.Hub) *Server {
	s.eventHub = hub
	return s
}

// WithRealtimeSubmit routes journal-backed events through the gateway ordering
// coordinator before they are published to browser subscribers.
func (s *Server) WithRealtimeSubmit(submit func(eventhub.Event) error) *Server {
	s.realtimeSubmit = submit
	return s
}

func (s *Server) EventHub() *eventhub.Hub {
	return s.eventHub
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

func (s *Server) WithProviderReader(reader NotificationProviderReader) *Server {
	s.providerReader = reader
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

func (s *Server) WithTTSClient(client *ttsclient.Client) *Server {
	s.ttsClient = client
	return s
}

func (s *Server) Handler() http.Handler { return s.handler }

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	roleStr := string(s.cfg.RuntimeRole)
	// 1. Role check if requested
	expectedRole := r.URL.Query().Get("role")
	if expectedRole == "" {
		expectedRole = r.Header.Get("X-Expected-Role")
	}
	if expectedRole != "" && roleStr != "" {
		if !strings.EqualFold(roleStr, expectedRole) && !strings.EqualFold(roleStr, "all-in-one") {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{
				"status": "error",
				"error":  fmt.Sprintf("role mismatch: expected %s, got %s", expectedRole, roleStr),
				"role":   roleStr,
			})
			return
		}
	}

	// 2. Release check if requested
	expectedRel := r.URL.Query().Get("release")
	if expectedRel == "" {
		expectedRel = r.Header.Get("X-Expected-Release")
	}
	if expectedRel != "" && s.cfg.ReleaseCommit != "" && s.cfg.ReleaseCommit != expectedRel {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"status":  "error",
			"error":   fmt.Sprintf("release mismatch: expected %s, got %s", expectedRel, s.cfg.ReleaseCommit),
			"release": s.cfg.ReleaseCommit,
		})
		return
	}

	// 3. Slot check if requested
	expectedSlot := r.URL.Query().Get("slot")
	if expectedSlot == "" {
		expectedSlot = r.Header.Get("X-Expected-Slot")
	}
	if expectedSlot != "" && s.cfg.Slot != "" && !strings.EqualFold(s.cfg.Slot, expectedSlot) {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"status": "error",
			"error":  fmt.Sprintf("slot mismatch: expected %s, got %s", expectedSlot, s.cfg.Slot),
			"slot":   s.cfg.Slot,
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "ok",
		"role":    roleStr,
		"slot":    s.cfg.Slot,
		"release": s.cfg.ReleaseCommit,
	})
}

func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	roleStr := string(s.cfg.RuntimeRole)
	// 1. Role check if requested
	expectedRole := r.URL.Query().Get("role")
	if expectedRole == "" {
		expectedRole = r.Header.Get("X-Expected-Role")
	}
	if expectedRole != "" && roleStr != "" {
		if !strings.EqualFold(roleStr, expectedRole) && !strings.EqualFold(roleStr, "all-in-one") {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{
				"status": "not_ready",
				"error":  fmt.Sprintf("role mismatch: expected %s, got %s", expectedRole, roleStr),
				"role":   roleStr,
			})
			return
		}
	}

	// 2. Release check if requested
	expectedRel := r.URL.Query().Get("release")
	if expectedRel == "" {
		expectedRel = r.Header.Get("X-Expected-Release")
	}
	if expectedRel != "" && s.cfg.ReleaseCommit != "" && s.cfg.ReleaseCommit != expectedRel {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"status":  "not_ready",
			"error":   fmt.Sprintf("release mismatch: expected %s, got %s", expectedRel, s.cfg.ReleaseCommit),
			"release": s.cfg.ReleaseCommit,
		})
		return
	}

	// 3. Storage health check
	if err := s.store.Health(r.Context()); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"status": "not_ready",
			"error":  "storage unhealthy: " + err.Error(),
		})
		return
	}

	// 4. Schema version check
	schemaReport, err := s.store.SchemaVersion(r.Context())
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"status": "not_ready",
			"error":  "schema check failed: " + err.Error(),
		})
		return
	}

	// 5. Schema check if requested
	expectedSchema := r.URL.Query().Get("schema")
	if expectedSchema == "" {
		expectedSchema = r.Header.Get("X-Expected-Schema")
	}
	if expectedSchema != "" && fmt.Sprintf("%d", schemaReport.Version) != expectedSchema {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"status": "not_ready",
			"error":  fmt.Sprintf("schema mismatch: expected %s, got %d", expectedSchema, schemaReport.Version),
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status":        "ready",
		"role":          roleStr,
		"slot":          s.cfg.Slot,
		"release":       s.cfg.ReleaseCommit,
		"schemaVersion": schemaReport.Version,
	})
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

	roleStr := string(s.cfg.RuntimeRole)
	ctx := r.Context()
	resp := map[string]any{
		"role":             roleStr,
		"release":          s.cfg.ReleaseCommit,
		"slot":             s.cfg.Slot,
		"nonce":            s.instanceNonce,
		"workerRpcVersion": "v2",
	}

	status := "ready"

	// Role check if requested
	expectedRole := r.URL.Query().Get("role")
	if expectedRole == "" {
		expectedRole = r.Header.Get("X-Expected-Role")
	}
	if expectedRole != "" && roleStr != "" {
		if !strings.EqualFold(roleStr, expectedRole) && !strings.EqualFold(roleStr, "all-in-one") {
			resp["roleError"] = fmt.Sprintf("expected %s, got %s", expectedRole, roleStr)
			status = "not_ready"
		}
	}

	// Release check if requested
	expectedRel := r.URL.Query().Get("release")
	if expectedRel == "" {
		expectedRel = r.Header.Get("X-Expected-Release")
	}
	if expectedRel != "" && s.cfg.ReleaseCommit != "" && s.cfg.ReleaseCommit != expectedRel {
		resp["releaseError"] = fmt.Sprintf("expected %s, got %s", expectedRel, s.cfg.ReleaseCommit)
		status = "not_ready"
	}

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
		expectedSchema := r.URL.Query().Get("schema")
		if expectedSchema == "" {
			expectedSchema = r.Header.Get("X-Expected-Schema")
		}
		if expectedSchema != "" && fmt.Sprintf("%d", schemaReport.Version) != expectedSchema {
			resp["schemaError"] = fmt.Sprintf("expected %s, got %d", expectedSchema, schemaReport.Version)
			status = "not_ready"
		}
	}

	// 3. Worker check (if configured)
	if s.cfg.WorkerRPCURL != "" {
		if s.workerProber != nil {
			if err := s.workerProber.Ready(ctx); err != nil {
				if strings.Contains(strings.ToLower(err.Error()), "stale") {
					resp["worker"] = "stale: " + err.Error()
				} else {
					resp["worker"] = "unreachable: " + err.Error()
				}
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

	// Deployment readiness covers only critical request dependencies. Auxiliary
	// realtime and TTS outages are reported below but do not evict a healthy
	// gateway from the blue/green route.
	criticalStatus := status

	if s.cfg.WorkerRealtimeEnabled {
		realtime := telemetry.Default.FullSnapshot().Realtime
		resp["realtime"] = realtime.StreamState
		if realtime.StreamState != "connected" && status == "ready" {
			status = "degraded"
		}
	} else {
		resp["realtime"] = "disabled"
	}

	// TTS gateway check (if configured)
	if s.cfg.TTSGatewayURL != "" {
		client := &http.Client{Timeout: 1500 * time.Millisecond}
		res, err := client.Get(strings.TrimRight(s.cfg.TTSGatewayURL, "/") + "/health")
		if err != nil || res.StatusCode != http.StatusOK {
			resp["tts"] = "unreachable"
			if status == "ready" {
				status = "degraded"
			}
			if res != nil {
				_ = res.Body.Close()
			}
		} else {
			resp["tts"] = "ready"
			_ = res.Body.Close()
		}
	} else {
		resp["tts"] = "disabled"
	}

	// 6. Bark check (if configured)
	if s.barkSender != nil || s.cfg.BarkServerURL != "" {
		resp["bark"] = "ready"
	} else {
		resp["bark"] = "disabled"
	}

	// 7. Mutation gate state
	gate, gateErr := s.store.GetDeploymentGate(ctx)
	if gateErr == nil && gate != nil {
		resp["mutationGate"] = gate.GateState
	} else {
		resp["mutationGate"] = "OPEN"
	}

	resp["status"] = status
	code := http.StatusOK
	if criticalStatus == "not_ready" {
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
	w.Header().Set("Cache-Control", "no-store")
	service := s.payments
	if service == nil {
		service = payments.NewService(s.cfg, s.store, nil, nil)
	}
	paymentStatus, err := service.Status(r.Context())
	if err != nil {
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
		"payments":      paymentStatus,
		"storage":       map[string]string{"status": "READY"},
		"webhooks":      summary.ByProvider[notification.ProviderWebhook],
		"notifications": summary,
		"role":          s.cfg.RuntimeRole,
		"slot":          s.cfg.Slot,
		"release":       s.cfg.ReleaseCommit,
	})
}

func (s *Server) realtimeStatus(w http.ResponseWriter, r *http.Request) {
	if s.eventHub != nil {
		telemetry.Default.SetConnectedClients(int64(s.eventHub.SubscriberCount()))
	}
	writeJSON(w, http.StatusOK, telemetry.Default.Report())
}

func (s *Server) telemetry(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if s.store != nil {
		if summary, err := s.store.NotificationSummary(ctx); err == nil {
			byProv := make(map[string]telemetry.ProviderSnapshot, len(summary.ByProvider))
			for prov, ds := range summary.ByProvider {
				byProv[prov] = telemetry.ProviderSnapshot{
					Pending:    ds.Pending,
					DeadLetter: ds.DeadLetter,
				}
			}
			isStuck := summary.Total.DeadLetter > 10 || summary.Total.Pending > 100
			telemetry.Default.SetNotificationBacklog(summary.Total.Pending, summary.Total.DeadLetter, isStuck, byProv)
		}
		if gate, err := s.store.GetDeploymentGate(ctx); err == nil && gate != nil {
			var expIn time.Duration
			if gate.LeaseExpiresAt != "" {
				if t, err := time.Parse(time.RFC3339Nano, gate.LeaseExpiresAt); err == nil {
					expIn = time.Until(t)
				} else if t, err := time.Parse(time.RFC3339, gate.LeaseExpiresAt); err == nil {
					expIn = time.Until(t)
				}
			}
			telemetry.Default.SetMutationGate(gate.GateState, gate.Owner, gate.Reason, expIn)
		}
	}
	if s.eventHub != nil {
		telemetry.Default.SetConnectedClients(int64(s.eventHub.SubscriberCount()))
	}
	telemetry.Default.SetDeployment(s.cfg.Slot, s.cfg.ReleaseCommit, string(s.cfg.RuntimeRole), "compatible", "SUCCESS", time.Now())

	snap := telemetry.Default.FullSnapshot()
	alerts := telemetry.EvaluateAlerts(snap)

	writeJSON(w, http.StatusOK, map[string]any{
		"telemetry": snap,
		"alerts":    alerts,
	})
}

func (s *Server) operationalAlerts(w http.ResponseWriter, r *http.Request) {
	snap := telemetry.Default.FullSnapshot()
	alerts := telemetry.EvaluateAlerts(snap)
	writeJSON(w, http.StatusOK, map[string]any{
		"alerts": alerts,
	})
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
		if s.cfg.RuntimeRole != "" {
			w.Header().Set("X-Runtime-Role", string(s.cfg.RuntimeRole))
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
