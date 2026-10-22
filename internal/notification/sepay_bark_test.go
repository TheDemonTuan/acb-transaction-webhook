package notification_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/bark"
	"github.com/thedemontuan/acb-transaction-webhook/internal/notification"
	"github.com/thedemontuan/acb-transaction-webhook/internal/security"
	"github.com/thedemontuan/acb-transaction-webhook/internal/sepay"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
	"github.com/thedemontuan/acb-transaction-webhook/internal/webhook"
)

type privateBoundarySender struct {
	t      *testing.T
	sender notification.Sender
}

func (s privateBoundarySender) Send(ctx context.Context, req notification.SendRequest) notification.SendResult {
	s.t.Helper()
	if req.SePayDetails != nil {
		s.t.Fatal("WEBHOOK received private details")
	}
	return s.sender.Send(ctx, req)
}

func TestSePayBarkPrivateSnapshotDispatch(t *testing.T) {
	for _, scenario := range []string{"enriched", "disabled", "corrupt", "legacy"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			keyPath := filepath.Join(dir, "master.key")
			if err := os.WriteFile(keyPath, []byte(strings.Repeat("01", 32)), 0600); err != nil {
				t.Fatal(err)
			}
			keyring, err := security.LoadKeyring(keyPath)
			if err != nil {
				t.Fatal(err)
			}
			store, err := storage.Open(ctx, filepath.Join(dir, "gateway.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			store.WithKeyring(keyring)
			var pushes, hooks [][]byte
			capture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				payload, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
					return
				}
				if r.URL.Path == "/push" {
					pushes = append(pushes, payload)
					io.WriteString(w, `{"code":200,"message":"success"}`)
				} else {
					hooks = append(hooks, payload)
					w.WriteHeader(200)
				}
			}))
			defer capture.Close()
			cfg := storage.DefaultBarkConfig()
			cfg.IncludeBalance, cfg.IncludeDescription = scenario != "disabled", scenario != "disabled"
			channel, err := store.CreateBarkChannel(ctx, "fixture iPhone", "fixture-device-key", &cfg)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.SetEndpointStatus(ctx, channel.ID, "ACTIVE"); err != nil {
				t.Fatal(err)
			}
			endpoint, err := store.CreateEndpointWithSecret(ctx, "fixture webhook", capture.URL+"/webhook")
			if err != nil {
				t.Fatal(err)
			}
			if err := store.SetEndpointStatus(ctx, endpoint.ID, "ACTIVE"); err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC().Truncate(time.Second)
			serviceCfg := sepay.Config{Mode: sepay.ModeActive, NativeMode: sepay.ModeActive, ActiveTransport: "native", NativeWebhookSecret: strings.Repeat("s", 32), StoreKey: "private-store", BankCode: "VCB", BankName: "Fixture Bank", AccountNumber: "TEST123", ActivationAt: now.Add(-time.Hour)}
			var event storage.EventNotification
			service := sepay.NewService(serviceCfg, store, func(e storage.EventNotification) { event = e })
			balance := int64(2350000)
			hook := sepay.NativeWebhook{ID: 123, TransferAmount: 150000, BalanceVND: &balance, Gateway: serviceCfg.BankName, AccountNumber: serviceCfg.AccountNumber, TransferType: "in", TransactionAt: now, Content: "PRIVATE CUSTOMER MEMO", RawPayload: []byte(`{"fixture":true}`)}
			if err := service.HandleNativeWebhook(ctx, hook); err != nil {
				t.Fatal(err)
			}
			if event.EventID == "" {
				t.Fatal("missing event")
			}
			hook.BalanceVND, hook.Content = nil, "REPLAY REPLACEMENT"
			if err := service.HandleNativeWebhook(ctx, hook); err != nil {
				t.Fatal(err)
			}
			if scenario == "corrupt" || scenario == "disabled" {
				if _, err := store.DB().Exec(`UPDATE sepay_notification_details SET details_envelope=?`, []byte("PRIVATE CUSTOMER MEMO 2350000")); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "legacy" {
				if _, err := store.DB().Exec(`DELETE FROM sepay_notification_details`); err != nil {
					t.Fatal(err)
				}
			}
			registry := notification.NewRegistry()
			registry.Register(notification.ProviderBark, bark.NewSender(bark.Config{ServerURL: capture.URL, Timeout: time.Second}, capture.Client(), ""))
			registry.Register(notification.ProviderWebhook, privateBoundarySender{t, webhook.NewSender(capture.Client(), true)})
			dispatcher := notification.NewDispatcher(store, registry)
			for range 2 {
				if processed, err := dispatcher.DispatchOne(ctx); err != nil || !processed {
					t.Fatalf("dispatch=%v %v", processed, err)
				}
			}
			if len(hooks) != 1 {
				t.Fatalf("webhook count=%d", len(hooks))
			}
			for _, payload := range [][]byte{hooks[0], event.Payload} {
				for _, secret := range []string{"PRIVATE CUSTOMER MEMO", "2350000", "REPLAY REPLACEMENT"} {
					if bytes.Contains(payload, []byte(secret)) {
						t.Fatal("shared payload leaked private details")
					}
				}
			}
			var publicDescription, journal string
			var balanceProjection any
			if err := store.DB().QueryRow(`SELECT description_envelope,balance FROM transactions`).Scan(&publicDescription, &balanceProjection); err != nil {
				t.Fatal(err)
			}
			if err := store.DB().QueryRow(`SELECT payload_json FROM event_journal WHERE event_type='bank.transaction.credit'`).Scan(&journal); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(publicDescription+journal, "PRIVATE CUSTOMER MEMO") || strings.Contains(journal, "2350000") || balanceProjection != nil {
				t.Fatal("public projection or journal leaked private details")
			}
			wantPushes := 1
			if scenario == "corrupt" {
				wantPushes = 0
			}
			if len(pushes) != wantPushes {
				t.Fatalf("pushes=%d want=%d", len(pushes), wantPushes)
			}
			if scenario == "enriched" {
				var push map[string]any
				if err := json.Unmarshal(pushes[0], &push); err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(string(pushes[0]), "PRIVATE CUSTOMER MEMO") || !strings.Contains(string(pushes[0]), "2.350.000") {
					t.Fatalf("private snapshot missing: %s", pushes[0])
				}
			}
			if scenario == "disabled" || scenario == "legacy" {
				if bytes.Contains(pushes[0], []byte("PRIVATE CUSTOMER MEMO")) || bytes.Contains(pushes[0], []byte("2.350.000")) || bytes.Contains(pushes[0], []byte("Thanh toán QR cửa hàng")) {
					t.Fatal("missing/disabled snapshot fell back to public description")
				}
			}
			if scenario == "corrupt" {
				var outcome, sanitized, code, status string
				if err := store.DB().QueryRow(`SELECT a.outcome,a.sanitized_error,a.provider_error_code,d.status FROM delivery_attempts a JOIN deliveries d ON d.id=a.delivery_id WHERE a.provider='BARK'`).Scan(&outcome, &sanitized, &code, &status); err != nil {
					t.Fatal(err)
				}
				if outcome != "RETRY" || code != "SEPAY_NOTIFICATION_DETAILS_UNAVAILABLE" || sanitized != "SePay notification details unavailable" || status != "PENDING" {
					t.Fatalf("retry path: %s %s %s %s", outcome, sanitized, code, status)
				}
			}
			for _, table := range []string{"transactions", "events", "sepay_native_inbox"} {
				var count int
				if err := store.DB().QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil || count != 1 {
					t.Fatalf("replay count %s=%d %v", table, count, err)
				}
			}
		})
	}
}
