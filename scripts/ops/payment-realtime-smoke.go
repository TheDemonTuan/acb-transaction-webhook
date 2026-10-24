// Command payment-realtime-smoke exercises real payment ingress, SQLite financial
// writes, worker RPC, journal relay, gateway coordinators, SSE and signed outbox
// delivery. It never opens an existing database or accepts a provider URL/key.
//
// Run from the repository root:
// go run ./scripts/ops/payment-realtime-smoke.go --scenario smoke --start-delay 30s --duration 90s --output payment-smoke.json
// For UI listening, start Vite separately on loopback:5173 and enable the speaker
// before start-delay expires. TTS_BASE_URL must also be a literal loopback URL.
// A two-hour soak needs a launcher whose lifetime exceeds two hours. Prefer a
// foreground terminal or a named service without a command deadline; a canceled
// launcher is an interrupted run, never evidence of provider/key expiration.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	payos "github.com/payOSHQ/payos-lib-golang/v2"
	"github.com/thedemontuan/acb-transaction-webhook/internal/config"
	"github.com/thedemontuan/acb-transaction-webhook/internal/eventhub"
	"github.com/thedemontuan/acb-transaction-webhook/internal/httpapi"
	"github.com/thedemontuan/acb-transaction-webhook/internal/notification"
	"github.com/thedemontuan/acb-transaction-webhook/internal/payments"
	"github.com/thedemontuan/acb-transaction-webhook/internal/realtimestream"
	"github.com/thedemontuan/acb-transaction-webhook/internal/security"
	"github.com/thedemontuan/acb-transaction-webhook/internal/sepay"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
	"github.com/thedemontuan/acb-transaction-webhook/internal/ttsclient"
	"github.com/thedemontuan/acb-transaction-webhook/internal/voicecopy"
	"github.com/thedemontuan/acb-transaction-webhook/internal/webhook"
	"github.com/thedemontuan/acb-transaction-webhook/internal/workerrpc"
)

const checksum = "disposable-harness-payos-checksum"
const payOSPath = "/api/integrations/payos/webhook"

type options struct {
	scenario, listenA, listenB, output string
	rate                               float64
	duration, delay                    time.Duration
	burst                              int
}
type distribution struct {
	Count           int64   `json:"count"`
	RetainedSamples int     `json:"retainedSamples"`
	P50             float64 `json:"p50Ms"`
	P95             float64 `json:"p95Ms"`
	P99             float64 `json:"p99Ms"`
	Max             float64 `json:"maxMs"`
}

func summarize(samples []time.Duration, count int64) distribution {
	values := append([]time.Duration(nil), samples...)
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	if len(values) == 0 {
		return distribution{Count: count}
	}
	at := func(p float64) float64 {
		return float64(values[int(math.Ceil(p*float64(len(values))))-1]) / float64(time.Millisecond)
	}
	return distribution{count, len(values), at(.50), at(.95), at(.99), at(1)}
}

type resources struct {
	At          string `json:"at"`
	Goroutines  int    `json:"goroutines"`
	HeapAlloc   uint64 `json:"heapAllocBytes"`
	HeapInuse   uint64 `json:"heapInuseBytes"`
	HeapObjects uint64 `json:"heapObjects"`
	GCCycles    uint32 `json:"gcCycles"`
}

func snapshot() resources {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return resources{time.Now().UTC().Format(time.RFC3339Nano), runtime.NumGoroutine(), m.HeapAlloc, m.HeapInuse, m.HeapObjects, m.NumGC}
}

type phaseMetrics struct {
	ACK         distribution `json:"ack"`
	CommitToSSE distribution `json:"commitObservedToSse"`
}
type report struct {
	FixtureMode       bool   `json:"fixtureMode"`
	Scenario          string `json:"scenario"`
	StartedAt         string `json:"startedAt"`
	OS, Arch, Go      string
	DurationSeconds   float64                     `json:"durationSeconds"`
	Rate              float64                     `json:"rate"`
	Burst             int                         `json:"burst"`
	Load              map[string]any              `json:"load,omitempty"`
	Status            string                      `json:"status"`
	Error             string                      `json:"error,omitempty"`
	Audio             map[string]any              `json:"audio"`
	Counts            map[string]int64            `json:"counts"`
	Expected          map[string]int64            `json:"expected"`
	Checkpoints       map[string]map[string]int64 `json:"checkpoints"`
	Phases            map[string]phaseMetrics     `json:"phases"`
	Faults            map[string]string           `json:"faults"`
	Warmup            resources                   `json:"warmup"`
	Final             resources                   `json:"final"`
	Lifecycle         []resources                 `json:"lifecycleSamples"`
	Missing           int                         `json:"missingEvents"`
	Duplicate         int                         `json:"duplicateEvents"`
	Unexpected        int                         `json:"unexpectedEvents"`
	Notes             []string                    `json:"notes"`
	StorageFailures   []map[string]any            `json:"storageFailures,omitempty"`
	FailureGoroutines string                      `json:"failureGoroutines,omitempty"`
	WarmupPools       map[string]any              `json:"warmupPools,omitempty"`
	FinalPools        map[string]any              `json:"finalPools,omitempty"`
}

// All timestamps below retain Go's monotonic clock. Commit observation is the
// first harness callback after Store returns its successful transaction commit;
// journal created_at and clocks received over HTTP are never latency origins.
type measurements struct {
	mu                                                                              sync.Mutex
	commits                                                                         map[string]time.Time
	arrivals                                                                        map[string]time.Time
	observedCommits, observedEvents, delivered, duplicateEvents, unavailableLatency int64
	lastSeq                                                                         int64
	phases                                                                          map[string]string
	lastEventID                                                                     string
	ack                                                                             map[string][]time.Duration
	latency                                                                         map[string][]time.Duration
	ackCount, latencyCount                                                          map[string]int64
	activePhase                                                                     string
	expectedCredits, expectedTotal, expectedPayOS, expectedTelegram, expectedNative int64
}

func newMeasurements() *measurements {
	return &measurements{commits: map[string]time.Time{}, arrivals: map[string]time.Time{}, phases: map[string]string{}, ack: map[string][]time.Duration{}, latency: map[string][]time.Duration{}, ackCount: map[string]int64{}, latencyCount: map[string]int64{}, activePhase: "warm"}
}
func (m *measurements) phase(p string) { m.mu.Lock(); m.activePhase = p; m.mu.Unlock() }

// Keep only unmatched in-flight observations and the latest 1,000 latency
// samples per phase. Long soaks must not measure unbounded harness bookkeeping.
func retainSample(samples map[string][]time.Duration, counts map[string]int64, phase string, value time.Duration) {
	counts[phase]++
	if len(samples[phase]) < 1000 {
		samples[phase] = append(samples[phase], value)
		return
	}
	samples[phase][(counts[phase]-1)%1000] = value
}
func (m *measurements) committed(e storage.EventNotification) {
	at := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.observedCommits++
	if arrival, ok := m.arrivals[e.TransactionID]; ok {
		// A recovery tick can read the committed journal before the notifier
		// runs. Its arrival is real but lacks a monotonic commit observation;
		// report the missing latency sample, never a negative/fabricated one.
		if arrival.Before(at) {
			m.unavailableLatency++
		} else {
			retainSample(m.latency, m.latencyCount, m.activePhase, arrival.Sub(at))
		}
		delete(m.arrivals, e.TransactionID)
		return
	}
	m.commits[e.TransactionID] = at
	m.phases[e.TransactionID] = m.activePhase
}
func (m *measurements) received(id, eventID string) error {
	at := time.Now()
	parts := strings.Split(eventID, ":")
	if len(parts) != 2 || parts[0] != "ep1" {
		return errors.New("invalid SSE cursor")
	}
	seq, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || seq <= 0 {
		return errors.New("invalid SSE sequence")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if seq <= m.lastSeq {
		m.duplicateEvents++
		return nil
	}
	m.lastSeq = seq
	m.lastEventID = eventID
	m.observedEvents++
	if commit, ok := m.commits[id]; ok {
		retainSample(m.latency, m.latencyCount, m.phases[id], at.Sub(commit))
		delete(m.commits, id)
		delete(m.phases, id)
	} else {
		m.arrivals[id] = at
	}
	return nil
}
func (m *measurements) expect(provider string, amount int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.expectedCredits++
	m.expectedTotal += amount
	switch provider {
	case "payos":
		m.expectedPayOS++
	case "telegram":
		m.expectedTelegram++
	case "native":
		m.expectedNative++
	}
}
func loopbackAddress(address string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("listen address must be literal loopback host:port: %w", err)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return errors.New("only literal loopback listeners are permitted")
	}
	return nil
}
func loopbackURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return errors.New("TTS_BASE_URL must be an http literal loopback origin")
	}
	ip := net.ParseIP(u.Hostname())
	if ip == nil || !ip.IsLoopback() {
		return errors.New("TTS_BASE_URL must be literal loopback; remote synthesis is forbidden")
	}
	return nil
}
func randomToken() (string, error) {
	b := make([]byte, 32)
	_, err := rand.Read(b)
	return hex.EncodeToString(b), err
}
func encode(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// This is the exact sorted scalar signing procedure used by the existing
// tests/integration/payos_fixture_test.go; the real payOS SDK verifies it.
func signature(data map[string]any) string {
	var normalized map[string]any
	if err := json.Unmarshal(encode(data), &normalized); err != nil {
		panic(err)
	}
	keys := make([]string, 0, len(normalized))
	for key := range normalized {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		value := ""
		switch v := normalized[key].(type) {
		case nil:
		case string:
			value = v
		case float64:
			value = strconv.FormatFloat(v, 'f', -1, 64)
		default:
			value = string(encode(v))
		}
		parts = append(parts, key+"="+value)
	}
	mac := hmac.New(sha256.New, []byte(checksum))
	_, _ = mac.Write([]byte(strings.Join(parts, "&")))
	return hex.EncodeToString(mac.Sum(nil))
}
func fixtureProvider() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v2/payment-requests" {
			http.Error(w, "fixture route not found", 404)
			return
		}
		var in payos.CreatePaymentLinkRequest
		if json.NewDecoder(io.LimitReader(r.Body, 65536)).Decode(&in) != nil {
			http.Error(w, "invalid fixture order", 400)
			return
		}
		link := fmt.Sprintf("fixture-link-%d", in.OrderCode)
		data := map[string]any{"orderCode": in.OrderCode, "amount": in.Amount, "description": in.Description, "currency": "VND", "paymentLinkId": link, "status": "PENDING", "qrCode": "fixture-qr-" + link, "accountNumber": fmt.Sprintf("VA%d", in.OrderCode), "accountName": "FIXTURE", "bin": "970452", "checkoutUrl": "https://pay.payos.vn/web/" + link}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"code": "00", "data": data, "signature": signature(data)})
	}))
}
func paymentBody(o storage.PaymentOrder) []byte {
	vn := time.FixedZone("Asia/Ho_Chi_Minh", 7*3600)
	data := map[string]any{"orderCode": o.OrderCode, "amount": o.AmountVnd, "description": o.Description, "accountNumber": "FIXTUREMAIN", "virtualAccountNumber": o.AccountNumber, "reference": fmt.Sprintf("FIXTURE-%d", o.OrderCode), "transactionDateTime": time.Now().In(vn).Format("2006-01-02 15:04:05"), "currency": "VND", "paymentLinkId": o.PaymentLinkID, "code": "00"}
	return encode(map[string]any{"code": "00", "success": true, "data": data, "signature": signature(data)})
}

// streamGate faults the real worker stream, not the public SSE handler. All
// existing connections are canceled; reconnects receive 503 until restored.
type streamGate struct {
	handler     http.Handler
	mu          sync.Mutex
	disabled    bool
	next        int
	connections map[int]context.CancelFunc
}

func (g *streamGate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	if g.disabled {
		g.mu.Unlock()
		http.Error(w, "fixture worker offline", 503)
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	g.next++
	id := g.next
	g.connections[id] = cancel
	handler := g.handler
	g.mu.Unlock()
	defer func() { cancel(); g.mu.Lock(); delete(g.connections, id); g.mu.Unlock() }()
	handler.ServeHTTP(w, r.WithContext(ctx))
}
func (g *streamGate) disable(disabled bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.disabled = disabled
	if disabled {
		for _, cancel := range g.connections {
			cancel()
		}
	}
}

type worker struct {
	mu         sync.RWMutex
	offline    atomic.Bool
	publisher  *realtimestream.JournalPublisher
	dispatcher *notification.Dispatcher
	payments   *payments.Service
	blocked    atomic.Bool
	entered    chan struct{}
	release    chan struct{}
}

func (w *worker) WakeDispatcher(ctx context.Context) error {
	if w.blocked.Load() {
		select {
		case w.entered <- struct{}{}:
		default:
		}
		select {
		case <-w.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if w.offline.Load() {
		return errors.New("fixture worker down")
	}
	w.mu.RLock()
	defer w.mu.RUnlock()
	w.publisher.Wake()
	w.dispatcher.Wake()
	return nil
}
func (w *worker) WakePaymentReconciler(context.Context) error {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.offline.Load() {
		return errors.New("fixture worker down")
	}
	w.payments.Wake()
	return nil
}
func (w *worker) TestNotificationChannel(context.Context, string) (workerrpc.TestNotificationResponse, error) {
	return workerrpc.TestNotificationResponse{}, errors.New("not part of payment fixture")
}
func (w *worker) Quiesce(ctx context.Context) (workerrpc.QuiesceResponse, error) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	err := w.dispatcher.Drain(ctx)
	return workerrpc.QuiesceResponse{Quiesced: err == nil, ActiveDeliveries: int(w.dispatcher.ActiveDeliveries())}, err
}
func (w *worker) Resume(context.Context) error {
	w.mu.RLock()
	defer w.mu.RUnlock()
	w.dispatcher.Resume()
	return nil
}

type consumer struct {
	cancel context.CancelFunc
	done   chan struct{}
	ready  chan struct{}
	err    chan error
}

func consume(parent context.Context, base string, m *measurements, lastID string) *consumer {
	ctx, cancel := context.WithCancel(parent)
	c := &consumer{cancel: cancel, done: make(chan struct{}), ready: make(chan struct{}), err: make(chan error, 1)}
	go func() {
		defer close(c.done)
		req, err := http.NewRequestWithContext(ctx, "GET", base+"/api/public/v1/events", nil)
		if err != nil {
			c.err <- err
			return
		}
		if lastID != "" {
			req.Header.Set("Last-Event-ID", lastID)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			c.err <- err
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			c.err <- fmt.Errorf("SSE status %d", resp.StatusCode)
			return
		}
		scan := bufio.NewScanner(resp.Body)
		scan.Buffer(make([]byte, 4096), 1024*1024)
		event, data, eventID := "", "", ""
		initialized := false
		for scan.Scan() {
			line := scan.Text()
			if strings.HasPrefix(line, "id:") {
				eventID = strings.TrimSpace(strings.TrimPrefix(line, "id:"))
			}
			if strings.HasPrefix(line, "event:") {
				event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			}
			if strings.HasPrefix(line, "data:") {
				data += strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			}
			if line != "" {
				continue
			}
			if !initialized && (event == "initial_state" || event == "bank.transaction.credit" || event == "realtime.ready") {
				initialized = true
				close(c.ready)
			}
			if event == "bank.transaction.credit" && m != nil {
				var payload struct {
					TransactionID string `json:"transactionId"`
				}
				if err := json.Unmarshal([]byte(data), &payload); err != nil || payload.TransactionID == "" {
					c.err <- errors.New("invalid credit SSE payload")
					return
				}
				if err := m.received(payload.TransactionID, eventID); err != nil {
					c.err <- err
					return
				}
			}
			event, data, eventID = "", "", ""
		}
		if ctx.Err() == nil {
			if err := scan.Err(); err != nil {
				c.err <- err
			} else {
				c.err <- errors.New("unexpected SSE end")
			}
		}
	}()
	return c
}
func (c *consumer) waitReady(ctx context.Context) error {
	select {
	case <-c.ready:
		return nil
	case err := <-c.err:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (c *consumer) close() { c.cancel(); <-c.done }
func waitUntil(ctx context.Context, fn func() (bool, error)) error {
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		ok, err := fn()
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		select {
		case <-tick.C:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
func pause(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// The diagnostic sink permits only the fixed storage-operation diagnostic;
// normal production logs (which may contain references) remain discarded.
type fixtureDiagnosticLog struct {
	mu      sync.Mutex
	records []map[string]any
}

func (l *fixtureDiagnosticLog) Write(raw []byte) (int, error) {
	var record map[string]any
	if json.Unmarshal(raw, &record) == nil && record["msg"] == "payment storage operation failed" {
		allowed := map[string]any{"operation": record["operation"], "failureClass": record["failureClass"], "sqliteCode": record["sqliteCode"]}
		l.mu.Lock()
		if len(l.records) < 20 {
			l.records = append(l.records, allowed)
		}
		l.mu.Unlock()
	}
	return len(raw), nil
}
func (l *fixtureDiagnosticLog) snapshot() []map[string]any {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]map[string]any(nil), l.records...)
}

type fixtureProfileBuffer struct{ bytes.Buffer }

func (b *fixtureProfileBuffer) Write(raw []byte) (int, error) {
	const capBytes = 256 * 1024
	size := len(raw)
	if remaining := capBytes - b.Len(); remaining > 0 {
		if len(raw) > remaining {
			raw = raw[:remaining]
		}
		_, _ = b.Buffer.Write(raw)
	}
	return size, nil
}

type harness struct {
	ctx                context.Context
	cancel             context.CancelFunc
	stores             []*storage.Store
	gatewayA, gatewayB *httpapi.Server
	payB               *payments.Service
	sepayB             *sepay.Service
	sepayConfig        sepay.Config
	nativeConfig       sepay.Config
	metrics            *measurements
	worker             *worker
	gate               *streamGate
	rpcClient          *workerrpc.Client
	baseA, baseB       string
	client             *http.Client
	index              atomic.Int64
	endpoint           string
	mainConsumer       *consumer
	opts               options
	rep                *report
	closers            []func()
	stopA              func()
	startA             func() error
	stopWorker         func()
	startWorker        func() error
	wakeA, wakeB       func(context.Context) error
	failureOnce        sync.Once
}

func (h *harness) close() {
	h.cancel()
	for i := len(h.closers) - 1; i >= 0; i-- {
		h.closers[i]()
	}
}
func (h *harness) post(path string, body []byte, headers map[string]string, expectedStatus int, discardACK bool) error {
	req, err := http.NewRequestWithContext(h.ctx, "POST", h.baseB+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	start := time.Now()
	resp, err := h.client.Do(req)
	elapsed := time.Since(start)
	h.metrics.mu.Lock()
	p := h.metrics.activePhase
	retainSample(h.metrics.ack, h.metrics.ackCount, p, elapsed)
	h.metrics.mu.Unlock()
	if err != nil {
		return fmt.Errorf("fixture callback transport: %w", err)
	}
	if discardACK {
		_ = resp.Body.Close()
	} else {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 65536))
		_ = resp.Body.Close()
	}
	if resp.StatusCode != expectedStatus {
		return fmt.Errorf("callback %s returned %d, want %d", path, resp.StatusCode, expectedStatus)
	}
	return nil
}
func (h *harness) order(amount int64) (storage.PaymentOrder, error) {
	i := h.index.Add(1)
	key := fmt.Sprintf("00000000-0000-4000-8000-%012d", i)
	o, created, err := h.payB.CreateOrder(h.ctx, amount, "OPERATOR_DYNAMIC", key)
	if err != nil {
		h.failureOnce.Do(func() {
			var trace fixtureProfileBuffer
			// debug=1 reports function stacks/counts, not argument values or
			// request payloads. Only collect on failure, never every credit.
			if profile := pprof.Lookup("goroutine"); profile != nil {
				_ = profile.WriteTo(&trace, 1)
			}
			h.rep.FailureGoroutines = trace.String()
		})
		// The production service intentionally sanitizes storage/lifecycle
		// failures. In this isolated fixture, retain the harness lifecycle
		// cause so a launcher interrupt cannot masquerade as provider expiry.
		if cause := h.ctx.Err(); cause != nil {
			return o, fmt.Errorf("fixture create canceled by harness lifecycle: %w", cause)
		}
		lookupCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		saved, lookupErr := h.stores[1].PaymentOrderByKey(lookupCtx, "fixture-channel", key)
		stage, status := "before_reservation", "absent"
		if lookupErr == nil {
			stage, status = "after_reservation", saved.Status
		}
		if created {
			stage = "after_reservation"
		}
		gateState := "allowed"
		if gateErr := h.stores[1].CheckMutationAllowed(lookupCtx); gateErr != nil {
			gateState = "unavailable"
			if errors.Is(gateErr, context.Canceled) {
				gateState = "context_canceled"
			}
			if errors.Is(gateErr, context.DeadlineExceeded) {
				gateState = "context_deadline"
			}
			if errors.Is(gateErr, storage.ErrMutationGateLocked) {
				gateState = "locked"
			}
		}
		pool := h.stores[1].DB().Stats()
		return o, fmt.Errorf("fixture create stage=%s created=%t status=%s configured=%s mutationGate=%s poolOpen=%d poolInUse=%d poolWaitCount=%d: %w", stage, created, status, h.payB.Config().Status, gateState, pool.OpenConnections, pool.InUse, pool.WaitCount, err)
	}
	if !created || o.Status != "PENDING" {
		return o, errors.New("fixture order not newly pending")
	}
	return o, nil
}
func (h *harness) settle(amount int64) error {
	o, err := h.order(amount)
	if err != nil {
		return err
	}
	body := paymentBody(o)
	h.metrics.expect("payos", amount)
	for range 2 {
		if err := h.post(payOSPath, body, nil, 200, false); err != nil {
			return err
		}
	}
	saved, err := h.stores[0].PaymentOrder(h.ctx, o.ID)
	if err != nil {
		return err
	}
	if saved.Status != "PAID" {
		return errors.New("ACK did not settle valid fixture payment")
	}
	return nil
}
func seedLegacy(ctx context.Context, s *storage.Store) error {
	// Historical data is inserted only into this harness's newly-created DB;
	// no retired ACB runtime/provider is revived.
	_, err := s.DB().ExecContext(ctx, `
INSERT INTO connections(id,bank_code,account_masked,state,generation,created_at,updated_at) VALUES('fixture-legacy-acb','ACB','***8888','PAUSED',7,'2026-09-01T00:00:00Z','2026-09-01T00:00:00Z');
INSERT INTO transactions(id,connection_id,semantic_key,canonical_hash,transaction_date,effective_date,debit,credit,balance,parser_version,baseline_state,first_seen_at,transaction_at_iso,transaction_day,ingest_source) VALUES
('fixture-legacy-credit','fixture-legacy-acb','ACB:LEGACY-CREDIT','fixture-legacy-credit-hash','01/09/2026','01/09/2026',0,100000,500000,'acb-v1','NONE','2026-09-01T00:00:00Z','2026-09-01T00:00:00Z','2026-09-01','REALTIME'),
('fixture-legacy-debit','fixture-legacy-acb','ACB:LEGACY-DEBIT','fixture-legacy-debit-hash','01/09/2026','01/09/2026',25000,0,475000,'acb-v1','NONE','2026-09-01T00:00:00Z','2026-09-01T00:00:00Z','2026-09-01','REALTIME');
INSERT INTO events(id,transaction_id,event_type,payload,payload_hash,created_at) VALUES('fixture-legacy-event','fixture-legacy-credit','bank.transaction.credit','{"transactionId":"fixture-legacy-credit","credit":"100000","bank":"ACB"}','fixture-legacy-event-hash','2026-09-01T00:00:00Z');
INSERT INTO event_journal(epoch,event_type,aggregate_id,payload_json,created_at) VALUES('ep1','bank.transaction.credit','fixture-legacy-credit','{"transactionId":"fixture-legacy-credit","credit":"100000","bank":"ACB"}','2026-09-01T00:00:00Z');
INSERT INTO webhook_endpoints(id,name,status,current_revision,created_at,updated_at) VALUES('fixture-legacy-endpoint','Retained fixture outbox','DISABLED',1,'2026-09-01T00:00:00Z','2026-09-01T00:00:00Z');
INSERT INTO endpoint_versions(endpoint_id,revision,url,filters_json,created_at) VALUES('fixture-legacy-endpoint',1,'http://127.0.0.1:1/retained','[]','2026-09-01T00:00:00Z');
INSERT INTO deliveries(id,event_id,endpoint_id,endpoint_revision,key_id,status,attempts,next_attempt_at,created_at,updated_at) VALUES('fixture-legacy-delivery','fixture-legacy-event','fixture-legacy-endpoint',1,'retained-key','PENDING',2,'2099-01-01T00:00:00Z','2026-09-01T00:00:00Z','2026-09-01T00:00:00Z');`)
	return err
}
func setup(ctx context.Context, opts options, rep *report) (_ *harness, resultErr error) {
	ctx, cancel := context.WithCancel(ctx)
	h := &harness{ctx: ctx, cancel: cancel, opts: opts, rep: rep, metrics: newMeasurements(), client: &http.Client{Timeout: 15 * time.Second}}
	defer func() {
		if resultErr != nil {
			h.close()
		}
	}()
	dir, err := os.MkdirTemp("", "payment-realtime-fixture-")
	if err != nil {
		return nil, err
	}
	h.closers = append(h.closers, func() { _ = os.RemoveAll(dir) })
	key, err := randomToken()
	if err != nil {
		return nil, err
	}
	keyPath := filepath.Join(dir, "master.key")
	if err := os.WriteFile(keyPath, []byte(key), 0600); err != nil {
		return nil, err
	}
	keyring, err := security.LoadKeyring(keyPath)
	if err != nil {
		return nil, err
	}
	dbPath := filepath.Join(dir, "fixture.db")
	for range 3 {
		s, err := storage.Open(ctx, dbPath)
		if err != nil {
			return nil, err
		}
		s.WithKeyring(keyring)
		h.stores = append(h.stores, s)
		h.closers = append(h.closers, func() { _ = s.Close() })
	}
	if err := seedLegacy(ctx, h.stores[0]); err != nil {
		return nil, err
	}
	var sinkSecret string
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(io.LimitReader(r.Body, 1024*1024))
		if err != nil {
			http.Error(w, "read failed", 400)
			return
		}
		var payload struct {
			TransactionID string `json:"transactionId"`
		}
		if r.Method != "POST" || json.Unmarshal(raw, &payload) != nil || payload.TransactionID == "" || r.Header.Get("X-Bank-Signature") == "" {
			http.Error(w, "invalid signed delivery", 400)
			return
		}
		if err := webhook.Verify([]byte(sinkSecret), raw, r.Header.Get("X-Bank-Timestamp"), r.Header.Get("X-Bank-Nonce"), r.Header.Get("X-Bank-Signature"), time.Now(), 5*time.Minute); err != nil {
			http.Error(w, "invalid signature", 401)
			return
		}
		h.metrics.mu.Lock()
		h.metrics.delivered++
		h.metrics.mu.Unlock()
		w.WriteHeader(204)
	}))
	h.closers = append(h.closers, sink.Close)
	ep, err := h.stores[0].CreateEndpointWithSecret(ctx, "Disposable loopback outbox", sink.URL)
	if err != nil {
		return nil, err
	}
	h.endpoint = ep.ID
	sinkSecret = ep.Secret
	if err := h.stores[0].SetEndpointStatus(ctx, ep.ID, "ACTIVE"); err != nil {
		return nil, err
	}
	provider := fixtureProvider()
	h.closers = append(h.closers, provider.Close)
	adapter, err := payments.NewPayOS("fixture-channel", "fixture-api", checksum, payments.WithBaseURL(provider.URL), payments.WithHTTPClient(provider.Client()))
	if err != nil {
		return nil, err
	}
	cfg := config.Config{DevelopmentSubject: "fixture@example.test", Timezone: time.FixedZone("Asia/Ho_Chi_Minh", 7*3600), PayOSClientID: "fixture-channel", PayOSAPIKey: "fixture-api", PayOSChecksumKey: checksum, PaymentsEnabled: true, PayOSWebhookConfirmed: true, PaymentMaxAmountVND: 500000000, PaymentPublicOrigin: "http://127.0.0.1:5173"}
	registry := notification.NewRegistry()
	registry.Register(notification.ProviderWebhook, webhook.NewSender(&http.Client{Timeout: 3 * time.Second}, true))
	dispatcher := notification.NewDispatcher(h.stores[2], registry)
	workerHub := eventhub.New()
	publisher := realtimestream.NewJournalPublisher(h.stores[2], workerHub)
	if err := publisher.Init(ctx); err != nil {
		return nil, err
	}
	workerPayments := payments.NewService(cfg, h.stores[2], adapter, func(e storage.EventNotification) { h.metrics.committed(e); publisher.Wake(); dispatcher.Wake() })
	h.worker = &worker{publisher: publisher, dispatcher: dispatcher, payments: workerPayments, entered: make(chan struct{}, 1), release: make(chan struct{})}
	token, err := randomToken()
	if err != nil {
		return nil, err
	}
	rpc, err := workerrpc.NewServer(h.worker, token)
	if err != nil {
		return nil, err
	}
	rpcServer := httptest.NewServer(rpc.Handler())
	h.closers = append(h.closers, rpcServer.Close)
	h.rpcClient = workerrpc.NewClient(rpcServer.URL, token)
	h.gate = &streamGate{handler: realtimestream.NewServer(realtimestream.ServerConfig{Hub: workerHub, Token: token}), connections: map[int]context.CancelFunc{}}
	streamServer := httptest.NewServer(h.gate)
	h.closers = append(h.closers, streamServer.Close)
	startWorker := func() {
		workerCtx, stopWorker := context.WithCancel(ctx)
		var wg sync.WaitGroup
		for _, run := range []func(context.Context){publisher.Run, dispatcher.Start, workerPayments.Run} {
			wg.Add(1)
			go func() { defer wg.Done(); run(workerCtx) }()
		}
		h.stopWorker = func() { stopWorker(); wg.Wait() }
		h.closers = append(h.closers, h.stopWorker)
	}
	h.startWorker = func() error {
		workerHub = eventhub.New()
		publisher = realtimestream.NewJournalPublisher(h.stores[2], workerHub)
		if err := publisher.Init(ctx); err != nil {
			return err
		}
		dispatcher = notification.NewDispatcher(h.stores[2], registry)
		workerPayments = payments.NewService(cfg, h.stores[2], adapter, func(e storage.EventNotification) { h.metrics.committed(e); publisher.Wake(); dispatcher.Wake() })
		h.worker.mu.Lock()
		h.worker.publisher = publisher
		h.worker.dispatcher = dispatcher
		h.worker.payments = workerPayments
		h.worker.mu.Unlock()
		h.gate.mu.Lock()
		h.gate.handler = realtimestream.NewServer(realtimestream.ServerConfig{Hub: workerHub, Token: token})
		h.gate.mu.Unlock()
		startWorker()
		h.worker.offline.Store(false)
		return nil
	}
	startWorker()
	telegramSecret, err := randomToken()
	if err != nil {
		return nil, err
	}
	h.sepayConfig = sepay.Config{Mode: sepay.ModeActive, StoreKey: "fixture-telegram", StoreName: "Fixture Telegram", BankCode: "VCB", BankName: "Vietcombank", AccountNumber: "FIXTURETG75", AccountName: "FIXTURE", QRPayload: "0002010102116304ABCD", BotID: 900001, ChatID: -100900003, SenderBotID: 900002, WebhookSecret: telegramSecret, ActivationAt: time.Now().Add(-time.Hour).UTC()}
	nativeSecret, err := randomToken()
	if err != nil {
		return nil, err
	}
	h.nativeConfig = sepay.Config{Mode: sepay.ModeActive, ActiveTransport: "native", NativeMode: sepay.ModeActive, NativeWebhookSecret: nativeSecret, StoreKey: "fixture-native", StoreName: "Fixture Native", BankCode: "TCB", BankName: "Techcombank", AccountNumber: "FIXTURENATIVE200", AccountName: "FIXTURE", QRPayload: "0002010102116304ABCD", ActivationAt: time.Now().Add(-time.Hour).UTC()}
	startGateway := func(index int, address string) error {
		gatewayCtx, stopGateway := context.WithCancel(ctx)
		h.closers = append(h.closers, stopGateway)
		store := h.stores[index]
		signal := workerrpc.NewWakeSignal(h.rpcClient.WakeDispatcher, 2*time.Second)
		if index == 0 {
			h.wakeA = signal.Wake
		} else {
			h.wakeB = signal.Wake
		}
		reconcileSignal := workerrpc.NewWakeSignal(h.rpcClient.WakePaymentReconciler, 2*time.Second)
		go signal.Run(gatewayCtx)
		go reconcileSignal.Run(gatewayCtx)
		gateway := httpapi.New(cfg, store).WithEventHub(eventhub.New()).WithWakeDispatcher(signal.Wake)
		coordinator := httpapi.NewRealtimeCoordinator(gateway, time.Hour)
		gateway.WithRealtimeSubmit(coordinator.Submit)
		go coordinator.Run(gatewayCtx)
		if err := coordinator.WaitReady(gatewayCtx); err != nil {
			return err
		}
		commit := func(e storage.EventNotification) { h.metrics.committed(e); gateway.PaymentCommitNotifier()(e) }
		paymentService := payments.NewService(cfg, store, adapter, commit).WithReconcileWake(reconcileSignal.Wake)
		sepayService := sepay.NewService(h.sepayConfig, store, commit)
		gateway.WithPayments(paymentService).WithSePay(sepayService)
		if base := os.Getenv("TTS_BASE_URL"); base != "" {
			gateway.WithTTSClient(ttsclient.New(base, os.Getenv("TTS_INTERNAL_TOKEN")))
		}
		connected := make(chan struct{}, 1)
		stream, err := realtimestream.NewClient(realtimestream.ClientConfig{BaseURL: streamServer.URL, Token: token, InitialBackoff: 20 * time.Millisecond, MaxBackoff: 100 * time.Millisecond, OnConnect: func() {
			coordinator.RequestReconcile()
			select {
			case connected <- struct{}{}:
			default:
			}
		}})
		if err != nil {
			return err
		}
		go func() { _ = stream.Run(gatewayCtx, coordinator.Submit) }()
		select {
		case <-connected:
		case <-gatewayCtx.Done():
			return gatewayCtx.Err()
		}
		ln, err := net.Listen("tcp", address)
		if err != nil {
			return err
		}
		handler := gateway.Handler()
		if index == 1 {
			nativeService := sepay.NewService(h.nativeConfig, store, commit)
			nativeHandler := httpapi.New(cfg, store).WithSePay(nativeService).Handler()
			primaryHandler := handler
			handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/integrations/sepay/webhook" {
					nativeHandler.ServeHTTP(w, r)
					return
				}
				primaryHandler.ServeHTTP(w, r)
			})
		}
		server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}
		h.closers = append(h.closers, func() { _ = server.Close() })
		go func() { _ = server.Serve(ln) }()
		base := "http://" + ln.Addr().String()
		if index == 0 {
			h.gatewayA = gateway
			h.baseA = base
			h.stopA = func() { stopGateway(); _ = server.Close() }
		} else {
			h.gatewayB = gateway
			h.baseB = base
			h.payB = paymentService
			h.sepayB = sepayService
		}
		return nil
	}
	h.startA = func() error { return startGateway(0, opts.listenA) }
	for index, address := range []string{opts.listenA, opts.listenB} {
		if err := startGateway(index, address); err != nil {
			return nil, err
		}
	}
	h.mainConsumer = consume(ctx, h.baseA, h.metrics, "")
	h.closers = append(h.closers, func() { h.mainConsumer.close() })
	readyCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := h.mainConsumer.waitReady(readyCtx); err != nil {
		return nil, err
	}
	return h, nil
}

func (h *harness) telegramFixture() error {
	c := h.sepayConfig
	text := "SEPAY_STORE_V1\ndirection=vào\namount=75,000\naccount=" + c.AccountNumber + "\nbank=" + c.BankName + "\ntime=" + time.Now().In(time.FixedZone("VN", 7*3600)).Format("2006-01-02 15:04:05") + "\nreference=FIXTURE-TG-75\ncontent=fixture"
	body := encode(map[string]any{"update_id": 1, "message": map[string]any{"message_id": 1, "date": time.Now().Unix(), "chat": map[string]any{"id": c.ChatID, "type": "supergroup"}, "from": map[string]any{"id": c.SenderBotID, "is_bot": true, "first_name": "SePay"}, "text": text}})
	h.metrics.expect("telegram", 75000)
	for range 2 {
		if err := h.post("/api/integrations/sepay/telegram", body, map[string]string{"X-Telegram-Bot-Api-Secret-Token": c.WebhookSecret}, 200, false); err != nil {
			return err
		}
	}
	return nil
}
func (h *harness) nativeFixture() error {
	c := h.nativeConfig
	body := encode(map[string]any{"id": int64(9007199254740993), "gateway": c.BankName, "transactionDate": time.Now().In(time.FixedZone("VN", 7*3600)).Format("2006-01-02 15:04:05"), "accountNumber": c.AccountNumber, "subAccount": "", "code": nil, "content": "fixture tiếng Việt", "description": "", "transferType": "in", "transferAmount": 200000, "accumulated": 200000, "referenceCode": "FIXTURE-NATIVE-200"})
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, []byte(c.NativeWebhookSecret))
	_, _ = mac.Write([]byte(timestamp + "."))
	_, _ = mac.Write(body)
	headers := map[string]string{"X-SePay-Timestamp": timestamp, "X-SePay-Signature": "sha256=" + hex.EncodeToString(mac.Sum(nil))}
	h.metrics.expect("native", 200000)
	for range 2 {
		if err := h.post("/api/integrations/sepay/webhook", body, headers, 200, false); err != nil {
			return err
		}
	}
	return nil
}
func (h *harness) catchUp() error {
	ctx, cancel := context.WithTimeout(h.ctx, 20*time.Second)
	defer cancel()
	err := waitUntil(ctx, func() (bool, error) {
		select {
		case err := <-h.mainConsumer.err:
			return false, err
		default:
		}
		h.metrics.mu.Lock()
		m := h.metrics
		ready := m.observedCommits == m.expectedCredits && m.observedEvents == m.expectedCredits && len(m.commits) == 0 && len(m.arrivals) == 0 && m.delivered >= m.expectedCredits
		h.metrics.mu.Unlock()
		if !ready {
			return false, nil
		}
		// Receipt of the sink HTTP request is not delivery completion. Wait
		// for the dispatcher's committed DELIVERED update before stopping its
		// lifecycle; otherwise cancellation can strand a claimed row under
		// its real 30s lease while the next phase waits only 20s.
		var pending int64
		err := h.stores[0].DB().QueryRowContext(ctx, `SELECT count(*) FROM deliveries WHERE id!='fixture-legacy-delivery' AND status!='DELIVERED'`).Scan(&pending)
		return pending == 0, err
	})
	if err != nil {
		return h.phaseError("catch_up", err)
	}
	return nil
}
func (h *harness) phaseError(stage string, err error) error {
	m := h.metrics
	m.mu.Lock()
	phase, expected, commits, events, sink, unmatchedCommits, unmatchedEvents := m.activePhase, m.expectedCredits, m.observedCommits, m.observedEvents, m.delivered, len(m.commits), len(m.arrivals)
	m.mu.Unlock()
	var pending, inFlight, delivered int64
	queryCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	dbErr := h.stores[0].DB().QueryRowContext(queryCtx, `SELECT coalesce(sum(status='PENDING'),0),coalesce(sum(status='IN_FLIGHT'),0),coalesce(sum(status='DELIVERED'),0) FROM deliveries WHERE id!='fixture-legacy-delivery'`).Scan(&pending, &inFlight, &delivered)
	databaseState := fmt.Sprintf("pending=%d inFlight=%d delivered=%d", pending, inFlight, delivered)
	if dbErr != nil {
		databaseState = "delivery-state-unavailable"
	}
	lifecycleState := "active"
	if cause := h.ctx.Err(); cause != nil {
		lifecycleState = cause.Error()
	}
	return fmt.Errorf("phase=%s stage=%s lifecycle=%s expected=%d commits=%d sse=%d sinkRequests=%d unmatchedCommits=%d unmatchedEvents=%d %s: %w", phase, stage, lifecycleState, expected, commits, events, sink, unmatchedCommits, unmatchedEvents, databaseState, err)
}
func (h *harness) faultChecks() error {
	h.metrics.phase("degraded_rpc_blocked")
	h.worker.blocked.Store(true)
	// Enter the real HTTP RPC before posting the callback. A single coalesced
	// hint runner can then become full without blocking a request goroutine.
	rpcCtx, rpcCancel := context.WithCancel(h.ctx)
	rpcDone := make(chan error, 1)
	go func() { rpcDone <- h.rpcClient.WakeDispatcher(rpcCtx) }()
	defer rpcCancel()
	select {
	case <-h.worker.entered:
	case <-h.ctx.Done():
		rpcCancel()
		return h.ctx.Err()
	}
	for range 1000 {
		if err := h.wakeA(h.ctx); err != nil {
			return err
		}
		if err := h.wakeB(h.ctx); err != nil {
			return err
		}
	}
	err := h.settle(31000)
	select {
	case <-rpcDone:
		h.worker.blocked.Store(false)
		close(h.worker.release)
		return errors.New("blocked worker RPC ended before webhook ACK; causal ACK assertion unavailable")
	default:
	}
	// The RPC release is intentionally AFTER both webhook responses.
	h.worker.blocked.Store(false)
	close(h.worker.release)
	rpcCancel()
	<-rpcDone
	if err != nil {
		return err
	}
	h.rep.Faults["blockedWorkerRPC"] = "PASS: HTTP callbacks ACKed while a real worker RPC was blocked; release occurred only after ACK"
	h.rep.Faults["coalescedWakeFlood"] = "PASS: 1,000 wake hints sent to each gateway while runner RPC was blocked; webhook still ACKed (no assertion about private channel occupancy)"
	if err := h.catchUp(); err != nil {
		return err
	}

	h.metrics.phase("degraded_stream_disconnect")
	h.gate.disable(true)
	if err := h.settle(32000); err != nil {
		h.gate.disable(false)
		return err
	}
	h.gate.disable(false)
	if err := h.catchUp(); err != nil {
		return err
	}
	if err := h.phaseCheckpoint("stream_disconnect"); err != nil {
		return err
	}
	h.rep.Faults["workerStreamDisconnect"] = "PASS: committed during stream outage, journal replay on reconnect recovered credit once"

	h.metrics.phase("degraded_write_failure")
	o, err := h.order(33000)
	if err != nil {
		return err
	}
	body := paymentBody(o)
	_, err = h.stores[0].DB().ExecContext(h.ctx, `CREATE TRIGGER fixture_fail_credit BEFORE INSERT ON transactions BEGIN SELECT RAISE(ABORT,'fixture write failure'); END`)
	if err != nil {
		return err
	}
	failErr := h.post(payOSPath, body, nil, 503, false)
	_, dropErr := h.stores[0].DB().ExecContext(h.ctx, `DROP TRIGGER fixture_fail_credit`)
	if failErr != nil {
		return failErr
	}
	if dropErr != nil {
		return dropErr
	}
	var before int
	if err := h.stores[0].DB().QueryRowContext(h.ctx, `SELECT count(*) FROM payment_receipts WHERE order_id=?`, o.ID).Scan(&before); err != nil {
		return err
	}
	if before != 0 {
		return errors.New("failed commit retained a receipt")
	}
	h.metrics.expect("payos", 33000)
	for range 2 {
		if err := h.post(payOSPath, body, nil, 200, false); err != nil {
			return err
		}
	}
	h.rep.Faults["databaseWriteFailure"] = "PASS: real SQLite transaction failure returned 503 without receipt; retry settled once"

	h.metrics.phase("degraded_discarded_ack")
	o, err = h.order(34000)
	if err != nil {
		return err
	}
	body = paymentBody(o)
	h.metrics.expect("payos", 34000)
	if err := h.post(payOSPath, body, nil, 200, true); err != nil {
		return err
	}
	if err := h.post(payOSPath, body, nil, 200, false); err != nil {
		return err
	}
	h.rep.Faults["discardedACKRetry"] = "PASS: client discarded first ACK body after commit; identical retry produced no additional credit (not a wire-level packet-loss test)"
	if err := h.catchUp(); err != nil {
		return err
	}

	h.metrics.phase("degraded_worker_restart")
	h.gate.disable(true)
	h.worker.offline.Store(true)
	h.stopWorker()
	if err := h.settle(35000); err != nil {
		return err
	}
	if err := h.startWorker(); err != nil {
		return err
	}
	h.gate.disable(false)
	if err := h.catchUp(); err != nil {
		return err
	}
	h.rep.Faults["workerRestart"] = "PASS: worker publisher/dispatcher/reconciler runners stopped, callback committed, new journal publisher initialized and stream reconnect recovered once"

	h.metrics.phase("degraded_gateway_restart")
	h.metrics.mu.Lock()
	lastID := h.metrics.lastEventID
	h.metrics.mu.Unlock()
	h.mainConsumer.close()
	h.stopA()
	if err := h.settle(36000); err != nil {
		return err
	}
	if err := h.startA(); err != nil {
		return err
	}
	h.mainConsumer = consume(h.ctx, h.baseA, h.metrics, lastID)
	if err := h.catchUp(); err != nil {
		return err
	}
	h.rep.Faults["gatewayRestart"] = "PASS: gateway A coordinator/stream/HTTP owner restarted; Last-Event-ID replay recovered callback received by B while A was down"
	if err := h.phaseCheckpoint("restarts"); err != nil {
		return err
	}
	h.metrics.phase("degraded_slow_sse")
	parsed, err := url.Parse(h.baseA)
	if err != nil {
		return err
	}
	conn, err := net.DialTimeout("tcp", parsed.Host, 5*time.Second)
	if err != nil {
		return err
	}
	// Deliberately never read this subscriber's HTTP headers or body. The
	// normal measured SSE subscriber must keep receiving exact credits.
	_, err = fmt.Fprintf(conn, "GET /api/public/v1/events HTTP/1.1\r\nHost: %s\r\nConnection: keep-alive\r\n\r\n", parsed.Host)
	if err == nil {
		err = h.settle(37000)
	}
	_ = conn.Close()
	if err != nil {
		return err
	}
	if err := h.catchUp(); err != nil {
		return err
	}
	h.rep.Faults["unreadSSESubscriber"] = "PASS: independent subscriber read no bytes while normal SSE delivered credit; not a saturated-socket or browser hidden test"
	return nil
}
func (h *harness) load() error {
	if h.opts.scenario == "smoke" {
		return pause(h.ctx, h.opts.duration)
	}
	h.metrics.phase("warm_load")
	started := time.Now()
	h.metrics.mu.Lock()
	creditsBefore := h.metrics.observedCommits
	h.metrics.mu.Unlock()
	var periodicScheduled int64
	defer func() {
		elapsed := time.Since(started).Seconds()
		h.metrics.mu.Lock()
		accepted := h.metrics.observedCommits - creditsBefore
		h.metrics.mu.Unlock()
		h.rep.Load = map[string]any{"requestedPeriodicRate": h.opts.rate, "requestedPeriodicDurationSeconds": h.opts.duration.Seconds(), "burstUnique": h.opts.burst, "periodicUniqueScheduled": periodicScheduled, "acceptedUnique": accepted, "elapsedIncludingBurstAndDrainSeconds": elapsed, "achievedUniquePerSecondIncludingBurstAndDrain": float64(accepted) / elapsed, "maxCallbackWorkers": 16, "maxPendingJobs": 32}
	}()
	workCtx, cancel := context.WithCancel(h.ctx)
	defer cancel()
	jobs := make(chan int64, 32)
	errorsCh := make(chan error, 1)
	var workers sync.WaitGroup
	for range 16 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for amount := range jobs {
				if err := h.settle(amount); err != nil {
					select {
					case errorsCh <- err:
					default:
					}
					cancel()
					return
				}
			}
		}()
	}
	// Burst size means unique accepted identities, each followed by one replay.
	for i := range h.opts.burst {
		select {
		case jobs <- 10000 + int64(i):
		case <-workCtx.Done():
			close(jobs)
			workers.Wait()
			select {
			case err := <-errorsCh:
				return err
			default:
				return workCtx.Err()
			}
		}
	}
	interval := time.Duration(float64(time.Second) / h.opts.rate)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	deadline := time.NewTimer(h.opts.duration)
	defer deadline.Stop()
	resourceTick := time.NewTicker(time.Minute)
	defer resourceTick.Stop()
	scheduling := true
	for scheduling {
		select {
		case <-deadline.C:
			scheduling = false
		case <-resourceTick.C:
			h.rep.Lifecycle = append(h.rep.Lifecycle, snapshot())
		case <-ticker.C:
			select {
			case jobs <- 50000:
				periodicScheduled++
			case <-workCtx.Done():
				scheduling = false
			}
		case <-workCtx.Done():
			scheduling = false
		}
	}
	close(jobs)
	workers.Wait()
	select {
	case err := <-errorsCh:
		return err
	default:
	}
	if h.ctx.Err() != nil {
		return h.ctx.Err()
	}
	return h.catchUp()
}
func (h *harness) lifecycle() error {
	h.metrics.phase("lifecycle")
	for i := range 100 {
		// Keep the measured A consumer alive. These independent real HTTP SSE
		// clients exercise gateway subscriber registration/cancellation only.
		c := consume(h.ctx, h.baseA, nil, "")
		readyCtx, cancel := context.WithTimeout(h.ctx, 5*time.Second)
		err := c.waitReady(readyCtx)
		cancel()
		c.close()
		if err != nil {
			return err
		}
		if (i+1)%25 == 0 {
			h.rep.Lifecycle = append(h.rep.Lifecycle, snapshot())
		}
	}
	h.rep.Faults["sseReconnectCancel100"] = "PASS: 100 real SSE connect/initial-state/cancel cycles completed; resource samples reported, not proof of absence of leaks"
	return nil
}
func (h *harness) assertCounts() error {
	m := h.metrics
	m.mu.Lock()
	n, total, pay, tg, native := m.expectedCredits, m.expectedTotal, m.expectedPayOS, m.expectedTelegram, m.expectedNative
	m.mu.Unlock()
	h.rep.Expected = map[string]int64{"transactions": n + 2, "creditVnd": total + 100000, "debitVnd": 25000, "payment_receipts": pay, "sepay_receipts": tg, "sepay_native_inbox": native, "events": n + 1, "event_journal": n + 1, "deliveries": n + 1}
	queries := map[string]string{"transactions": `SELECT count(*) FROM transactions`, "creditVnd": `SELECT coalesce(sum(credit),0) FROM transactions`, "debitVnd": `SELECT coalesce(sum(debit),0) FROM transactions`, "payment_receipts": `SELECT count(*) FROM payment_receipts`, "sepay_receipts": `SELECT count(*) FROM sepay_receipts`, "sepay_native_inbox": `SELECT count(*) FROM sepay_native_inbox`, "events": `SELECT count(*) FROM events`, "event_journal": `SELECT count(*) FROM event_journal`, "deliveries": `SELECT count(*) FROM deliveries`}
	h.rep.Counts = map[string]int64{}
	var assertionErrors []string
	for key, query := range queries {
		var actual int64
		if err := h.stores[0].DB().QueryRowContext(h.ctx, query).Scan(&actual); err != nil {
			return err
		}
		h.rep.Counts[key] = actual
		if actual != h.rep.Expected[key] {
			assertionErrors = append(assertionErrors, fmt.Sprintf("%s=%d want %d", key, actual, h.rep.Expected[key]))
		}
	}
	integrityQueries := map[string]string{
		"retainedOutbox":            `SELECT count(*) FROM deliveries WHERE id='fixture-legacy-delivery' AND event_id='fixture-legacy-event' AND endpoint_id='fixture-legacy-endpoint' AND status='PENDING' AND attempts=2 AND next_attempt_at='2099-01-01T00:00:00Z'`,
		"mismatchedPayOSProjection": `SELECT count(*) FROM payment_orders o JOIN payment_receipts r ON r.order_id=o.id JOIN transactions t ON t.id=r.transaction_id WHERE o.status!='PAID' OR o.transaction_id!=t.id OR o.amount_vnd!=t.credit OR r.amount_vnd!=t.credit`,
		"duplicateFinancialEvents":  `SELECT count(*) FROM (SELECT transaction_id FROM events GROUP BY transaction_id,event_type HAVING count(*)!=1)`,
		"orphanFinancialJournal": `SELECT count(*) FROM transactions t
			LEFT JOIN (SELECT aggregate_id,count(*) AS n FROM event_journal WHERE event_type='bank.transaction.credit' GROUP BY aggregate_id) j ON j.aggregate_id=t.id
			LEFT JOIN (SELECT transaction_id,count(*) AS n FROM events WHERE event_type='bank.transaction.credit' GROUP BY transaction_id) e ON e.transaction_id=t.id
			WHERE t.credit>0 AND t.id!='fixture-legacy-credit' AND (coalesce(j.n,0)!=1 OR coalesce(e.n,0)!=1)`,
	}
	for key, query := range integrityQueries {
		var actual int64
		if err := h.stores[0].DB().QueryRowContext(h.ctx, query).Scan(&actual); err != nil {
			return err
		}
		h.rep.Counts[key] = actual
		expected := int64(0)
		if key == "retainedOutbox" {
			expected = 1
		}
		if actual != expected {
			assertionErrors = append(assertionErrors, fmt.Sprintf("%s=%d want %d", key, actual, expected))
		}
	}
	var delivered, retained int64
	if err := h.stores[0].DB().QueryRowContext(h.ctx, `SELECT count(*) FROM deliveries WHERE endpoint_id=? AND status='DELIVERED'`, h.endpoint).Scan(&delivered); err != nil {
		return err
	}
	h.rep.Counts["successfulDeliveries"] = delivered
	if delivered != n {
		assertionErrors = append(assertionErrors, fmt.Sprintf("successful deliveries=%d want %d", delivered, n))
	}
	if err := h.stores[0].DB().QueryRowContext(h.ctx, `SELECT count(*) FROM transactions WHERE (id='fixture-legacy-credit' AND canonical_hash='fixture-legacy-credit-hash' AND credit=100000) OR (id='fixture-legacy-debit' AND canonical_hash='fixture-legacy-debit-hash' AND debit=25000)`).Scan(&retained); err != nil {
		return err
	}
	if retained != 2 {
		assertionErrors = append(assertionErrors, "historical ACB identities/hash/amounts changed")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	h.rep.Missing = len(m.commits)
	h.rep.Unexpected = len(m.arrivals)
	h.rep.Duplicate = int(m.duplicateEvents)
	for phase, samples := range m.ack {
		h.rep.Phases[phase] = phaseMetrics{summarize(samples, m.ackCount[phase]), summarize(m.latency[phase], m.latencyCount[phase])}
	}
	h.rep.Counts["uniqueObservedCommits"] = m.observedCommits
	h.rep.Counts["uniqueSseCredits"] = m.observedEvents
	h.rep.Counts["latencySamplesUnavailable"] = m.unavailableLatency
	if h.rep.Missing != 0 || h.rep.Duplicate != 0 || h.rep.Unexpected != 0 {
		assertionErrors = append(assertionErrors, fmt.Sprintf("SSE missing=%d duplicate=%d unexpected=%d", h.rep.Missing, h.rep.Duplicate, h.rep.Unexpected))
	}
	if len(assertionErrors) != 0 {
		return errors.New(strings.Join(assertionErrors, "; "))
	}
	return nil
}
func (h *harness) audio() error {
	base := os.Getenv("TTS_BASE_URL")
	if base == "" {
		return nil
	}
	// Use the same public backend voice resolver the browser test button uses;
	// a successful download is synthesis evidence, never listening evidence.
	req, err := http.NewRequestWithContext(h.ctx, "GET", h.baseA+"/api/public/v1/voice/profile", nil)
	if err != nil {
		return err
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return err
	}
	var profile storage.VoiceProfile
	err = json.NewDecoder(io.LimitReader(resp.Body, 1024*1024)).Decode(&profile)
	_ = resp.Body.Close()
	if err != nil || resp.StatusCode != 200 {
		return errors.New("local TTS configured but public voice profile unavailable")
	}
	expectedProvider := profile.SynthesisProvider()
	if expectedProvider != "vieneu" || profile.ProviderMode != storage.ProviderModeVieneuOnly || profile.VoiceID != storage.VoiceIDVieneuMaiAnh || profile.Rate != 1 || profile.PitchHz != 0 || profile.SynthesisKey != storage.ComputeSynthesisKey(storage.VoiceIDVieneuMaiAnh, 1, 0) {
		return errors.New("local TTS smoke requires the fixed VieNeu Mai Anh profile")
	}
	start := time.Now()
	req, err = http.NewRequestWithContext(h.ctx, "POST", h.baseA+"/api/public/v1/voice/test", bytes.NewReader(encode(map[string]any{"profileRevision": profile.Revision, "part": "full"})))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err = h.client.Do(req)
	if err != nil {
		h.rep.Audio = map[string]any{"status": "EXTERNAL_FAILURE", "listening": "NOT_RUN", "error": "local fixed-voice synthesis transport failed"}
		return nil
	}
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024+1))
	_ = resp.Body.Close()
	mpeg := len(raw) >= 3 && (string(raw[:3]) == "ID3" || (raw[0] == 0xff && raw[1]&0xe0 == 0xe0))
	valid := readErr == nil && resp.StatusCode == 200 && len(raw) <= 4*1024*1024 && mpeg &&
		resp.Header.Get("Content-Type") == "audio/mpeg" &&
		resp.Header.Get("X-TTS-Provider") == expectedProvider && resp.Header.Get("X-TTS-Voice") == profile.VoiceID &&
		resp.Header.Get("X-TTS-Fallback") == "false" &&
		resp.Header.Get("X-Voice-Revision") == strconv.FormatInt(profile.Revision, 10) &&
		resp.Header.Get("X-Voice-Synthesis-Key") == profile.SynthesisKey
	status := "PASS"
	if !valid {
		status = "EXTERNAL_FAILURE"
	}
	h.rep.Audio = map[string]any{"status": status, "listening": "NOT_RUN", "httpStatus": resp.StatusCode, "bytes": len(raw), "durationMs": float64(time.Since(start)) / float64(time.Millisecond), "provider": resp.Header.Get("X-TTS-Provider"), "voice": resp.Header.Get("X-TTS-Voice"), "revision": profile.Revision, "mpeg": mpeg, "expectedAmountText": voicecopy.SpeakVND(500000), "transcriptVerification": "NOT_RUN"}
	return nil
}
func (h *harness) phaseCheckpoint(name string) error {
	ctx, cancel := context.WithTimeout(h.ctx, 20*time.Second)
	defer cancel()
	if err := waitUntil(ctx, func() (bool, error) {
		var n int64
		err := h.stores[0].DB().QueryRowContext(ctx, `SELECT count(*) FROM deliveries WHERE id!='fixture-legacy-delivery' AND status!='DELIVERED'`).Scan(&n)
		return n == 0, err
	}); err != nil {
		return err
	}
	if err := h.assertCounts(); err != nil {
		return err
	}
	if h.rep.Checkpoints == nil {
		h.rep.Checkpoints = map[string]map[string]int64{}
	}
	h.rep.Checkpoints[name] = h.rep.Counts
	return nil
}
func (h *harness) poolSnapshot() map[string]any {
	pools := map[string]any{}
	for index, role := range []string{"gatewayA", "gatewayB", "worker"} {
		stats := h.stores[index].DB().Stats()
		pools[role] = map[string]any{"maxOpen": stats.MaxOpenConnections, "open": stats.OpenConnections, "inUse": stats.InUse, "idle": stats.Idle, "waitCount": stats.WaitCount, "waitDurationMs": float64(stats.WaitDuration) / float64(time.Millisecond)}
	}
	return pools
}
func execute(ctx context.Context, opts options, rep *report) (resultErr error) {
	runCtx, cancel := context.WithCancel(ctx)
	h, err := setup(runCtx, opts, rep)
	if err != nil {
		cancel()
		return err
	}
	defer func() { cancel(); h.close() }()
	defer func() {
		if resultErr != nil {
			resultErr = h.phaseError("execute", resultErr)
		}
	}()
	defer func() { rep.Final = snapshot(); _ = h.assertCounts(); rep.FinalPools = h.poolSnapshot() }()
	fmt.Fprintf(os.Stderr, "FIXTURE ONLY: gateway A %s; gateway B %s; disposable SQLite; batch starts in %s\n", h.baseA, h.baseB, opts.delay)
	if err := pause(ctx, opts.delay); err != nil {
		return err
	}
	h.metrics.phase("warm_seed")
	for _, amount := range []int64{50000, 50000, 120000} {
		if err := h.settle(amount); err != nil {
			return err
		}
	}
	if err := h.telegramFixture(); err != nil {
		return err
	}
	if err := h.nativeFixture(); err != nil {
		return err
	}
	if err := h.catchUp(); err != nil {
		return err
	}
	if err := h.phaseCheckpoint("seed"); err != nil {
		return err
	}
	rep.Warmup = snapshot()
	rep.WarmupPools = h.poolSnapshot()
	if err := h.faultChecks(); err != nil {
		return err
	}
	if err := h.phaseCheckpoint("faults"); err != nil {
		return err
	}
	if err := h.load(); err != nil {
		return err
	}
	if err := h.phaseCheckpoint("load"); err != nil {
		return err
	}
	if err := h.lifecycle(); err != nil {
		return err
	}
	if err := h.catchUp(); err != nil {
		return err
	}
	// Dispatcher success persistence follows sink response; wait for durable
	// success, not just the sink's receipt of the HTTP request.
	deliveryCtx, deliveryCancel := context.WithTimeout(ctx, 20*time.Second)
	defer deliveryCancel()
	if err := waitUntil(deliveryCtx, func() (bool, error) {
		var n int64
		err := h.stores[0].DB().QueryRowContext(ctx, `SELECT count(*) FROM deliveries WHERE id!='fixture-legacy-delivery' AND status!='DELIVERED'`).Scan(&n)
		return n == 0, err
	}); err != nil {
		return err
	}
	if err := h.audio(); err != nil {
		return err
	}
	rep.Final = snapshot()
	return h.assertCounts()
}
func main() {
	var opts options
	flag.StringVar(&opts.scenario, "scenario", "smoke", "smoke, load, or soak (disposable fixtures only)")
	flag.StringVar(&opts.listenA, "listen-a", "127.0.0.1:18081", "gateway A literal loopback listener")
	flag.StringVar(&opts.listenB, "listen-b", "127.0.0.1:18082", "gateway B literal loopback listener")
	flag.Float64Var(&opts.rate, "rate", 1, "unique callbacks per second; each has one duplicate retry")
	flag.DurationVar(&opts.duration, "duration", 90*time.Second, "smoke UI observation window or load/soak scheduling duration")
	flag.IntVar(&opts.burst, "burst", 100, "initial load/soak unique callback burst")
	flag.DurationVar(&opts.delay, "start-delay", 0, "delay for enabling browser audio before fixtures")
	flag.StringVar(&opts.output, "output", "", "JSON evidence file (stdout when omitted)")
	flag.Parse()
	started := time.Now()
	rep := &report{FixtureMode: true, Scenario: opts.scenario, StartedAt: started.UTC().Format(time.RFC3339Nano), OS: runtime.GOOS, Arch: runtime.GOARCH, Go: runtime.Version(), Rate: opts.rate, Burst: opts.burst, Status: "FAIL", Audio: map[string]any{"status": "NOT_RUN", "listening": "NOT_RUN", "reason": "TTS_BASE_URL not configured"}, Phases: map[string]phaseMetrics{}, Faults: map[string]string{}, Notes: []string{"Only disposable financial fixtures; never signed production callbacks.", "ACK measures harness HTTP request through response headers; same-process loopback, includes transport.", "Commit-observed→SSE uses one harness monotonic clock at the first post-commit service notifier and parsed HTTP SSE frame; excludes commit→notifier instructions, never substitutes journal created_at.", "Warm/degraded are separate phase samples. No browser/UI/chime/meaningful-speech latency is inferred from SSE or synthesis downloads.", "Resource snapshots force GC; differences alone do not establish boundedness or absence of leaks.", "Browser autoplay/hidden/leader/StrictMode and Python task/audio-node/cache-cap gates require browser/Python suites; NOT_RUN by this Go harness."}}
	rep.Notes = append(rep.Notes, "Percentiles use the latest at most 1,000 monotonic samples per phase; count includes all observations, retainedSamples discloses sample window.", "Worker restart is owner-runner/hub replacement within one process, not an OS-process/container kill; database/RPC/stream are real.", "Separate Telegram/native receiver scopes use separate configured HTTP service handlers at B; no store scope has two active authorities.")
	var err error
	switch {
	case flag.NArg() != 0:
		err = errors.New("positional arguments are not accepted")
	case opts.scenario != "smoke" && opts.scenario != "load" && opts.scenario != "soak":
		err = errors.New("scenario must be smoke, load, or soak")
	case math.IsNaN(opts.rate) || math.IsInf(opts.rate, 0) || opts.rate <= 0 || opts.rate > 1000:
		err = errors.New("rate must be >0 and <=1000")
	case opts.duration <= 0 || opts.delay < 0 || opts.burst < 0:
		err = errors.New("duration must be positive; start-delay and burst must be nonnegative")
	default:
		err = loopbackAddress(opts.listenA)
		if err == nil {
			err = loopbackAddress(opts.listenB)
		}
	}
	if err == nil && os.Getenv("TTS_BASE_URL") != "" {
		err = loopbackURL(os.Getenv("TTS_BASE_URL"))
	}
	if err == nil && os.Getenv("TTS_BASE_URL") == "" && os.Getenv("TTS_INTERNAL_TOKEN") != "" {
		err = errors.New("TTS_INTERNAL_TOKEN requires a local TTS_BASE_URL")
	}
	// Financial/runtime logs may carry references. Evidence contains only the
	// explicit aggregate allowlist above, never production identifiers/secrets.
	diagnosticLog := &fixtureDiagnosticLog{}
	slog.SetDefault(slog.New(slog.NewJSONHandler(diagnosticLog, &slog.HandlerOptions{Level: slog.LevelError})))
	if err == nil {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		err = execute(ctx, opts, rep)
		stop()
	}
	rep.StorageFailures = diagnosticLog.snapshot()
	rep.DurationSeconds = time.Since(started).Seconds()
	if err != nil {
		rep.Error = err.Error()
	} else {
		rep.Status = "PASS"
	}
	raw, marshalErr := json.MarshalIndent(rep, "", "  ")
	if marshalErr != nil {
		fmt.Fprintln(os.Stderr, "cannot encode evidence")
		os.Exit(1)
	}
	raw = append(raw, '\n')
	if opts.output == "" {
		_, _ = os.Stdout.Write(raw)
	} else if writeErr := os.WriteFile(opts.output, raw, 0600); writeErr != nil {
		fmt.Fprintln(os.Stderr, writeErr)
		os.Exit(1)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "fixture harness failed:", err)
		os.Exit(1)
	}
}
