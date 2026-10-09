package payments

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	payos "github.com/payOSHQ/payos-lib-golang/v2"
	"github.com/thedemontuan/acb-transaction-webhook/internal/config"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

const createKey = "3e628a46-7db9-4f7b-a95f-d9763e2af3de"

func createTestStore(t *testing.T) *storage.Store {
	t.Helper()
	store, err := storage.Open(context.Background(), filepath.Join(t.TempDir(), "payments.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func createTestService(t *testing.T, store *storage.Store, server *httptest.Server) *Service {
	t.Helper()
	return NewService(config.Config{
		PayOSClientID: "fixture-client", PayOSAPIKey: "fixture-api-key", PayOSChecksumKey: fixtureChecksumKey,
		PaymentsEnabled: true, PayOSWebhookConfirmed: true, PaymentMaxAmountVND: 500000000,
		PaymentPublicOrigin: "https://transactions.example.test",
	}, store, fixtureAdapter(t, server), nil)
}

func decodeCreateRequest(t *testing.T, r *http.Request) payos.CreatePaymentLinkRequest {
	t.Helper()
	if r.Method != http.MethodPost || r.URL.Path != "/v2/payment-requests" {
		t.Errorf("unexpected provider method/path: %s %s", r.Method, r.URL.Path)
	}
	var req payos.CreatePaymentLinkRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		t.Error(err)
	}
	return req
}

func signedCreateData(req payos.CreatePaymentLinkRequest) map[string]any {
	data := createFixture()
	data["orderCode"], data["amount"], data["description"] = req.OrderCode, req.Amount, req.Description
	return data
}

func TestCreateOrderConcurrentReplayAndConflict(t *testing.T) {
	store := createTestStore(t)
	entered, release := make(chan payos.CreatePaymentLinkRequest, 1), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		req := decodeCreateRequest(t, r)
		persisted, err := store.PaymentOrderByCode(r.Context(), req.OrderCode)
		if err != nil {
			t.Error(err)
		} else {
			expiry, _ := time.Parse(time.RFC3339Nano, persisted.ExpiresAt)
			lease, _ := time.Parse(time.RFC3339Nano, persisted.OperationLeaseUntil)
			next, _ := time.Parse(time.RFC3339Nano, persisted.NextReconcileAt)
			if persisted.Status != "CREATING" || persisted.OperationToken == "" || !lease.After(time.Now()) || next.Before(lease) {
				t.Errorf("network started before durable leased intent: %+v", persisted)
			}
			if req.ExpiredAt == nil || int64(*req.ExpiredAt) != expiry.Unix() || req.ReturnUrl != "https://transactions.example.test/pay/"+persisted.ID || req.CancelUrl != req.ReturnUrl || req.Description != persisted.Description {
				t.Errorf("request does not reuse durable fields: %+v", req)
			}
		}
		entered <- req
		<-release
		writeSignedFixture(t, w, signedCreateData(req))
	}))
	defer server.Close()
	defer unblock()
	svc := createTestService(t, store, server)
	// Separate service and Store emulate two gateways sharing SQLite.
	otherStore, err := storage.Open(context.Background(), storePath(t, store))
	if err != nil {
		t.Fatal(err)
	}
	defer otherStore.Close()
	otherSvc := createTestService(t, otherStore, server)
	type result struct {
		order   storage.PaymentOrder
		created bool
		err     error
	}
	first := make(chan result, 1)
	go func() {
		order, created, err := svc.CreateOrder(context.Background(), 50000, "STATIC_URL", createKey)
		first <- result{order: order, created: created, err: err}
	}()
	req := <-entered
	var wg sync.WaitGroup
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			order, created, err := otherSvc.CreateOrder(context.Background(), 50000, "STATIC_URL", createKey)
			if err != nil || created || order.OrderCode != req.OrderCode || order.Status != "CREATING" {
				t.Errorf("concurrent replay: order=%+v created=%v err=%v", order, created, err)
			}
		}()
	}
	wg.Wait()
	for _, input := range []struct {
		amount int64
		origin string
	}{{50001, "STATIC_URL"}, {50000, "OPERATOR_DYNAMIC"}} {
		if _, _, err := otherSvc.CreateOrder(context.Background(), input.amount, input.origin, createKey); !errors.Is(err, ErrIdempotencyConflict) {
			t.Errorf("different intent did not conflict: %v", err)
		}
	}
	unblock()
	got := <-first
	if got.err != nil || !got.created || got.order.Status != "PENDING" || got.order.QRCode != "original-provider-qr" || got.order.OperationToken != "" || got.order.AccountNumber != "VA-ORDER-1" {
		t.Fatalf("new result: %+v err=%v", got, got.err)
	}
	replay, created, err := svc.CreateOrder(context.Background(), 50000, "STATIC_URL", createKey)
	if err != nil || created || replay.ID != got.order.ID || replay.OrderCode != req.OrderCode || calls.Load() != 1 {
		t.Fatalf("replay created another provider intent: %+v %v %v calls=%d", replay, created, err, calls.Load())
	}
	var orders int
	if err := store.DB().QueryRow("SELECT count(*) FROM payment_orders").Scan(&orders); err != nil || orders != 1 {
		t.Fatalf("order count=%d err=%v", orders, err)
	}
}

func storePath(t *testing.T, store *storage.Store) string {
	t.Helper()
	var seq int
	var name, path string
	if err := store.DB().QueryRow("PRAGMA database_list").Scan(&seq, &name, &path); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCreateOrderTimeoutKeepsSameDurableIntent(t *testing.T) {
	store := createTestStore(t)
	entered := make(chan payos.CreatePaymentLinkRequest, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := decodeCreateRequest(t, r)
		entered <- req
		<-r.Context().Done() // Provider may have accepted the intent before disconnect.
	}))
	defer server.Close()
	svc := createTestService(t, store, server)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type result struct {
		order   storage.PaymentOrder
		created bool
		err     error
	}
	finished := make(chan result, 1)
	go func() {
		order, created, err := svc.CreateOrder(ctx, 50000, "STATIC_URL", createKey)
		finished <- result{order, created, err}
	}()
	req := <-entered
	cancel()
	got := <-finished
	if got.err != nil || !got.created || got.order.Status != "CREATING" || got.order.LastErrorCode != "CREATE_OUTCOME_UNKNOWN" || got.order.OrderCode != req.OrderCode || got.order.QRCode != "" {
		t.Fatalf("uncertain outcome discarded intent: %+v err=%v", got, got.err)
	}
	lease, _ := time.Parse(time.RFC3339Nano, got.order.CreatedAt)
	next, _ := time.Parse(time.RFC3339Nano, got.order.NextReconcileAt)
	if next.Before(lease.Add(29*time.Second)) || got.order.OperationToken != "" {
		t.Fatalf("uncertainty is not durably scheduled beyond original lease: %+v", got.order)
	}
	// Even after the lease is gone/restart, POST replay is not a recovery Create.
	restarted := createTestService(t, store, server)
	replay, created, err := restarted.CreateOrder(context.Background(), 50000, "STATIC_URL", createKey)
	if err != nil || created || replay.ID != got.order.ID || replay.OrderCode != req.OrderCode {
		t.Fatalf("timeout replay: %+v %v %v", replay, created, err)
	}
}

func TestCreateOrderValidatesSignedMetadata(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"wrong order", func(d map[string]any) { d["orderCode"] = int64(123) }},
		{"wrong amount", func(d map[string]any) { d["amount"] = 1 }},
		{"wrong currency", func(d map[string]any) { d["currency"] = "USD" }},
		{"missing link", func(d map[string]any) { d["paymentLinkId"] = "" }},
		{"missing QR", func(d map[string]any) { d["qrCode"] = " \n" }},
		{"missing account", func(d map[string]any) { d["accountNumber"] = "" }},
		{"missing account name", func(d map[string]any) { d["accountName"] = "" }},
		{"missing bank", func(d map[string]any) { d["bin"] = "" }},
		{"non pending", func(d map[string]any) { d["status"] = "PAID" }},
		{"HTTP checkout", func(d map[string]any) { d["checkoutUrl"] = "http://pay.payos.vn/web/link" }},
		{"other checkout", func(d map[string]any) { d["checkoutUrl"] = "https://pay.payos.vn.evil.test/web/link" }},
		{"userinfo checkout", func(d map[string]any) { d["checkoutUrl"] = "https://user@pay.payos.vn/web/link" }},
		{"non TLS port", func(d map[string]any) { d["checkoutUrl"] = "https://pay.payos.vn:8443/web/link" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := createTestStore(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				data := signedCreateData(decodeCreateRequest(t, r))
				tc.mutate(data)
				writeSignedFixture(t, w, data)
			}))
			defer server.Close()
			order, created, err := createTestService(t, store, server).CreateOrder(context.Background(), 50000, "OPERATOR_DYNAMIC", createKey)
			if err != nil || !created || order.Status != "CREATING" || order.LastErrorCode != "INVALID_CREATE_RESPONSE" || order.QRCode != "" || order.CheckoutURL != "" || order.PaymentLinkID != "" {
				t.Fatalf("invalid metadata escaped quarantine: %+v %v %v", order, created, err)
			}
		})
	}
}

func TestCreateOrderProviderRejectionStillNeedsEvidence(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusOK, http.StatusTooManyRequests} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			store := createTestStore(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				decodeCreateRequest(t, r)
				w.Header().Set("Retry-After", "60")
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"code":"unverified-code","desc":"SECRET must not escape","data":null}`))
			}))
			defer server.Close()
			// Bound the SDK's own retry/backoff for the throttling case.
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			order, created, err := createTestService(t, store, server).CreateOrder(ctx, 50000, "STATIC_URL", createKey)
			if err != nil || !created || order.Status != "CREATING" || strings.Contains(order.LastErrorCode, "SECRET") {
				t.Fatalf("unproven rejection became terminal: %+v %v %v", order, created, err)
			}
			if status == http.StatusBadRequest && order.LastErrorCode != "CREATE_REJECTED_HTTP_400" {
				t.Fatalf("definitive HTTP metadata lost: %+v", order)
			}
			if status == http.StatusOK && order.LastErrorCode != "CREATE_OUTCOME_UNKNOWN" {
				t.Fatalf("unknown provider code guessed absence: %+v", order)
			}
			if status == http.StatusTooManyRequests {
				next, _ := time.Parse(time.RFC3339Nano, order.NextReconcileAt)
				if next.Before(time.Now().Add(55 * time.Second)) {
					t.Fatalf("Retry-After lower bound not persisted: %+v", order)
				}
			}
		})
	}
}

func TestCreateOrderLateResponseCannotOverwritePaid(t *testing.T) {
	store := createTestStore(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := decodeCreateRequest(t, r)
		// Emulate settlement's atomic status change while network I/O is active.
		if _, err := store.DB().ExecContext(r.Context(), "UPDATE payment_orders SET status='PAID', paid_at=? WHERE order_code=?", time.Now().UTC().Format(time.RFC3339Nano), req.OrderCode); err != nil {
			t.Error(err)
		}
		writeSignedFixture(t, w, signedCreateData(req))
	}))
	defer server.Close()
	order, created, err := createTestService(t, store, server).CreateOrder(context.Background(), 50000, "STATIC_URL", createKey)
	if err != nil || !created || order.Status != "PAID" || order.PaidAt == "" || order.AccountNumber != "VA-ORDER-1" || order.QRCode != "original-provider-qr" {
		t.Fatalf("late Create overwrote settlement or dropped metadata: %+v %v %v", order, created, err)
	}
}

func TestCreateOrderInputAndNewOnlyGates(t *testing.T) {
	store := createTestStore(t)
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		writeSignedFixture(t, w, signedCreateData(decodeCreateRequest(t, r)))
	}))
	defer server.Close()
	svc := createTestService(t, store, server)
	for _, amount := range []int64{-1, 0, 500000001, maxSafeInteger + 1} {
		if _, _, err := svc.CreateOrder(context.Background(), amount, "STATIC_URL", createKey); !errors.Is(err, ErrInvalidAmount) {
			t.Errorf("amount %d accepted: %v", amount, err)
		}
	}
	safeCfg := svc.cfg
	safeCfg.PaymentMaxAmountVND = maxSafeInteger + 1
	if _, _, err := NewService(safeCfg, store, fixtureAdapter(t, server), nil).CreateOrder(context.Background(), maxSafeInteger+1, "STATIC_URL", createKey); !errors.Is(err, ErrInvalidAmount) {
		t.Fatalf("configuration bypassed safe integer amount cap: %v", err)
	}
	for _, key := range []string{"", "not-a-uuid", strings.ToUpper(createKey), "00000000-0000-0000-0000-000000000000", "3e628a467db94f7ba95fd9763e2af3de"} {
		if _, _, err := svc.CreateOrder(context.Background(), 1, "STATIC_URL", key); !errors.Is(err, ErrInvalidIdempotencyKey) {
			t.Errorf("invalid UUID accepted: %q %v", key, err)
		}
	}
	if _, _, err := svc.CreateOrder(context.Background(), 1, "static_url", createKey); !errors.Is(err, ErrInvalidOrigin) {
		t.Fatalf("unallowlisted origin accepted: %v", err)
	}
	order, _, err := svc.CreateOrder(context.Background(), 1, "STATIC_URL", createKey)
	if err != nil {
		t.Fatal(err)
	}
	for _, gate := range []string{"disabled", "unconfirmed", "deployment"} {
		t.Run(gate, func(t *testing.T) {
			cfg := svc.cfg
			switch gate {
			case "disabled":
				cfg.PaymentsEnabled = false
			case "unconfirmed":
				cfg.PayOSWebhookConfirmed = false
			case "deployment":
				if _, err := store.AcquireMutationGate(context.Background(), "test", time.Minute, "test"); err != nil {
					t.Fatal(err)
				}
			}
			gated := NewService(cfg, store, fixtureAdapter(t, server), nil)
			replay, created, err := gated.CreateOrder(context.Background(), 1, "STATIC_URL", createKey)
			if err != nil || created || replay.ID != order.ID {
				t.Fatalf("gate blocked existing retry: %+v %v %v", replay, created, err)
			}
			if _, _, err := gated.CreateOrder(context.Background(), 1, "STATIC_URL", "572dbf77-801e-4a5e-8073-47d5e6c02ddf"); err == nil {
				t.Fatal("gate admitted new intent")
			}
		})
	}
	if calls.Load() != 1 {
		t.Fatalf("invalid input/gates invoked provider: %d", calls.Load())
	}
}

func TestCreateOrderRacingInitialRequests(t *testing.T) {
	store := createTestStore(t)
	otherStore, err := storage.Open(context.Background(), storePath(t, store))
	if err != nil {
		t.Fatal(err)
	}
	defer otherStore.Close()
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		writeSignedFixture(t, w, signedCreateData(decodeCreateRequest(t, r)))
	}))
	defer server.Close()
	services := []*Service{createTestService(t, store, server), createTestService(t, otherStore, server)}
	type result struct {
		order   storage.PaymentOrder
		created bool
		err     error
	}
	results := make(chan result, 20)
	start := make(chan struct{})
	for i := range 20 {
		go func(svc *Service) {
			<-start
			order, created, err := svc.CreateOrder(context.Background(), 50000, "STATIC_URL", createKey)
			results <- result{order, created, err}
		}(services[i%2])
	}
	close(start)
	var id string
	var code int64
	createdCount := 0
	for range 20 {
		got := <-results
		if got.err != nil {
			t.Error(got.err)
			continue
		}
		if id == "" {
			id, code = got.order.ID, got.order.OrderCode
		}
		if got.order.ID != id || got.order.OrderCode != code {
			t.Errorf("racing request got different intent: %+v", got.order)
		}
		if got.created {
			createdCount++
		}
	}
	if createdCount != 1 || calls.Load() != 1 {
		t.Fatalf("racing Create duplicated intent: new=%d providerCalls=%d", createdCount, calls.Load())
	}
}

func TestCreateOrderInvalidSignatureDoesNotPersistProviderData(t *testing.T) {
	store := createTestStore(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data := signedCreateData(decodeCreateRequest(t, r))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"code": "00", "desc": "success", "data": data, "signature": strings.Repeat("0", 64)})
	}))
	defer server.Close()
	order, _, err := createTestService(t, store, server).CreateOrder(context.Background(), 50000, "STATIC_URL", createKey)
	if err != nil || order.Status != "CREATING" || order.LastErrorCode != "CREATE_OUTCOME_UNKNOWN" || order.QRCode != "" || order.AccountNumber != "" || order.PaymentLinkID != "" {
		t.Fatalf("unverified SDK response bound to intent: %+v %v", order, err)
	}
}

func TestCreateOrderProviderSucceededBeforePersistenceFault(t *testing.T) {
	store := createTestStore(t)
	path := storePath(t, store)
	var calls atomic.Int64
	var reqCode atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		req := decodeCreateRequest(t, r)
		reqCode.Store(req.OrderCode)
		if _, err := store.DB().ExecContext(r.Context(), `CREATE TRIGGER fail_create_save BEFORE UPDATE ON payment_orders BEGIN SELECT RAISE(ABORT,'fixture write fault'); END`); err != nil {
			t.Error(err)
		}
		writeSignedFixture(t, w, signedCreateData(req))
	}))
	defer server.Close()
	order, created, err := createTestService(t, store, server).CreateOrder(context.Background(), 50000, "STATIC_URL", createKey)
	if !errors.Is(err, ErrPaymentUnavailable) || !created || order.Status != "CREATING" || order.OrderCode != reqCode.Load() {
		t.Fatalf("save fault lost durable intent: %+v %v %v", order, created, err)
	}
	if _, err := store.DB().Exec("DROP TRIGGER fail_create_save"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := storage.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	replay, created, err := createTestService(t, reopened, server).CreateOrder(context.Background(), 50000, "STATIC_URL", createKey)
	if err != nil || created || replay.ID != order.ID || replay.OrderCode != order.OrderCode || replay.Status != "CREATING" || replay.OperationToken == "" || replay.NextReconcileAt == "" || calls.Load() != 1 {
		t.Fatalf("restart after provider success retried Create: %+v %v %v calls=%d", replay, created, err, calls.Load())
	}
}

func TestCreateOrderRetainsOriginalQRPayload(t *testing.T) {
	store := createTestStore(t)
	const original = "  provider-QR+payload/unchanged\n"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data := signedCreateData(decodeCreateRequest(t, r))
		data["qrCode"] = original
		writeSignedFixture(t, w, data)
	}))
	defer server.Close()
	order, _, err := createTestService(t, store, server).CreateOrder(context.Background(), 50000, "STATIC_URL", createKey)
	if err != nil || order.Status != "PENDING" || order.QRCode != original {
		t.Fatalf("provider QR was reconstructed or normalized: %q %v", order.QRCode, err)
	}
}
