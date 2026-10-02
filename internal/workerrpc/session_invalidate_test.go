package workerrpc_test

import (
	"context"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
	"github.com/thedemontuan/acb-transaction-webhook/internal/workerrpc"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fencedInvalidationHandler struct {
	mockWorkerHandler
	store   *storage.Store
	cleared bool
}

func (h *fencedInvalidationHandler) InvalidateSession(ctx context.Context, id string, generation int64) error {
	if err := h.store.CheckACBLogoutFence(ctx, id, generation); err != nil {
		return err
	}
	h.cleared = true
	return nil
}
func TestSessionInvalidationRPCRequiresAuthAndCurrentLogoutFence(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	c, err := store.ConfigureConnection(ctx, "***1234")
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.TelegramAuthState(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	a, err := store.CreateTelegramAuthAction(ctx, storage.TelegramAuthAction{BotID: 42, ChatID: 123, UserID: 456, ExpectedGeneration: c.Generation, Action: "LOGOUT"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DeliverTelegramAuthAction(ctx, a.ID, 100); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ConsumeTelegramAuthAction(ctx, a.ID, 42, 123, 456, 100, time.Now()); err != nil {
		t.Fatal(err)
	}
	fenced, err := store.Connection(ctx)
	if err != nil {
		t.Fatal(err)
	}
	h := &fencedInvalidationHandler{store: store}
	server, err := workerrpc.NewServer(h, "synthetic-internal")
	if err != nil {
		t.Fatal(err)
	}
	api := httptest.NewServer(server.Handler())
	defer api.Close()
	bad := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/rpc/session/invalidate", strings.NewReader(`{"connectionId":"private","generation":1}`))
	server.Handler().ServeHTTP(bad, req)
	if bad.Code != 401 || h.cleared {
		t.Fatalf("unauthorized invalidation status=%d cleared=%t", bad.Code, h.cleared)
	}
	client := workerrpc.NewClient(api.URL, "synthetic-internal")
	if err := client.InvalidateSession(ctx, c.ID, c.Generation); err == nil || h.cleared {
		t.Fatal("stale fence cleared session")
	}
	if err := client.InvalidateSession(ctx, c.ID, fenced.Generation); err != nil {
		t.Fatal(err)
	}
	if !h.cleared {
		t.Fatal("valid current logout fence not acknowledged")
	}
	if err := client.InvalidateSession(ctx, c.ID, fenced.Generation); err != nil {
		t.Fatal("idempotent same-fence clear:", err)
	}
}
