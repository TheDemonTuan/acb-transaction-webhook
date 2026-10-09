package payments

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/config"
	"github.com/thedemontuan/acb-transaction-webhook/internal/security"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

func managedFixtureService(t *testing.T, store *storage.Store, server *httptest.Server) *Service {
	t.Helper()
	keyring, err := security.NewKeyring([]byte("01234567890123456789012345678901"))
	if err != nil {
		t.Fatal(err)
	}
	store.WithKeyring(keyring)
	svc := NewManagedService(config.Config{PayOSClientID: "ignored-env-client", PayOSAPIKey: "ignored-env-api", PayOSChecksumKey: "ignored-env-checksum", PaymentsEnabled: false, PaymentMaxAmountVND: 500000000, PaymentPublicOrigin: "https://transactions.example.test"}, store, nil)
	svc.providerFactory = func(keys storage.PaymentProviderCredentials) (Provider, error) {
		return NewPayOS(keys.ClientID, keys.APIKey, keys.ChecksumKey, WithBaseURL(server.URL), WithHTTPClient(server.Client()))
	}
	return svc
}
func managedFixtureKeys() storage.PaymentProviderCredentials {
	return storage.PaymentProviderCredentials{ClientID: "fixture-client", APIKey: "fixture-api-key", ChecksumKey: fixtureChecksumKey}
}
func saveManagedFixture(t *testing.T, svc *Service) {
	t.Helper()
	if _, err := svc.SaveProviderConfig(context.Background(), managedFixtureKeys(), true, storage.PaymentProviderActor{}); err != nil {
		t.Fatal(err)
	}
}

func TestManagedProviderReloadAndUnconfiguredBoot(t *testing.T) {
	ctx := context.Background()
	store := createTestStore(t)
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("x-client-id") != "fixture-client" || r.Header.Get("x-api-key") != "fixture-api-key" {
			t.Error("environment credentials reached provider")
		}
		if r.URL.Path == "/confirm-webhook" {
			var request map[string]string
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
			}
			writeSignedFixture(t, w, map[string]any{"webhookUrl": request["webhookUrl"]})
			return
		}
		writeSignedFixture(t, w, signedCreateData(decodeCreateRequest(t, r)))
	}))
	defer server.Close()
	gateway := managedFixtureService(t, store, server)
	otherStore, err := storage.Open(ctx, storePath(t, store))
	if err != nil {
		t.Fatal(err)
	}
	defer otherStore.Close()
	worker := managedFixtureService(t, otherStore, server)
	if got := gateway.Config(); got.Status != "UNCONFIGURED" || got.Ready {
		t.Fatalf("boot config: %+v", got)
	}
	if _, _, err := gateway.CreateOrder(ctx, 50000, "STATIC_URL", createKey); !errors.Is(err, ErrPaymentUnavailable) {
		t.Fatal("unconfigured service created order")
	}
	if calls.Load() != 0 {
		t.Fatal("unconfigured service contacted provider")
	}
	saveManagedFixture(t, gateway)
	if got := worker.Config(); got.Status != "WEBHOOK_UNCONFIRMED" {
		t.Fatalf("worker did not reload saved configuration: %+v", got)
	}
	if _, _, err := worker.CreateOrder(ctx, 50000, "STATIC_URL", createKey); !errors.Is(err, ErrWebhookUnconfirmed) {
		t.Fatalf("unconfirmed channel created order: %v", err)
	}
	if err := gateway.ConfirmWebhook(ctx); err != nil {
		t.Fatal(err)
	}
	if got := worker.Config(); !got.Ready {
		t.Fatalf("worker did not reload confirmation or env false locked enabled: %+v", got)
	}
	order, created, err := worker.CreateOrder(ctx, 50000, "STATIC_URL", createKey)
	if err != nil || !created || order.Status != "PENDING" {
		t.Fatalf("managed create: %+v %v %v", order, created, err)
	}
	restarted := managedFixtureService(t, otherStore, server)
	snapshot, err := restarted.Order(ctx, order.ID)
	if err != nil || snapshot.OrderCode != order.OrderCode {
		t.Fatal("restart did not resolve channel from shared DB")
	}
	body, err := json.Marshal(restarted.Config())
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"fixture-client", "fixture-api-key", fixtureChecksumKey, "ignored-env-client"} {
		if strings.Contains(string(body), secret) {
			t.Fatal("public configuration leaks credential")
		}
	}
}

func TestManagedConfirmCASAndFailure(t *testing.T) {
	ctx := context.Background()
	store := createTestStore(t)
	entered, release := make(chan struct{}, 1), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	var reject atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if reject.Load() {
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"code":"FAIL","desc":"fixture-api-key raw rejection"}`))
			return
		}
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
		writeSignedFixture(t, w, map[string]any{"webhookUrl": "https://transactions.example.test/api/integrations/payos/webhook"})
	}))
	defer server.Close()
	defer unblock()
	svc := managedFixtureService(t, store, server)
	saveManagedFixture(t, svc)
	finished := make(chan error, 1)
	go func() { finished <- svc.ConfirmWebhook(ctx) }()
	<-entered
	if _, err := svc.SaveProviderConfig(ctx, storage.PaymentProviderCredentials{ClientID: "fixture-client"}, false, storage.PaymentProviderActor{}); err != nil {
		t.Fatal(err)
	}
	unblock()
	err := <-finished
	var conflict *ServiceError
	if !errors.As(err, &conflict) || conflict.Code != "PROVIDER_CONFIG_CHANGED" {
		t.Fatalf("stale SDK result confirmed newer revision: %v", err)
	}
	saved, err := svc.ProviderConfig(ctx)
	if err != nil || saved.WebhookConfirmed {
		t.Fatal("stale confirmation persisted")
	}
	reject.Store(true)
	if err := svc.ConfirmWebhook(ctx); !errors.Is(err, ErrPaymentUnavailable) || strings.Contains(err.Error(), "fixture-api-key") {
		t.Fatalf("provider rejection not sanitized: %v", err)
	}
	reject.Store(false)
	if err := svc.ConfirmWebhook(ctx); err != nil {
		t.Fatal(err)
	}
	saved, err = svc.ProviderConfig(ctx)
	if err != nil || !saved.WebhookConfirmed || saved.Enabled {
		t.Fatal("confirmation not persisted independently of enabled")
	}
	var count int
	if err := store.DB().QueryRow(`SELECT count(*) FROM transactions`).Scan(&count); err != nil || count != 0 {
		t.Fatal("confirmation invented financial transaction")
	}
}

func TestManagedRotationPinsActiveNetworkAndRetainsDelayedSignatures(t *testing.T) {
	ctx := context.Background()
	store := createTestStore(t)
	entered, release := make(chan struct{}, 1), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := decodeCreateRequest(t, r)
		if r.Header.Get("x-api-key") != "fixture-api-key" {
			t.Error("active operation switched credentials")
		}
		entered <- struct{}{}
		<-release
		writeSignedFixture(t, w, signedCreateData(req))
	}))
	defer server.Close()
	defer unblock()
	svc := managedFixtureService(t, store, server)
	saveManagedFixture(t, svc)
	saved, err := store.PaymentProviderConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ConfirmPaymentProviderWebhook(ctx, saved.Revision, storage.PaymentProviderActor{}); err != nil {
		t.Fatal(err)
	}
	type createResult struct {
		order storage.PaymentOrder
		err   error
	}
	finished := make(chan createResult, 1)
	go func() {
		order, _, err := svc.CreateOrder(ctx, 50000, "STATIC_URL", createKey)
		finished <- createResult{order, err}
	}()
	<-entered
	rotated := managedFixtureKeys()
	rotated.ChecksumKey = "rotated-fixture-checksum"
	if _, err := svc.SaveProviderConfig(ctx, rotated, true, storage.PaymentProviderActor{}); err == nil || err.Error() != "PROVIDER_CONFIG_BUSY" {
		t.Fatalf("active operation allowed key rotation: %v", err)
	}
	if _, err := svc.SaveProviderConfig(ctx, storage.PaymentProviderCredentials{ClientID: "fixture-client"}, false, storage.PaymentProviderActor{}); err != nil {
		t.Fatal(err)
	}
	unblock()
	result := <-finished
	if result.err != nil || result.order.Status != "PENDING" {
		t.Fatalf("admitted operation lost after flags save: %v", result.err)
	}
	if _, err := svc.SaveProviderConfig(ctx, rotated, false, storage.PaymentProviderActor{}); err != nil {
		t.Fatal(err)
	}
	callback := webhookFixture()
	callback["orderCode"] = result.order.OrderCode
	if err := svc.HandleWebhook(ctx, signedWebhookBody(t, callback)); err != nil {
		t.Fatalf("delayed old-checksum callback rejected: %v", err)
	}
	paid, err := svc.Order(ctx, result.order.ID)
	if err != nil || paid.Status != "PAID" {
		t.Fatal("same-channel rotated credentials abandoned issued money")
	}
	if err := svc.HandleWebhook(ctx, signedWebhookBody(t, callback)); err != nil {
		t.Fatal(err)
	}
	var receipts int
	if err := store.DB().QueryRow(`SELECT count(*) FROM payment_receipts`).Scan(&receipts); err != nil || receipts != 1 {
		t.Fatal("rotation/replay duplicated settlement")
	}
	rotated.ClientID = "replacement-channel"
	if _, err := svc.SaveProviderConfig(ctx, rotated, false, storage.PaymentProviderActor{}); err == nil || err.Error() != "PROVIDER_CHANNEL_LOCKED" {
		t.Fatal("channel changed after issuance")
	}
}

func TestManagedQuiesceDrainsOtherProcessOperations(t *testing.T) {
	ctx := context.Background()
	store := createTestStore(t)
	entered, release := make(chan struct{}, 1), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := decodeCreateRequest(t, r)
		entered <- struct{}{}
		<-release
		writeSignedFixture(t, w, signedCreateData(req))
	}))
	defer server.Close()
	defer unblock()
	gateway := managedFixtureService(t, store, server)
	saveManagedFixture(t, gateway)
	saved, err := store.PaymentProviderConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ConfirmPaymentProviderWebhook(ctx, saved.Revision, storage.PaymentProviderActor{}); err != nil {
		t.Fatal(err)
	}
	other, err := storage.Open(ctx, storePath(t, store))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	worker := managedFixtureService(t, other, server)
	created := make(chan error, 1)
	go func() { _, _, err := gateway.CreateOrder(ctx, 50000, "STATIC_URL", createKey); created <- err }()
	<-entered
	if worker.ActiveRequests() != 1 {
		t.Fatal("worker cannot see gateway network request")
	}
	drained := make(chan error, 1)
	go func() { drained <- worker.Quiesce(ctx) }()
	select {
	case err := <-drained:
		t.Fatalf("quiesce returned before active request ended: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	unblock()
	if err := <-created; err != nil {
		t.Fatal(err)
	}
	if err := <-drained; err != nil {
		t.Fatal(err)
	}
	if _, _, err := gateway.CreateOrder(ctx, 50000, "STATIC_URL", "00000000-0000-4000-8000-000000000009"); err == nil {
		t.Fatal("quiesced worker admitted gateway create")
	}
	worker.Resume()
	if got := gateway.Config(); !got.Ready {
		t.Fatalf("shared resume did not recover config: %+v", got)
	}
}

func TestManagedDurableSnapshotsDoNotDependOnCredentials(t *testing.T) {
	ctx := context.Background()
	store := createTestStore(t)
	order := reserveReconcileOrder(t, store, "PENDING", true)
	service := NewManagedService(config.Config{}, store, nil)
	service.providerFactory = func(storage.PaymentProviderCredentials) (Provider, error) {
		t.Fatal("read-only snapshot constructed a provider")
		return nil, ErrPaymentUnavailable
	}
	for _, configured := range []bool{false, true} {
		if configured {
			keyring, err := security.NewKeyring([]byte("01234567890123456789012345678901"))
			if err != nil {
				t.Fatal(err)
			}
			store.WithKeyring(keyring)
			if _, err := store.SavePaymentProviderConfig(ctx, managedFixtureKeys(), true, storage.PaymentProviderActor{}); err != nil {
				t.Fatal(err)
			}
			store.WithKeyring(nil)
		}
		got, err := service.Order(ctx, order.ID)
		if err != nil || got.ID != order.ID || got.Status != order.Status {
			t.Fatalf("durable snapshot unavailable: %v", err)
		}
		page, err := service.Orders(ctx, "PENDING", "", 20)
		if err != nil || len(page.Items) != 1 || page.Items[0].ID != order.ID {
			t.Fatalf("durable list unavailable: %v", err)
		}
	}
}
