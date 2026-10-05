package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
)

func TestPollRowsMatchedMigrationAndReadRoundtrip(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "gateway.db")
	legacy, err := openInternal(ctx, path, OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := legacy.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, checksum TEXT NOT NULL, applied_at TEXT NOT NULL)`); err != nil {
			return err
		}
		for _, migration := range migrations {
			if migration.version >= 14 {
				break
			}
			if _, err := tx.ExecContext(ctx, migration.sql); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations VALUES(?,?,?)`, migration.version, migration.checksum, "2026-10-04T00:00:00Z"); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO connections(id,state,generation,created_at,updated_at) VALUES('conn','MONITORING',1,'2026-10-04T00:00:00Z','2026-10-04T00:00:00Z'); INSERT INTO poll_runs(id,connection_id,generation,status,classifier,http_status,pages,rows_seen,started_at,finished_at) VALUES('legacy','conn',1,'SUCCEEDED','HISTORY_PAGE',200,1,180,'2026-10-04T00:00:00Z','2026-10-04T00:00:01Z')`)
		return err
	}); err != nil {
		legacy.Close()
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	last, err := store.LastSuccessfulPoll(ctx, "conn")
	if err != nil || last.RowsMatched != nil || last.RowsSeen != 180 {
		t.Fatalf("legacy rows must remain unknown: %+v %v", last, err)
	}
	want := map[string]*int{"legacy": nil}
	for i, count := range []*int{nil, new(int), func() *int { n := 22; return &n }()} {
		poll, err := store.StartPoll(ctx)
		if err != nil {
			t.Fatal(err)
		}
		poll.Status, poll.Classifier, poll.HTTPStatus = "SUCCEEDED", "HISTORY_PAGE", 200
		poll.RowsSeen, poll.RowsMatched = 180, count
		if err := store.FinishPoll(ctx, poll); err != nil {
			t.Fatal(err)
		}
		// Deterministic boundaries exercise tied timestamp ordering and next cursors.
		stamp := "2026-10-05T00:00:00Z"
		if _, err := store.DB().ExecContext(ctx, `UPDATE poll_runs SET started_at=?,finished_at=? WHERE id=?`, stamp, stamp, poll.ID); err != nil {
			t.Fatal(err)
		}
		want[poll.ID] = count
		var persisted sql.NullInt64
		if err := store.DB().QueryRowContext(ctx, `SELECT rows_matched FROM poll_runs WHERE id=?`, poll.ID).Scan(&persisted); err != nil || persisted.Valid != (count != nil) || count != nil && persisted.Int64 != int64(*count) {
			t.Fatalf("SQL NULL/zero/value case %d: %+v %v", i, persisted, err)
		}
	}
	check := func(poll PollRun) {
		t.Helper()
		count, exists := want[poll.ID]
		if !exists || poll.RowsSeen != 180 || (poll.RowsMatched == nil) != (count == nil) || count != nil && *poll.RowsMatched != *count {
			t.Fatalf("wrong row metric roundtrip: %+v expected=%v", poll, count)
		}
	}
	seen := map[string]bool{}
	cursor := ""
	for {
		page, err := store.ListPollRunsPage(ctx, 1, cursor)
		if err != nil {
			t.Fatal(err)
		}
		for _, poll := range page.Items {
			check(poll)
			if seen[poll.ID] {
				t.Fatalf("duplicate pagination row %s", poll.ID)
			}
			seen[poll.ID] = true
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if len(seen) != len(want) {
		t.Fatalf("pagination lost rows: %v", seen)
	}
	polls, err := store.ListPollRuns(ctx, 10)
	if err != nil || len(polls) != len(want) {
		t.Fatalf("list roundtrip: %+v %v", polls, err)
	}
	for _, poll := range polls {
		check(poll)
		// Choose each row uniquely as the latest successful poll.
		if _, err := store.DB().ExecContext(ctx, `UPDATE poll_runs SET finished_at='2026-10-06T00:00:00Z' WHERE id=?`, poll.ID); err != nil {
			t.Fatal(err)
		}
		last, err := store.LastSuccessfulPoll(ctx, "conn")
		if err != nil || last.ID != poll.ID {
			t.Fatalf("last successful read: %+v %v", last, err)
		}
		check(last)
		if _, err := store.DB().ExecContext(ctx, `UPDATE poll_runs SET finished_at='2026-10-04T00:00:01Z' WHERE id=?`, poll.ID); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPollRowsMatchedJSONAndJournalNullable(t *testing.T) {
	for _, count := range []*int{nil, new(int), func() *int { n := 22; return &n }()} {
		poll := PollRun{RowsSeen: 180, RowsMatched: count}
		wire, err := json.Marshal(poll)
		if err != nil {
			t.Fatal(err)
		}
		journal, err := PollCompletedPayload(poll, 1)
		if err != nil {
			t.Fatal(err)
		}
		for _, payload := range [][]byte{wire, journal} {
			var decoded map[string]any
			if err := json.Unmarshal(payload, &decoded); err != nil {
				t.Fatal(err)
			}
			value, exists := decoded["rowsMatched"]
			if exists != (count != nil) || count != nil && value != float64(*count) || decoded["rowsSeen"] != float64(180) {
				t.Fatalf("nullable count serialization: %s", payload)
			}
		}
	}
}
