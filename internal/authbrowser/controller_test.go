package authbrowser

import (
	"context"
	"testing"
	"time"
)

func TestControllerStartsOneBrowserAndCancels(t *testing.T) {
	started := ""
	stopped := ""
	controller := New(func(_ context.Context, id string) error { started = id; return nil }, func(id string) error { stopped = id; return nil }, time.Minute)
	controller.now = func() time.Time { return time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC) }

	session, err := controller.Start(context.Background(), "auth_one")
	if err != nil || started != "auth_one" {
		t.Fatalf("start failed: session=%+v err=%v", session, err)
	}
	if _, err := controller.Start(context.Background(), "auth_two"); err == nil {
		t.Fatal("allowed a second browser session")
	}
	if err := controller.Cancel("auth_one"); err != nil || stopped != "auth_one" {
		t.Fatalf("cancel failed: %v", err)
	}
}
