package main

import (
	"context"
	"errors"
	"github.com/thedemontuan/acb-transaction-webhook/internal/acb"
	"github.com/thedemontuan/acb-transaction-webhook/internal/authbrowser"
	"github.com/thedemontuan/acb-transaction-webhook/internal/monitor"
	"github.com/thedemontuan/acb-transaction-webhook/internal/security"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
	"path/filepath"
	"testing"
	"time"
)

func TestWorkerInvalidationClearsMonitorAndVerifierWithoutRestoringStaleSession(t *testing.T) {
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
	kr, err := security.NewKeyring(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	bank, err := acb.NewClient("https://online.acb.com.vn", nil)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := acb.NewClient("https://online.acb.com.vn", nil)
	if err != nil {
		t.Fatal(err)
	}
	handoff := authbrowser.Handoff{URL: "https://online.acb.com.vn/acbib/Request", Action: "https://online.acb.com.vn/acbib/Request", Fields: map[string]string{"dse_sessionId": "synthetic", "dse_processorState": "page1", "AccountNbr": "0012345678"}, Cookies: []authbrowser.Cookie{{Name: "session", Value: "synthetic", Domain: "online.acb.com.vn", Path: "/", Secure: true}}}
	for _, client := range []*acb.Client{bank, verifier} {
		if err := client.RestoreSession(handoff); err != nil {
			t.Fatal(err)
		}
	}
	loader := monitor.NewSessionLoader(store, kr, bank)
	vloader := monitor.NewSessionLoader(store, kr, verifier)
	m := monitor.New(store, bank, time.Minute, time.Minute).WithSessionLoader(loader)
	service := &workerService{store: store, bankMonitor: m, verifierSessionLoader: vloader, verifierClient: verifier}
	action, err := store.CreateTelegramAuthAction(ctx, storage.TelegramAuthAction{BotID: 42, ChatID: 123, UserID: 456, ExpectedGeneration: c.Generation, Action: "LOGOUT"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DeliverTelegramAuthAction(ctx, action.ID, 100); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ConsumeTelegramAuthAction(ctx, action.ID, 42, 123, 456, 100, time.Now()); err != nil {
		t.Fatal(err)
	}
	fenced, err := store.Connection(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.InvalidateSession(ctx, c.ID, c.Generation); err == nil {
		t.Fatal("stale invalidate accepted")
	}
	if err := service.InvalidateSession(ctx, c.ID, fenced.Generation); err != nil {
		t.Fatal(err)
	}
	for _, client := range []*acb.Client{bank, verifier} {
		if _, err := client.SnapshotSession(); err == nil {
			t.Fatal("cookie/bootstrap resurrected after clear")
		}
	}
	if err := loader.Restore(ctx, c.ID, c.Generation); !errors.Is(err, storage.ErrGenerationFenceMismatch) {
		t.Fatalf("stale runtime restore: %v", err)
	}
	if err := service.InvalidateSession(ctx, c.ID, fenced.Generation); err != nil {
		t.Fatal("same fence not idempotent:", err)
	}
}
