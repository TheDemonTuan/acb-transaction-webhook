package storage

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"database/sql"
)

func monitoringStore(t *testing.T) (*Store, context.Context) {
	t.Helper()
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ConfigureConnection(ctx, "***1234"); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, `UPDATE connections SET state='MONITORING'`); err != nil {
		store.Close()
		t.Fatal(err)
	}
	return store, ctx
}

func TestLastSuccessfulPoll(t *testing.T) {
	store, ctx := monitoringStore(t)
	defer store.Close()

	connection, err := store.Connection(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LastSuccessfulPoll(ctx, connection.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("expected no successful poll, got %v", err)
	}

	failed, err := store.StartPoll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	failed.Status = "FAILED"
	failed.Error = "upstream error"
	if err := store.FinishPoll(ctx, failed); err != nil {
		t.Fatal(err)
	}

	succeeded, err := store.StartPoll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	succeeded.Status = "SUCCEEDED"
	succeeded.Classifier = "HISTORY_PAGE"
	succeeded.HTTPStatus = 200
	succeeded.Pages = 1
	succeeded.RowsSeen = 3
	if err := store.FinishPoll(ctx, succeeded); err != nil {
		t.Fatal(err)
	}

	last, err := store.LastSuccessfulPoll(ctx, connection.ID)
	if err != nil {
		t.Fatal(err)
	}
	if last.ID != succeeded.ID || last.RowsSeen != 3 || last.FinishedAt == "" {
		t.Fatalf("unexpected last successful poll: %#v", last)
	}
}

func TestPollRunFencesStaleConnection(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.ConfigureConnection(ctx, "***1234"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionConnection(ctx, "resume"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, `UPDATE connections SET state='MONITORING'`); err != nil {
		t.Fatal(err)
	}
	poll, err := store.StartPoll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionConnection(ctx, "pause"); err != nil {
		t.Fatal(err)
	}
	poll.Status = "AUTH_REQUIRED"
	if err := store.FinishPoll(ctx, poll); err == nil {
		t.Fatal("stale poll changed a newer connection")
	}
}
